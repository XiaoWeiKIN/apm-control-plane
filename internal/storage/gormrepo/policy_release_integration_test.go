//go:build integration

package gormrepo

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/XiaoWeiKIN/apm-control-plane/internal/appconfig"
	"github.com/XiaoWeiKIN/apm-control-plane/internal/auth"
	"github.com/XiaoWeiKIN/apm-control-plane/internal/config"
	"github.com/XiaoWeiKIN/apm-control-plane/internal/policy"
	"github.com/open-telemetry/opamp-go/protobufs"
	"gorm.io/gorm"
)

func TestPolicyReleaseAtomicPostgresAndMySQL(t *testing.T) {
	for _, tt := range []struct{ driver, env string }{
		{"postgres", "APM_TEST_POSTGRES_DSN"},
		{"mysql", "APM_TEST_MYSQL_DSN"},
	} {
		t.Run(tt.driver, func(t *testing.T) {
			dsn := os.Getenv(tt.env)
			if dsn == "" {
				t.Skip(tt.env + " not configured")
			}
			ctx := context.Background()
			store, err := Open(ctx, appconfig.Database{Driver: tt.driver, DSN: dsn,
				MaxOpen: 10, MaxIdle: 2, MaxLifetime: time.Minute})
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()

			workspace := "p25-policy-release"
			uid := newUID(t)
			owner := auth.Principal{WorkspaceID: workspace, CredentialID: "java-agent"}
			if _, err := store.Observe(ctx, owner, uid, Observation{
				SeenAt: time.Now().UTC(), Transport: "http",
				Capabilities: uint64(protobufs.AgentCapabilities_AgentCapabilities_AcceptsRemoteConfig),
				Description:  &Description{Kind: config.JavaAgentKind, Service: "checkout", Environment: "prod"},
			}); err != nil {
				t.Fatal(err)
			}
			createdPolicy, revision, err := store.CreatePolicy(ctx, workspace, "admin", "checkout-sampling", 0.2)
			if err != nil {
				t.Fatal(err)
			}
			selector := policy.Selector{ServiceName: "checkout", Environment: "prod"}
			binding, err := store.CreatePolicyBinding(ctx, workspace, "admin", BindingInput{
				PolicyRevisionID: revision.PolicyRevisionID, ScopeType: policy.ScopeService,
				Selector: selector, Priority: 1,
			})
			if err != nil {
				t.Fatal(err)
			}

			// 1. 未命中、跨 Workspace 和过期预览均不得创建 ConfigRevision。
			if _, err := store.PublishAgentPolicy(ctx, "other-workspace", uid,
				revision.PolicyRevisionID, "admin", ""); !errors.Is(err, ErrNotFound) {
				t.Fatalf("cross-workspace release accepted: %v", err)
			}
			if _, err := store.PublishAgentPolicy(ctx, workspace, uid,
				"00000000-0000-4000-8000-000000000000", "admin", ""); !errors.Is(err, policy.ErrConflict) {
				t.Fatalf("mismatched expected policy revision: %v", err)
			}
			var count int64
			if err := store.db.Model(&ConfigRevisionRecord{}).Where("workspace_id = ?", workspace).Count(&count).Error; err != nil || count != 0 {
				t.Fatalf("rejected request created ConfigRevision: %d %v", count, err)
			}

			// 缺少 RemoteConfig Capability 时提前失败，不插入孤立 ConfigRevision。
			unsupportedUID := newUID(t)
			if _, err := store.Observe(ctx, owner, unsupportedUID, Observation{
				SeenAt: time.Now().UTC(), Transport: "http",
				Capabilities: 0,
				Description:  &Description{Kind: config.JavaAgentKind, Service: "checkout", Environment: "prod"},
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := store.PublishAgentPolicy(ctx, workspace, unsupportedUID,
				revision.PolicyRevisionID, "admin", ""); !errors.Is(err, ErrUnsupportedReleaseTarget) {
				t.Fatalf("Agent without RemoteConfig capability accepted: %v", err)
			}
			if err := store.db.Model(&ConfigRevisionRecord{}).Where("workspace_id = ?", workspace).Count(&count).Error; err != nil || count != 0 {
				t.Fatalf("unsupported Agent left ConfigRevision: %d %v", count, err)
			}

			// 2. Binding 在发布读取快照后更新时，不能发布过期的 Revision（MySQL RR）。
			nextRevision, err := store.CreatePolicyRevision(ctx, workspace, "admin", createdPolicy.PolicyID, 0.4)
			if err != nil {
				t.Fatal(err)
			}
			entered, resume := make(chan struct{}), make(chan struct{})
			var gate sync.Once
			const resolveGate = "test:pause-policy-release-resolution"
			if err := store.db.Callback().Query().Before("gorm:query").Register(resolveGate, func(tx *gorm.DB) {
				_, locking := tx.Statement.Clauses["FOR"]
				if tx.Statement.Table == "policy_bindings" && locking {
					gate.Do(func() {
						close(entered)
						<-resume
					})
				}
			}); err != nil {
				t.Fatal(err)
			}
			publishErrs := make(chan error, 1)
			go func() {
				_, err := store.PublishAgentPolicy(ctx, workspace, uid, revision.PolicyRevisionID,
					"admin", "binding-race-"+uid[:16])
				publishErrs <- err
			}()
			select {
			case <-entered:
			case <-time.After(10 * time.Second):
				close(resume)
				t.Fatal("policy release did not reach locked binding read")
			}
			updated, err := store.UpdatePolicyBinding(ctx, workspace, "admin", binding.BindingID, BindingInput{
				PolicyRevisionID: nextRevision.PolicyRevisionID, ScopeType: policy.ScopeService,
				Selector: selector, Priority: 1, Enabled: true, RowVersion: binding.RowVersion,
			})
			close(resume)
			if err != nil {
				t.Fatal(err)
			}
			if err := <-publishErrs; !errors.Is(err, policy.ErrConflict) {
				t.Fatalf("stale binding was published: %v", err)
			}
			if err := store.db.Callback().Query().Remove(resolveGate); err != nil {
				t.Fatal(err)
			}
			if _, err := store.UpdatePolicyBinding(ctx, workspace, "admin", binding.BindingID, BindingInput{
				PolicyRevisionID: revision.PolicyRevisionID, ScopeType: policy.ScopeService,
				Selector: selector, Priority: 1, Enabled: true, RowVersion: updated.RowVersion,
			}); err != nil {
				t.Fatal(err)
			}
			if err := store.db.Model(&ConfigRevisionRecord{}).Where("workspace_id = ?", workspace).Count(&count).Error; err != nil || count != 0 {
				t.Fatalf("stale binding release created ConfigRevision: %d %v", count, err)
			}

			// 3. 事务提交后，ConfigRevision/Release/Target/Assignment/Event 必须保持相同来源。
			key := "p25-" + uid[:16]
			first, err := store.PublishAgentPolicy(ctx, workspace, uid, revision.PolicyRevisionID, "admin", key)
			if err != nil {
				t.Fatal(err)
			}
			if first.Status != "PENDING" || first.DesiredGeneration != 1 {
				t.Fatalf("unexpected release: %+v", first)
			}
			cfg, err := store.GetConfigRevision(ctx, workspace, first.ConfigRevisionID)
			if err != nil || cfg.SourcePolicyRevisionID == nil ||
				*cfg.SourcePolicyRevisionID != revision.PolicyRevisionID ||
				cfg.ConfigHash == "" || len(cfg.ConfigMapBytes) == 0 {
				t.Fatalf("missing immutable compiled provenance: %+v %v", cfg, err)
			}
			var release ConfigReleaseRecord
			if err := store.db.Where("workspace_id = ? AND release_id = ?", workspace, first.ReleaseID).
				Take(&release).Error; err != nil || release.PolicyRevisionID == nil ||
				*release.PolicyRevisionID != revision.PolicyRevisionID {
				t.Fatalf("release lost Policy Revision provenance: %+v %v", release, err)
			}
			assignment, err := store.GetAgentAssignment(ctx, workspace, uid)
			if err != nil || assignment.DesiredHash != cfg.ConfigHash || assignment.DesiredGeneration != 1 {
				t.Fatalf("atomic Desired wrong: %+v %v", assignment, err)
			}
			// P2.3 continues to send the exact immutable ConfigMap and converge only on matching ACK.
			offered, assigned, err := store.ReconcileAgentConfig(ctx, workspace, uid, nil, true)
			if err != nil || !assigned || offered == nil || offered.ConfigHash != cfg.ConfigHash {
				t.Fatalf("OpAMP durable offer not available: %+v %t %v", offered, assigned, err)
			}
			if _, _, err := store.ReconcileAgentConfig(ctx, workspace, uid,
				&ConfigStatus{Hash: cfg.ConfigHash, Status: "APPLIED"}, false); err != nil {
				t.Fatal(err)
			}
			applied, err := store.GetAgentAssignment(ctx, workspace, uid)
			if err != nil || applied.State != "APPLIED" {
				t.Fatalf("hash ACK did not converge: %+v %v", applied, err)
			}

			// 3. 同一 Key 重试返回原 Release，不产生额外配置或推进 Generation。
			again, err := store.PublishAgentPolicy(ctx, workspace, uid, revision.PolicyRevisionID, "admin", key)
			if err != nil || again.ReleaseID != first.ReleaseID || again.ConfigRevisionID != first.ConfigRevisionID {
				t.Fatalf("idempotency replay: %+v %v", again, err)
			}
			if err := store.db.Model(&ConfigRevisionRecord{}).Where("workspace_id = ?", workspace).Count(&count).Error; err != nil || count != 1 {
				t.Fatalf("idempotent publish duplicated ConfigRevision: %d %v", count, err)
			}
			if _, err := store.PublishAgentPolicy(ctx, workspace, uid, "00000000-0000-4000-8000-000000000000",
				"admin", key); !errors.Is(err, ErrReleaseConflict) {
				t.Fatalf("same key different request should conflict: %v", err)
			}

			// 4. 事务最末尾审计失败时，新增 ConfigRevision 也必须回滚。
			const callback = "test:reject-p25-policy-audit"
			auditErr := errors.New("injected audit failure")
			if err := store.db.Callback().Create().Before("gorm:create").Register(callback, func(tx *gorm.DB) {
				if tx.Statement != nil && tx.Statement.Schema != nil && tx.Statement.Schema.Table == "config_events" {
					tx.AddError(auditErr)
				}
			}); err != nil {
				t.Fatal(err)
			}
			_, err = store.PublishAgentPolicy(ctx, workspace, uid, revision.PolicyRevisionID, "admin", "failure-"+uid[:16])
			if !errors.Is(err, auditErr) {
				t.Fatalf("audit failure not propagated: %v", err)
			}
			if err := store.db.Callback().Create().Remove(callback); err != nil {
				t.Fatal(err)
			}
			if err := store.db.Model(&ConfigRevisionRecord{}).Where("workspace_id = ?", workspace).Count(&count).Error; err != nil || count != 1 {
				t.Fatalf("rollback left orphan ConfigRevision: %d %v", count, err)
			}
			still, err := store.GetAgentAssignment(ctx, workspace, uid)
			if err != nil || still.DesiredGeneration != 1 || still.State != "APPLIED" {
				t.Fatalf("rollback damaged active Assignment: %+v %v", still, err)
			}

			// 5. 同一 Agent 的并发发布按 Generation 串行化，两个配置均能溯源。
			var wg sync.WaitGroup
			gens := make(chan int64, 2)
			errs := make(chan error, 2)
			for i := 0; i < 2; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					r, err := store.PublishAgentPolicy(ctx, workspace, uid, revision.PolicyRevisionID, "admin",
						string(rune('a'+i))+"-"+uid[:16])
					if err != nil {
						errs <- err
						return
					}
					gens <- r.DesiredGeneration
				}(i)
			}
			wg.Wait()
			close(errs)
			close(gens)
			for err := range errs {
				t.Fatalf("concurrent policy release: %v", err)
			}
			seen := map[int64]bool{}
			for generation := range gens {
				seen[generation] = true
			}
			if !seen[2] || !seen[3] || len(seen) != 2 {
				t.Fatalf("nonserialized policy release: %+v", seen)
			}
		})
	}
}
