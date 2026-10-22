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
	"github.com/open-telemetry/opamp-go/protobufs"
	"gorm.io/gorm"
)

func TestFrozenBatchRolloutPostgresAndMySQL(t *testing.T) {
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
			workspace := "p3-rollout-contract"
			p := auth.Principal{WorkspaceID: workspace, CredentialID: "java-owner"}
			uids := []string{newUID(t), newUID(t), newUID(t), newUID(t)}
			for _, uid := range uids {
				if _, err := store.Observe(ctx, p, uid, Observation{
					SeenAt: time.Now().UTC(), Transport: "http",
					Capabilities: uint64(protobufs.AgentCapabilities_AgentCapabilities_AcceptsRemoteConfig),
					Description:  &Description{Kind: config.JavaAgentKind},
				}); err != nil {
					t.Fatal(err)
				}
			}
			snapshot, err := config.CompileJavaSampling(0.3)
			if err != nil {
				t.Fatal(err)
			}
			revision, err := store.CreateConfigRevision(ctx, workspace, "admin", snapshot)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.PublishRollout(ctx, workspace, "admin", revision.ConfigRevisionID,
				[]string{uids[0], uids[0]}, 1, ""); !errors.Is(err, ErrInvalidRollout) {
				t.Fatalf("duplicate Agent UID accepted: %v", err)
			}
			if _, err := store.PublishRollout(ctx, workspace, "admin", revision.ConfigRevisionID,
				[]string{uids[0], newUID(t)}, 1, ""); !errors.Is(err, ErrNotFound) {
				t.Fatalf("unknown Agent accepted: %v", err)
			}
			if _, err := store.PublishRollout(ctx, "other-workspace", "admin",
				revision.ConfigRevisionID, uids, 2, ""); !errors.Is(err, ErrNotFound) {
				t.Fatalf("cross-workspace targets leaked: %v", err)
			}

			// 1. 原子冻结四个目标，只激活首批；调用顺序不影响冻结排序。
			batch, err := store.PublishRollout(ctx, workspace, "admin", revision.ConfigRevisionID,
				[]string{uids[3], uids[1], uids[0], uids[2]}, 2, "batch-"+uids[0][:12])
			if err != nil {
				t.Fatal(err)
			}
			if batch.Status != "RUNNING" || len(batch.Targets) != 4 || len(batch.Activated) != 2 {
				t.Fatalf("unexpected frozen set: %+v", batch)
			}
			for i, target := range batch.Targets {
				wantBatch, wantStatus := i/2, "QUEUED"
				if i < 2 {
					wantStatus = "PENDING"
				}
				if target.BatchNo != wantBatch || target.Status != wantStatus ||
					target.ConfigRevisionID != revision.ConfigRevisionID || target.DesiredGeneration != 1 {
					t.Fatalf("frozen target[%d]: %+v", i, target)
				}
				_, err := store.GetAgentAssignment(ctx, workspace, target.InstanceUID)
				if i < 2 && err != nil || i >= 2 && !errors.Is(err, ErrNotFound) {
					t.Fatalf("unactivated target Assignment leak: index=%d err=%v", i, err)
				}
			}
			if _, err := store.AdvanceRollout(ctx, workspace, batch.ReleaseID, "admin"); !errors.Is(err, ErrRolloutNotReady) {
				t.Fatalf("advanced before first batch ACK: %v", err)
			}
			again, err := store.PublishRollout(ctx, workspace, "admin", revision.ConfigRevisionID,
				[]string{uids[0], uids[1], uids[2], uids[3]}, 2, "batch-"+uids[0][:12])
			if err != nil || again.ReleaseID != batch.ReleaseID || len(again.Activated) != 0 {
				t.Fatalf("idempotency reactivated targets: %+v %v", again, err)
			}
			if _, err := store.PublishRollout(ctx, workspace, "admin", revision.ConfigRevisionID,
				uids, 1, "batch-"+uids[0][:12]); !errors.Is(err, ErrReleaseConflict) {
				t.Fatalf("same key different batch size accepted: %v", err)
			}

			// 2. ACK 通过 P2.3 状态机归因；此前不得触发下一批 Desired。
			ack := func(uid string) {
				t.Helper()
				offered, ok, err := store.ReconcileAgentConfig(ctx, workspace, uid, nil, true)
				if err != nil || !ok || offered == nil || offered.ConfigHash != revision.ConfigHash {
					t.Fatalf("offered target %s: %+v %t %v", uid, offered, ok, err)
				}
				if _, _, err := store.ReconcileAgentConfig(ctx, workspace, uid,
					&ConfigStatus{Hash: offered.ConfigHash, Status: "APPLIED"}, false); err != nil {
					t.Fatal(err)
				}
			}
			for _, target := range batch.Targets[:2] {
				ack(target.InstanceUID)
			}

			// 下一批审计失败时，所有 Queued Target 和新 Assignment 必须回滚。
			const callback = "p3:reject-advance-audit"
			auditErr := errors.New("injected advance audit failure")
			if err := store.db.Callback().Create().Before("gorm:create").Register(callback, func(tx *gorm.DB) {
				if tx.Statement != nil && tx.Statement.Schema != nil && tx.Statement.Schema.Table == "config_events" {
					tx.AddError(auditErr)
				}
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := store.AdvanceRollout(ctx, workspace, batch.ReleaseID, "admin"); !errors.Is(err, auditErr) {
				t.Fatalf("advance audit failure not propagated: %v", err)
			}
			if err := store.db.Callback().Create().Remove(callback); err != nil {
				t.Fatal(err)
			}
			unchanged, err := store.GetRollout(ctx, workspace, batch.ReleaseID)
			if err != nil || unchanged.Targets[2].Status != "QUEUED" || unchanged.RowVersion != 1 {
				t.Fatalf("failed advance mutated frozen state: %+v %v", unchanged, err)
			}
			if _, err := store.GetAgentAssignment(ctx, workspace, batch.Targets[2].InstanceUID); !errors.Is(err, ErrNotFound) {
				t.Fatalf("failed advance left an Assignment: %v", err)
			}
			// 并发推进同一批时，最多一次激活，另一请求应明确 NotReady。
			var wg sync.WaitGroup
			views := make(chan RolloutView, 2)
			errs := make(chan error, 2)
			for i := 0; i < 2; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					view, err := store.AdvanceRollout(ctx, workspace, batch.ReleaseID, "admin")
					views <- view
					errs <- err
				}()
			}
			wg.Wait()
			close(views)
			close(errs)
			success, rejected := 0, 0
			var advanced RolloutView
			for view := range views {
				if len(view.Activated) == 2 {
					advanced = view
					success++
				}
			}
			for err := range errs {
				if err == nil {
					continue
				}
				if !errors.Is(err, ErrRolloutNotReady) {
					t.Fatalf("unexpected concurrent Advance error: %v", err)
				}
				rejected++
			}
			if success != 1 || rejected != 1 || advanced.Status != "RUNNING" {
				t.Fatalf("concurrent Advance not serialized: success=%d rejected=%d view=%+v", success, rejected, advanced)
			}
			for _, target := range advanced.Targets[2:] {
				if target.Status != "PENDING" {
					t.Fatalf("next batch status: %+v", target)
				}
				ack(target.InstanceUID)
			}
			done, err := store.GetRollout(ctx, workspace, batch.ReleaseID)
			if err != nil || done.Status != "COMPLETED" {
				t.Fatalf("rollout not completed by matching ACKs: %+v %v", done, err)
			}
			if _, err := store.GetRollout(ctx, "other-workspace", batch.ReleaseID); !errors.Is(err, ErrRolloutNotFound) {
				t.Fatalf("cross-workspace rollout read: %v", err)
			}
			if result, err := store.AdvanceRollout(ctx, workspace, batch.ReleaseID, "admin"); err != nil || result.Status != "COMPLETED" || len(result.Activated) != 0 {
				t.Fatalf("completed rollout was advanced: %+v %v", result, err)
			}
			// 3. 重开 Store 后目标快照和最终状态仍在数据库内。
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			store, err = Open(ctx, appconfig.Database{Driver: tt.driver, DSN: dsn,
				MaxOpen: 10, MaxIdle: 2, MaxLifetime: time.Minute})
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			recovered, err := store.GetRollout(ctx, workspace, batch.ReleaseID)
			if err != nil || recovered.Status != "COMPLETED" || len(recovered.Targets) != 4 {
				t.Fatalf("reopen lost release snapshot: %+v %v", recovered, err)
			}
		})
	}
}

