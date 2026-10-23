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

func TestExplicitRollbackPostgresAndMySQL(t *testing.T) {
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
			const workspace = "p33-rollback-contract"
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
			oldSnapshot, _ := config.CompileJavaSampling(0.1)
			newSnapshot, _ := config.CompileJavaSampling(0.9)
			oldConfig, err := store.CreateConfigRevision(ctx, workspace, "admin", oldSnapshot)
			if err != nil {
				t.Fatal(err)
			}
			newConfig, err := store.CreateConfigRevision(ctx, workspace, "admin", newSnapshot)
			if err != nil {
				t.Fatal(err)
			}
			source, err := store.PublishRollout(ctx, workspace, "admin", newConfig.ConfigRevisionID, uids, 1, "")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.PublishRollback(ctx, "other-workspace", "admin", source.ReleaseID,
				oldConfig.ConfigRevisionID, source.RowVersion, 1, ""); !errors.Is(err, ErrRolloutNotFound) {
				t.Fatalf("cross-workspace rollback: %v", err)
			}
			if _, err := store.PublishRollback(ctx, workspace, "admin", source.ReleaseID,
				newConfig.ConfigRevisionID, source.RowVersion, 1, ""); !errors.Is(err, ErrInvalidRollout) {
				t.Fatalf("same config is not a compensating rollback: %v", err)
			}
			if _, err := store.PublishRollback(ctx, workspace, "admin", source.ReleaseID,
				oldConfig.ConfigRevisionID, source.RowVersion+1, 1, ""); !errors.Is(err, ErrAssignmentConflict) {
				t.Fatalf("stale source CAS accepted: %v", err)
			}

			// 1. 新 Release 使用原冻结 UID 与显式旧 ConfigRevision，历史 Release 保持不变。
			key := "p33-" + uids[0][:16]
			rollback, err := store.PublishRollback(ctx, workspace, "admin", source.ReleaseID,
				oldConfig.ConfigRevisionID, source.RowVersion, 1, key)
			if err != nil {
				t.Fatal(err)
			}
			if rollback.ReleaseID == source.ReleaseID || rollback.RollbackOfReleaseID == nil ||
				*rollback.RollbackOfReleaseID != source.ReleaseID || len(rollback.Targets) != 3 ||
				len(rollback.Activated) != 1 {
				t.Fatalf("invalid compensating release: %+v", rollback)
			}
			for _, target := range rollback.Targets {
				if target.ConfigRevisionID != oldConfig.ConfigRevisionID {
					t.Fatalf("rollback target lost chosen config: %+v", target)
				}
			}
			var original ConfigReleaseRecord
			if err := store.db.Where("workspace_id = ? AND release_id = ?", workspace, source.ReleaseID).
				Take(&original).Error; err != nil {
				t.Fatal(err)
			}
			if original.RollbackOfReleaseID != nil || original.RequestSHA256 == "" {
				t.Fatalf("historical source modified: %+v", original)
			}
			var auditCount int64
			if err := store.db.Model(&ConfigEventRecord{}).
				Where("workspace_id = ? AND release_id = ? AND event_type = ?",
					workspace, rollback.ReleaseID, "ROLLBACK_REQUESTED").
				Count(&auditCount).Error; err != nil || auditCount != 1 {
				t.Fatalf("missing rollback audit: %d %v", auditCount, err)
			}
			again, err := store.PublishRollback(ctx, workspace, "admin", source.ReleaseID,
				oldConfig.ConfigRevisionID, source.RowVersion, 1, key)
			if err != nil || again.ReleaseID != rollback.ReleaseID || len(again.Activated) != 0 {
				t.Fatalf("rollback idempotency: %+v %v", again, err)
			}
			if _, err := store.PublishRollback(ctx, workspace, "admin", source.ReleaseID,
				oldConfig.ConfigRevisionID, source.RowVersion, 2, key); !errors.Is(err, ErrReleaseConflict) {
				t.Fatalf("key reused for changed batch size: %v", err)
			}

			// 2. 回滚按普通批次收敛，未激活目标不提前获得旧配置。
			for i := range rollback.Targets {
				current, err := store.GetRollout(ctx, workspace, rollback.ReleaseID)
				if err != nil {
					t.Fatal(err)
				}
				target := current.Targets[i]
				offered, _, err := store.ReconcileAgentConfig(ctx, workspace, target.InstanceUID, nil, true)
				if err != nil || offered == nil || offered.ConfigHash != oldConfig.ConfigHash {
					t.Fatalf("rollback ConfigMap missing: %+v %v", offered, err)
				}
				if _, _, err := store.ReconcileAgentConfig(ctx, workspace, target.InstanceUID,
					&ConfigStatus{Hash: offered.ConfigHash, Status: "APPLIED"}, false); err != nil {
					t.Fatal(err)
				}
				if i+1 < len(rollback.Targets) {
					next, err := store.AdvanceRollout(ctx, workspace, rollback.ReleaseID, "admin")
					if err != nil || len(next.Activated) != 1 {
						t.Fatalf("rollback advance: %+v %v", next, err)
					}
				}
			}
			final, err := store.GetRollout(ctx, workspace, rollback.ReleaseID)
			if err != nil || final.Status != "COMPLETED" {
				t.Fatalf("rollback not converged: %+v %v", final, err)
			}
		})
	}
}

