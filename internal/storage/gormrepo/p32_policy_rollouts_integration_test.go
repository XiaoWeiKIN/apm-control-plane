//go:build integration

package gormrepo

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/XiaoWeiKIN/apm-control-plane/internal/appconfig"
	"github.com/XiaoWeiKIN/apm-control-plane/internal/auth"
	"github.com/XiaoWeiKIN/apm-control-plane/internal/config"
	"github.com/XiaoWeiKIN/apm-control-plane/internal/policy"
	"github.com/open-telemetry/opamp-go/protobufs"
	"gorm.io/gorm"
)

func TestPolicySelectedRolloutAndResumePostgresAndMySQL(t *testing.T) {
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
			store, err := Open(ctx, appconfig.Database{
				Driver: tt.driver, DSN: dsn, MaxOpen: 10, MaxIdle: 2, MaxLifetime: time.Minute,
			})
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()

			const workspace = "p32-policy-rollout-contract"
			principal := auth.Principal{WorkspaceID: workspace, CredentialID: "java-owner"}
			uids := []string{newUID(t), newUID(t), newUID(t), newUID(t), newUID(t)}
			for i, uid := range uids {
				service, environment := "checkout", "prod"
				if i == 3 {
					environment = "dev"
				}
				if i == 4 {
					service = "billing"
				}
				if _, err := store.Observe(ctx, principal, uid, Observation{
					SeenAt: time.Now().UTC(), Transport: "http",
					Capabilities: uint64(protobufs.AgentCapabilities_AgentCapabilities_AcceptsRemoteConfig),
					Description:  &Description{Kind: config.JavaAgentKind, Service: service, Environment: environment},
				}); err != nil {
					t.Fatal(err)
				}
			}
			_, selected, err := store.CreatePolicy(ctx, workspace, "admin", "checkout-head", 0.2)
			if err != nil {
				t.Fatal(err)
			}
			binding, err := store.CreatePolicyBinding(ctx, workspace, "admin", BindingInput{
				PolicyRevisionID: selected.PolicyRevisionID, ScopeType: policy.ScopeService,
				Selector: policy.Selector{ServiceName: "checkout"}, Priority: 1,
			})
			if err != nil {
				t.Fatal(err)
			}
			_, override, err := store.CreatePolicy(ctx, workspace, "admin", "instance-override", 0.8)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.CreatePolicyBinding(ctx, workspace, "admin", BindingInput{
				PolicyRevisionID: override.PolicyRevisionID, ScopeType: policy.ScopeInstance,
				Selector: policy.Selector{InstanceUID: uids[2]}, Priority: 1,
			}); err != nil {
				t.Fatal(err)
			}

			filter := PolicyRolloutFilter{Environment: "prod", ServiceName: "checkout"}
			if _, err := store.PublishPolicyRollout(ctx, "other-workspace", "admin",
				selected.PolicyRevisionID, filter, 1, ""); !errors.Is(err, policy.ErrNotFound) {
				t.Fatalf("cross-workspace policy release accepted: %v", err)
			}
			if _, err := store.PublishPolicyRollout(ctx, workspace, "admin",
				"00000000-0000-4000-8000-000000000000", filter, 1, ""); !errors.Is(err, policy.ErrNotFound) {
				t.Fatalf("unknown PolicyRevision accepted: %v", err)
			}
			if _, err := store.PublishPolicyRollout(ctx, workspace, "admin",
				selected.PolicyRevisionID, filter, 3, ""); !errors.Is(err, ErrInvalidRollout) {
				t.Fatalf("batch_size exceeding matched agents accepted: %v", err)
			}
			// 审计失败必须连同外层 ConfigRevision 和冻结的 Release/Targets 一并回滚。
			const callback = "p32:reject-rollout-audit"
			auditErr := errors.New("injected P3.2 audit failure")
			if err := store.db.Callback().Create().Before("gorm:create").Register(callback, func(tx *gorm.DB) {
				if tx.Statement != nil && tx.Statement.Schema != nil && tx.Statement.Schema.Table == "config_events" {
					tx.AddError(auditErr)
				}
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := store.PublishPolicyRollout(ctx, workspace, "admin", selected.PolicyRevisionID,
				filter, 1, "audit-"+uids[0][:16]); !errors.Is(err, auditErr) {
				t.Fatalf("audit failure not propagated: %v", err)
			}
			if err := store.db.Callback().Create().Remove(callback); err != nil {
				t.Fatal(err)
			}
			for _, table := range []string{"config_revisions", "config_releases", "release_targets", "agent_assignments"} {
				var count int64
				if err := store.db.Table(table).Where("workspace_id = ?", workspace).Count(&count).Error; err != nil || count != 0 {
					t.Fatalf("%s leaked after failed Policy rollout: rows=%d err=%v", table, count, err)
				}
			}
			key := "p32-" + uids[0][:16]
			var bindingReads atomic.Int32
			const queryCallback = "p32:count-policy-bindings"
			if err := store.db.Callback().Query().Before("gorm:query").Register(queryCallback, func(tx *gorm.DB) {
				if tx.Statement != nil && tx.Statement.Table == "policy_bindings" {
					bindingReads.Add(1)
				}
			}); err != nil {
				t.Fatal(err)
			}
			created, err := store.PublishPolicyRollout(ctx, workspace, "admin",
				selected.PolicyRevisionID, filter, 1, key)
			if removeErr := store.db.Callback().Query().Remove(queryCallback); removeErr != nil {
				t.Fatal(removeErr)
			}
			if bindingReads.Load() != 1 {
				t.Fatalf("expected one Binding snapshot for all Agents, got %d", bindingReads.Load())
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(created.Targets) != 2 || len(created.Activated) != 1 || created.Status != "RUNNING" {
				t.Fatalf("wrong frozen target set: %+v", created)
			}
			for _, target := range created.Targets {
				if target.InstanceUID != uids[0] && target.InstanceUID != uids[1] {
					t.Fatalf("selector or higher-priority override ignored: %+v", target)
				}
			}
			var release ConfigReleaseRecord
			if err := store.db.Where("workspace_id = ? AND release_id = ?", workspace, created.ReleaseID).
				Take(&release).Error; err != nil {
				t.Fatal(err)
			}
			if release.PolicyRevisionID == nil || *release.PolicyRevisionID != selected.PolicyRevisionID {
				t.Fatalf("release missing Policy provenance: %+v", release)
			}
			var selector struct {
				PolicyRevisionID string   `json:"policy_revision_id"`
				InstanceUIDs     []string `json:"instance_uids"`
			}
			if err := json.Unmarshal([]byte(release.TargetSelectorJSON), &selector); err != nil ||
				selector.PolicyRevisionID != selected.PolicyRevisionID || len(selector.InstanceUIDs) != 2 {
				t.Fatalf("unfrozen selection evidence: %+v %v", selector, err)
			}
			cfg, err := store.GetConfigRevision(ctx, workspace, created.Targets[0].ConfigRevisionID)
			if err != nil || cfg.SourcePolicyRevisionID == nil ||
				*cfg.SourcePolicyRevisionID != selected.PolicyRevisionID {
				t.Fatalf("missing immutable ConfigRevision provenance: %+v %v", cfg, err)
			}
			if _, err := store.GetAgentAssignment(ctx, workspace, created.Targets[1].InstanceUID); !errors.Is(err, ErrNotFound) {
				t.Fatalf("queued target received Desired: %v", err)
			}
			again, err := store.PublishPolicyRollout(ctx, workspace, "admin",
				selected.PolicyRevisionID, filter, 1, key)
			if err != nil || again.ReleaseID != created.ReleaseID || len(again.Activated) != 0 {
				t.Fatalf("idempotent policy rollout changed release: %+v %v", again, err)
			}
			if _, err := store.PublishPolicyRollout(ctx, workspace, "admin",
				selected.PolicyRevisionID, filter, 2, key); !errors.Is(err, ErrReleaseConflict) {
				t.Fatalf("same key with different batch size accepted: %v", err)
			}
			// Binding 更新不更改已冻结目标，也不能使相同 Key 重试产生新 Release。
			if _, err := store.UpdatePolicyBinding(ctx, workspace, "admin", binding.BindingID, BindingInput{
				PolicyRevisionID: binding.PolicyRevisionID, ScopeType: binding.ScopeType,
				Selector: policy.Selector{ServiceName: "checkout"}, Priority: 1,
				Enabled: false, RowVersion: binding.RowVersion,
			}); err != nil {
				t.Fatal(err)
			}
			replay, err := store.PublishPolicyRollout(ctx, workspace, "admin",
				selected.PolicyRevisionID, filter, 1, key)
			if err != nil || replay.ReleaseID != created.ReleaseID || len(replay.Activated) != 0 {
				t.Fatalf("Binding change invalidated idempotent snapshot: %+v %v", replay, err)
			}

			// 1. 目标失败暂停；在匹配的新 APPLIED 回执前显式恢复必须失败。
			firstUID := created.Activated[0]
			offered, _, err := store.ReconcileAgentConfig(ctx, workspace, firstUID, nil, true)
			if err != nil || offered == nil {
				t.Fatalf("first target offer: %+v %v", offered, err)
			}
			if _, _, err := store.ReconcileAgentConfig(ctx, workspace, firstUID,
				&ConfigStatus{Hash: offered.ConfigHash, Status: "FAILED"}, false); err != nil {
				t.Fatal(err)
			}
			paused, err := store.GetRollout(ctx, workspace, created.ReleaseID)
			if err != nil || paused.Status != "PAUSED" {
				t.Fatalf("failed target did not pause: %+v %v", paused, err)
			}
			if _, err := store.ResumeRollout(ctx, workspace, created.ReleaseID, "admin", paused.RowVersion); !errors.Is(err, ErrRolloutBlocked) {
				t.Fatalf("resumed failed target before ACK recovery: %v", err)
			}
			if _, err := store.ResumeRollout(ctx, workspace, created.ReleaseID, "admin", paused.RowVersion-1); !errors.Is(err, ErrAssignmentConflict) {
				t.Fatalf("stale resume version accepted: %v", err)
			}

			// 2. 晚到 APPLIED 只能更新 Target，不会自动解除 PAUSED。
			if _, _, err := store.ReconcileAgentConfig(ctx, workspace, firstUID,
				&ConfigStatus{Hash: offered.ConfigHash, Status: "APPLIED"}, false); err != nil {
				t.Fatal(err)
			}
			still, err := store.GetRollout(ctx, workspace, created.ReleaseID)
			if err != nil || still.Status != "PAUSED" || still.Targets[0].Status != "APPLIED" {
				t.Fatalf("sticky pause bypassed: %+v %v", still, err)
			}
			resumed, err := store.ResumeRollout(ctx, workspace, created.ReleaseID, "admin", still.RowVersion)
			if err != nil || resumed.Status != "RUNNING" ||
				resumed.RowVersion != still.RowVersion+1 || len(resumed.Activated) != 0 {
				t.Fatalf("failed guarded resume: %+v %v", resumed, err)
			}
			if _, err := store.ResumeRollout(ctx, workspace, created.ReleaseID, "admin", still.RowVersion); !errors.Is(err, ErrAssignmentConflict) {
				t.Fatalf("duplicate resume version accepted: %v", err)
			}
			var after ConfigReleaseRecord
			if err := store.db.Where("workspace_id = ? AND release_id = ?", workspace, created.ReleaseID).
				Take(&after).Error; err != nil || after.FinishedAt != nil {
				t.Fatalf("resumed release retains finished_at: %+v %v", after, err)
			}

			// 3. Resume 无网络副作用；显式 Advance 激活第二批并以 ACK 完成。
			next, err := store.AdvanceRollout(ctx, workspace, created.ReleaseID, "admin")
			if err != nil || len(next.Activated) != 1 {
				t.Fatalf("advance after resume: %+v %v", next, err)
			}
			secondUID := next.Activated[0]
			offered, _, err = store.ReconcileAgentConfig(ctx, workspace, secondUID, nil, true)
			if err != nil || offered == nil || offered.ConfigHash != cfg.ConfigHash {
				t.Fatalf("second offer: %+v %v", offered, err)
			}
			if _, _, err := store.ReconcileAgentConfig(ctx, workspace, secondUID,
				&ConfigStatus{Hash: offered.ConfigHash, Status: "APPLIED"}, false); err != nil {
				t.Fatal(err)
			}
			done, err := store.GetRollout(ctx, workspace, created.ReleaseID)
			if err != nil || done.Status != "COMPLETED" {
				t.Fatalf("rollout not completed: %+v %v", done, err)
			}
			if _, err := store.ResumeRollout(ctx, "other-workspace", created.ReleaseID, "admin", done.RowVersion); !errors.Is(err, ErrRolloutNotFound) {
				t.Fatalf("cross-workspace resume leaked: %v", err)
			}
			if _, err := store.ResumeRollout(ctx, workspace, created.ReleaseID, "admin", done.RowVersion); !errors.Is(err, ErrRolloutBlocked) {
				t.Fatalf("completed release resumed: %v", err)
			}
			// 候选 Agent 超出上限不得静默截断后发布。
			for i := 0; i < 12; i++ {
				extra := newUID(t)
				if _, err := store.Observe(ctx, principal, extra, Observation{
					SeenAt: time.Now().UTC(), Transport: "http",
					Capabilities: uint64(protobufs.AgentCapabilities_AgentCapabilities_AcceptsRemoteConfig),
					Description:  &Description{Kind: config.JavaAgentKind, Service: "unrelated", Environment: "dev"},
				}); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := store.PublishPolicyRollout(ctx, workspace, "admin",
				selected.PolicyRevisionID, PolicyRolloutFilter{}, 1, ""); !errors.Is(err, ErrInvalidRollout) {
				t.Fatalf("unbounded candidate scan silently truncated: %v", err)
			}
		})
	}
}

func TestConcurrentPolicyRolloutIdempotencyPostgresAndMySQL(t *testing.T) {
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
			store, err := Open(ctx, appconfig.Database{
				Driver: tt.driver, DSN: dsn, MaxOpen: 10, MaxIdle: 2, MaxLifetime: time.Minute,
			})
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			uids := []string{newUID(t), newUID(t)}
			workspace := "p32-idempotency-" + uids[0][:12]
			principal := auth.Principal{WorkspaceID: workspace, CredentialID: "java-owner"}
			for _, uid := range uids {
				if _, err := store.Observe(ctx, principal, uid, Observation{
					SeenAt: time.Now().UTC(), Transport: "http",
					Capabilities: uint64(protobufs.AgentCapabilities_AgentCapabilities_AcceptsRemoteConfig),
					Description:  &Description{Kind: config.JavaAgentKind, Service: "checkout", Environment: "prod"},
				}); err != nil {
					t.Fatal(err)
				}
			}
			_, revision, err := store.CreatePolicy(ctx, workspace, "admin", "shared-rollout", 0.2)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.CreatePolicyBinding(ctx, workspace, "admin", BindingInput{
				PolicyRevisionID: revision.PolicyRevisionID, ScopeType: policy.ScopeService,
				Selector: policy.Selector{ServiceName: "checkout"}, Priority: 1,
			}); err != nil {
				t.Fatal(err)
			}
			type outcome struct {
				view RolloutView
				err  error
			}
			race := func(key string, firstSize, secondSize int) [2]outcome {
				start := make(chan struct{})
				results := make(chan outcome, 2)
				for _, size := range []int{firstSize, secondSize} {
					go func(batchSize int) {
						<-start
						view, err := store.PublishPolicyRollout(ctx, workspace, "admin",
							revision.PolicyRevisionID, PolicyRolloutFilter{ServiceName: "checkout"}, batchSize, key)
						results <- outcome{view, err}
					}(size)
				}
				close(start)
				return [2]outcome{<-results, <-results}
			}
			same := race("same-key-"+uids[0][:12], 1, 1)
			if same[0].err != nil || same[1].err != nil || same[0].view.ReleaseID != same[1].view.ReleaseID {
				t.Fatalf("same-key race did not replay original release: %+v", same)
			}
			if len(same[0].view.Activated)+len(same[1].view.Activated) != 1 {
				t.Fatalf("same-key race activated agents more than once: %+v", same)
			}
			different := race("different-body-"+uids[0][:12], 1, 2)
			success, conflicts := 0, 0
			for _, result := range different {
				switch {
				case result.err == nil && result.view.ReleaseID != "":
					success++
				case errors.Is(result.err, ErrReleaseConflict):
					conflicts++
				default:
					t.Fatalf("unexpected different-body race result: %+v", different)
				}
			}
			if success != 1 || conflicts != 1 {
				t.Fatalf("different-body race must create one release and return one conflict: %+v", different)
			}
			for _, table := range []string{"config_revisions", "config_releases"} {
				var count int64
				if err := store.db.Table(table).Where("workspace_id = ?", workspace).Count(&count).Error; err != nil || count != 2 {
					t.Fatalf("%s should contain only the two successful publications, count=%d err=%v", table, count, err)
				}
			}
		})
	}
}