func TestRolloutStopAndRollbackPostgresAndMySQL(t *testing.T) {
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
				MaxOpen: 8, MaxIdle: 2, MaxLifetime: time.Minute})
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			workspace := "p3-rollback-contract"
			p := auth.Principal{WorkspaceID: workspace, CredentialID: "agent"}
			uids := []string{newUID(t), newUID(t), newUID(t)}
			for _, uid := range uids {
				if _, err := store.Observe(ctx, p, uid, Observation{
					SeenAt: time.Now().UTC(), Transport: "http",
					Capabilities: uint64(protobufs.AgentCapabilities_AgentCapabilities_AcceptsRemoteConfig),
					Description:  &Description{Kind: config.JavaAgentKind},
				}); err != nil {
					t.Fatal(err)
				}
			}
			snapshot, _ := config.CompileJavaSampling(0.4)
			revision, err := store.CreateConfigRevision(ctx, workspace, "admin", snapshot)
			if err != nil {
				t.Fatal(err)
			}
			// 1. 审计事件写入失败必须回滚 Release、全部 Frozen Target 和 Assignment。
			const cb = "p3:reject-audit"
			auditErr := errors.New("injected audit failure")
			if err := store.db.Callback().Create().Before("gorm:create").Register(cb, func(tx *gorm.DB) {
				if tx.Statement != nil && tx.Statement.Schema != nil && tx.Statement.Schema.Table == "config_events" {
					tx.AddError(auditErr)
				}
			}); err != nil {
				t.Fatal(err)
			}
			_, err = store.PublishRollout(ctx, workspace, "admin", revision.ConfigRevisionID, uids, 1, "")
			if !errors.Is(err, auditErr) {
				t.Fatalf("audit failure not propagated: %v", err)
			}
			if err := store.db.Callback().Create().Remove(cb); err != nil {
				t.Fatal(err)
			}
			for _, table := range []string{"config_releases", "release_targets", "agent_assignments"} {
				var count int64
				if err := store.db.Table(table).Where("workspace_id = ?", workspace).Count(&count).Error; err != nil || count != 0 {
					t.Fatalf("%s rollback incomplete: count=%d err=%v", table, count, err)
				}
			}

			// 2. 首批 Agent 报错使发布 PAUSED；后续批次不获得 Desired。
			result, err := store.PublishRollout(ctx, workspace, "admin", revision.ConfigRevisionID, uids, 1, "")
			if err != nil {
				t.Fatal(err)
			}
			first := result.Activated[0]
			offered, _, err := store.ReconcileAgentConfig(ctx, workspace, first, nil, true)
			if err != nil || offered == nil {
				t.Fatalf("offer first batch: %v", err)
			}
			if _, _, err := store.ReconcileAgentConfig(ctx, workspace, first,
				&ConfigStatus{Hash: offered.ConfigHash, Status: "FAILED", Error: "reject"}, false); err != nil {
				t.Fatal(err)
			}
			view, err := store.GetRollout(ctx, workspace, result.ReleaseID)
			if err != nil || view.Status != "PAUSED" {
				t.Fatalf("failure did not pause rollout: %+v %v", view, err)
			}
			if _, err := store.AdvanceRollout(ctx, workspace, result.ReleaseID, "admin"); !errors.Is(err, ErrRolloutBlocked) {
				t.Fatalf("advanced failed rollout: %v", err)
			}
			for _, target := range view.Targets {
				if target.InstanceUID == first {
					continue
				}
				if _, err := store.GetAgentAssignment(ctx, workspace, target.InstanceUID); !errors.Is(err, ErrNotFound) {
					t.Fatalf("queued Agent unexpectedly assigned: %v", err)
				}
			}

			// 3. 冻结后有其他发布改变待激活 Agent 的 Generation，下一批显式 PAUSED。
			later := []string{newUID(t), newUID(t)}
			for _, uid := range later {
				if _, err := store.Observe(ctx, p, uid, Observation{SeenAt: time.Now().UTC(), Transport: "http",
					Capabilities: uint64(protobufs.AgentCapabilities_AgentCapabilities_AcceptsRemoteConfig),
					Description:  &Description{Kind: config.JavaAgentKind}}); err != nil {
					t.Fatal(err)
				}
			}
			other, err := store.PublishRollout(ctx, workspace, "admin", revision.ConfigRevisionID, later, 1, "")
			if err != nil {
				t.Fatal(err)
			}
			first = other.Activated[0]
			offered, _, err = store.ReconcileAgentConfig(ctx, workspace, first, nil, true)
			if err != nil || offered == nil {
				t.Fatal("first batch offer missing")
			}
			if _, _, err := store.ReconcileAgentConfig(ctx, workspace, first,
				&ConfigStatus{Hash: offered.ConfigHash, Status: "APPLIED"}, false); err != nil {
				t.Fatal(err)
			}
			queued := other.Targets[1].InstanceUID
			if _, err := store.PublishAgentConfig(ctx, workspace, queued, revision.ConfigRevisionID, "admin", ""); err != nil {
				t.Fatal(err)
			}
			stopped, err := store.AdvanceRollout(ctx, workspace, other.ReleaseID, "admin")
			if err != nil || stopped.Status != "PAUSED" {
				t.Fatalf("stale queued Generation not paused: %+v %v", stopped, err)
			}
		})
	}
}