func TestRollbackAuditFailureIsAtomicPostgresAndMySQL(t *testing.T) {
	for _, tt := range []struct{ driver, env string }{
		{"postgres", "APM_TEST_POSTGRES_DSN"}, {"mysql", "APM_TEST_MYSQL_DSN"},
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
			const workspace = "p33-rollback-audit"
			p := auth.Principal{WorkspaceID: workspace, CredentialID: "agent"}
			uids := []string{newUID(t), newUID(t)}
			for _, uid := range uids {
				if _, err := store.Observe(ctx, p, uid, Observation{
					SeenAt: time.Now().UTC(), Transport: "http",
					Capabilities: uint64(protobufs.AgentCapabilities_AgentCapabilities_AcceptsRemoteConfig),
					Description:  &Description{Kind: config.JavaAgentKind},
				}); err != nil {
					t.Fatal(err)
				}
			}
			original, _ := config.CompileJavaSampling(0.8)
			recovery, _ := config.CompileJavaSampling(0.2)
			a, err := store.CreateConfigRevision(ctx, workspace, "admin", original)
			if err != nil {
				t.Fatal(err)
			}
			b, err := store.CreateConfigRevision(ctx, workspace, "admin", recovery)
			if err != nil {
				t.Fatal(err)
			}
			source, err := store.PublishRollout(ctx, workspace, "admin", a.ConfigRevisionID, uids, 1, "")
			if err != nil {
				t.Fatal(err)
			}
			const callback = "p33:reject-rollback-audit"
			auditErr := errors.New("injected rollback audit failure")
			if err := store.db.Callback().Create().Before("gorm:create").Register(callback, func(tx *gorm.DB) {
				if tx.Statement != nil && tx.Statement.Schema != nil && tx.Statement.Schema.Table == "config_events" {
					tx.AddError(auditErr)
				}
			}); err != nil {
				t.Fatal(err)
			}
			_, err = store.PublishRollback(ctx, workspace, "admin", source.ReleaseID,
				b.ConfigRevisionID, source.RowVersion, 1, "")
			if !errors.Is(err, auditErr) {
				t.Fatalf("audit failure not propagated: %v", err)
			}
			if err := store.db.Callback().Create().Remove(callback); err != nil {
				t.Fatal(err)
			}
			var releases int64
			if err := store.db.Model(&ConfigReleaseRecord{}).Where("workspace_id = ?", workspace).
				Count(&releases).Error; err != nil || releases != 1 {
				t.Fatalf("partial compensating release persisted: %d %v", releases, err)
			}
			for _, uid := range uids {
				assignment, err := store.GetAgentAssignment(ctx, workspace, uid)
				if err == nil && assignment.ConfigRevisionID == b.ConfigRevisionID {
					t.Fatalf("rollback changed Desired after failed audit: %+v", assignment)
				}
			}
		})
	}
}

func TestRollbackBlocksSourceProgressPostgresAndMySQL(t *testing.T) {
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
				Driver: tt.driver, DSN: dsn, MaxOpen: 8, MaxIdle: 2, MaxLifetime: time.Minute,
			})
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			const workspace = "p33-rollback-block-source"
			uids := []string{newUID(t), newUID(t)}
			for _, uid := range uids {
				if _, err := store.Observe(ctx, auth.Principal{WorkspaceID: workspace, CredentialID: "agent"}, uid,
					Observation{
						SeenAt: time.Now().UTC(), Transport: "http",
						Capabilities: uint64(protobufs.AgentCapabilities_AgentCapabilities_AcceptsRemoteConfig),
						Description:  &Description{Kind: config.JavaAgentKind},
					}); err != nil {
					t.Fatal(err)
				}
			}
			newSnapshot, _ := config.CompileJavaSampling(0.9)
			oldSnapshot, _ := config.CompileJavaSampling(0.1)
			newConfig, err := store.CreateConfigRevision(ctx, workspace, "admin", newSnapshot)
			if err != nil {
				t.Fatal(err)
			}
			oldConfig, err := store.CreateConfigRevision(ctx, workspace, "admin", oldSnapshot)
			if err != nil {
				t.Fatal(err)
			}
			source, err := store.PublishRollout(ctx, workspace, "admin", newConfig.ConfigRevisionID, uids, 1, "")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.PublishRollback(ctx, workspace, "admin", source.ReleaseID,
				oldConfig.ConfigRevisionID, source.RowVersion, 1, ""); err != nil {
				t.Fatal(err)
			}

			// 1. 覆盖源首批 Desired 后，源发布暂停并禁止推进后续批次。
			current, err := store.GetRollout(ctx, workspace, source.ReleaseID)
			if err != nil {
				t.Fatal(err)
			}
			if current.Status != "PAUSED" || current.RowVersion != source.RowVersion+1 ||
				current.Targets[0].Status != "SUPERSEDED" {
				t.Fatalf("source not blocked after compensating release: %+v", current)
			}
			if _, err := store.AdvanceRollout(ctx, workspace, source.ReleaseID, "admin"); !errors.Is(err, ErrRolloutBlocked) {
				t.Fatalf("source advance after rollback: %v", err)
			}
			if _, err := store.ResumeRollout(ctx, workspace, source.ReleaseID, "admin",
				current.RowVersion); !errors.Is(err, ErrRolloutBlocked) {
				t.Fatalf("source resume after rollback: %v", err)
			}
			if _, err := store.PublishRollback(ctx, workspace, "admin", source.ReleaseID,
				oldConfig.ConfigRevisionID, source.RowVersion, 1, ""); !errors.Is(err, ErrAssignmentConflict) {
				t.Fatalf("stale source CAS after rollback: %v", err)
			}
		})
	}
}

func TestConcurrentRollbackAndAdvancePostgresAndMySQL(t *testing.T) {
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
				Driver: tt.driver, DSN: dsn, MaxOpen: 8, MaxIdle: 2, MaxLifetime: time.Minute,
			})
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			const workspace = "p33-rollback-advance-race"
			uids := []string{newUID(t), newUID(t)}
			for _, uid := range uids {
				if _, err := store.Observe(ctx, auth.Principal{WorkspaceID: workspace, CredentialID: "agent"}, uid,
					Observation{
						SeenAt: time.Now().UTC(), Transport: "http",
						Capabilities: uint64(protobufs.AgentCapabilities_AgentCapabilities_AcceptsRemoteConfig),
						Description:  &Description{Kind: config.JavaAgentKind},
					}); err != nil {
					t.Fatal(err)
				}
			}
			newSnapshot, _ := config.CompileJavaSampling(0.9)
			oldSnapshot, _ := config.CompileJavaSampling(0.1)
			newConfig, err := store.CreateConfigRevision(ctx, workspace, "admin", newSnapshot)
			if err != nil {
				t.Fatal(err)
			}
			oldConfig, err := store.CreateConfigRevision(ctx, workspace, "admin", oldSnapshot)
			if err != nil {
				t.Fatal(err)
			}
			source, err := store.PublishRollout(ctx, workspace, "admin", newConfig.ConfigRevisionID, uids, 1, "")
			if err != nil {
				t.Fatal(err)
			}
			// 1. 第一批 APPLIED，使 Advance 与 Rollback 都具备合法前置条件。
			first := source.Targets[0].InstanceUID
			offered, _, err := store.ReconcileAgentConfig(ctx, workspace, first, nil, true)
			if err != nil || offered == nil {
				t.Fatalf("source config not offered: %+v %v", offered, err)
			}
			if _, _, err := store.ReconcileAgentConfig(ctx, workspace, first,
				&ConfigStatus{Hash: offered.ConfigHash, Status: "APPLIED"}, false); err != nil {
				t.Fatal(err)
			}
			source, err = store.GetRollout(ctx, workspace, source.ReleaseID)
			if err != nil {
				t.Fatal(err)
			}
			type outcome struct {
				kind string
				err  error
			}
			start := make(chan struct{})
			outcomes := make(chan outcome, 2)
			go func() {
				<-start
				_, err := store.AdvanceRollout(ctx, workspace, source.ReleaseID, "admin")
				outcomes <- outcome{"advance", err}
			}()
			go func() {
				<-start
				_, err := store.PublishRollback(ctx, workspace, "admin", source.ReleaseID,
					oldConfig.ConfigRevisionID, source.RowVersion, 1, "")
				outcomes <- outcome{"rollback", err}
			}()
			close(start)
			a, b := <-outcomes, <-outcomes
			// 2. 两个操作串行化：先 Advance 则回滚 CAS 冲突，先回滚则旧发布不可推进。
			if a.kind == b.kind {
				t.Fatalf("missing racing operation: %+v %+v", a, b)
			}
			var advanced, rolledBack error
			for _, result := range []outcome{a, b} {
				if result.kind == "advance" {
					advanced = result.err
				} else {
					rolledBack = result.err
				}
			}
			if advanced == nil && !errors.Is(rolledBack, ErrAssignmentConflict) ||
				rolledBack == nil && !errors.Is(advanced, ErrRolloutBlocked) ||
				advanced != nil && rolledBack != nil {
				t.Fatalf("advance/rollback was not serialized: %+v %+v", a, b)
			}
		})
	}
}