func TestSupersededActiveRolloutPausesPostgresAndMySQL(t *testing.T) {
	for _, tt := range []struct{ driver, env string }{
		{"postgres", "APM_TEST_POSTGRES_DSN"},
		{"mysql", "APM_TEST_MYSQL_DSN"},
	} {
		for _, mode := range []string{"direct", "rollout", "fully-activated"} {
			t.Run(tt.driver+"/"+mode, func(t *testing.T) {
				dsn := os.Getenv(tt.env)
				if dsn == "" {
					t.Skip(tt.env + " not configured")
				}
				ctx := context.Background()
				store, err := Open(ctx, appconfig.Database{Driver: tt.driver, DSN: dsn,
					MaxOpen: 8, MaxIdle: 2, MaxLifetime: time.Minute})
				if err != nil {
					t.Fatal(err)
				}
				defer store.Close()

				workspace := "p3-supersede-" + mode
				owner := auth.Principal{WorkspaceID: workspace, CredentialID: "agent"}
				uids := []string{newUID(t), newUID(t), newUID(t)}
				for _, uid := range uids {
					if _, err := store.Observe(ctx, owner, uid, Observation{
						SeenAt: time.Now().UTC(), Transport: "http",
						Capabilities: uint64(protobufs.AgentCapabilities_AgentCapabilities_AcceptsRemoteConfig),
						Description:  &Description{Kind: config.JavaAgentKind},
					}); err != nil {
						t.Fatal(err)
					}
				}
				snapshot, err := config.CompileJavaSampling(0.55)
				if err != nil {
					t.Fatal(err)
				}
				revision, err := store.CreateConfigRevision(ctx, workspace, "admin", snapshot)
				if err != nil {
					t.Fatal(err)
				}
				batchSize := 1
				if mode == "fully-activated" {
					batchSize = 2
				}
				old, err := store.PublishRollout(ctx, workspace, "admin", revision.ConfigRevisionID, uids[:2], batchSize, "")
				if err != nil {
					t.Fatal(err)
				}
				active := old.Activated[0]
				if mode != "rollout" {
					// 覆盖 WAITING_ACK；新 Rollout 模式覆盖 PENDING。
					offered, _, err := store.ReconcileAgentConfig(ctx, workspace, active, nil, true)
					if err != nil || offered == nil {
						t.Fatalf("cannot offer original Desired: %v", err)
					}
					if _, err := store.PublishAgentConfig(ctx, workspace, active, revision.ConfigRevisionID, "admin", ""); err != nil {
						t.Fatal(err)
					}
				} else {
					if _, err := store.PublishRollout(ctx, workspace, "admin", revision.ConfigRevisionID,
						[]string{active, uids[2]}, 2, ""); err != nil {
						t.Fatal(err)
					}
				}
				view, err := store.GetRollout(ctx, workspace, old.ReleaseID)
				if err != nil || view.Status != "PAUSED" {
					t.Fatalf("superseded rollout did not stop: %+v %v", view, err)
				}
				for _, target := range view.Targets {
					want := "SUPERSEDED"
					if target.InstanceUID != active {
						want = "QUEUED"
						if mode == "fully-activated" {
							want = "PENDING"
						}
					}
					if target.Status != want {
						t.Fatalf("unexpected frozen target state: %+v, want %s", target, want)
					}
					if want == "QUEUED" {
						if _, err := store.GetAgentAssignment(ctx, workspace, target.InstanceUID); !errors.Is(err, ErrNotFound) {
							t.Fatalf("queued target unexpectedly activated: %v", err)
						}
					}
				}
				var events int64
				if err := store.db.Model(&ConfigEventRecord{}).
					Where("workspace_id = ? AND release_id = ? AND event_type = ?",
						workspace, old.ReleaseID, "ROLLOUT_PAUSED").
					Count(&events).Error; err != nil || events != 1 {
					t.Fatalf("missing old rollout pause audit event: count=%d err=%v", events, err)
				}
				if _, err := store.AdvanceRollout(ctx, workspace, old.ReleaseID, "admin"); !errors.Is(err, ErrRolloutBlocked) {
					t.Fatalf("superseded rollout still advanceable: %v", err)
				}
			})
		}
	}
}