func TestConcurrentRollbackIdempotencyPostgresAndMySQL(t *testing.T) {
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
				Driver: tt.driver, DSN: dsn, MaxOpen: 8, MaxIdle: 2, MaxLifetime: time.Minute,
			})
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			const workspace = "p33-rollback-idempotent-race"
			uids := []string{newUID(t), newUID(t)}
			for _, uid := range uids {
				if _, err := store.Observe(ctx, auth.Principal{WorkspaceID: workspace, CredentialID: "agent"}, uid,
					Observation{
						SeenAt: time.Now().UTC(), Transport: "http",
						Capabilities: uint64(protobufs.AgentCapabilities_AgentCapabilities_AcceptsRemoteConfig),
						Description:  &Description{Kind: config.JavaAgentKind},
					}); err != nil {
					t.Fatal(err)
				}
			}
			newSnapshot, _ := config.CompileJavaSampling(0.9)
			oldSnapshot, _ := config.CompileJavaSampling(0.1)
			newConfig, err := store.CreateConfigRevision(ctx, workspace, "admin", newSnapshot)
			if err != nil {
				t.Fatal(err)
			}
			oldConfig, err := store.CreateConfigRevision(ctx, workspace, "admin", oldSnapshot)
			if err != nil {
				t.Fatal(err)
			}
			source, err := store.PublishRollout(ctx, workspace, "admin", newConfig.ConfigRevisionID, uids, 1, "")
			if err != nil {
				t.Fatal(err)
			}
			type outcome struct {
				view RolloutView
				err  error
			}
			start := make(chan struct{})
			results := make(chan outcome, 2)
			key := "concurrent-" + uids[0]
			var wg sync.WaitGroup
			wg.Add(2)
			for i := 0; i < 2; i++ {
				go func() {
					defer wg.Done()
					<-start
					view, err := store.PublishRollback(ctx, workspace, "admin", source.ReleaseID,
						oldConfig.ConfigRevisionID, source.RowVersion, 1, key)
					results <- outcome{view, err}
				}()
			}
			close(start)
			wg.Wait()
			close(results)
			activated := 0
			releaseID := ""
			for r := range results {
				if r.err != nil {
					t.Fatalf("concurrent idempotent rollback: %v", r.err)
				}
				if releaseID != "" && r.view.ReleaseID != releaseID {
					t.Fatalf("duplicate releases: %s %s", releaseID, r.view.ReleaseID)
				}
				releaseID = r.view.ReleaseID
				activated += len(r.view.Activated)
			}
			if activated != 1 {
				t.Fatalf("expected exactly one first-batch activation, got %d", activated)
			}
			var count int64
			if err := store.db.Model(&ConfigEventRecord{}).
				Where("workspace_id = ? AND release_id = ? AND event_type = ?",
					workspace, releaseID, "ROLLBACK_REQUESTED").Count(&count).Error; err != nil || count != 1 {
				t.Fatalf("concurrent rollback created duplicate audit events: %d %v", count, err)
			}
		})
	}
}
