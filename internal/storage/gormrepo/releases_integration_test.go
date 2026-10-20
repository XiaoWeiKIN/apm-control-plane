//go:build integration

package gormrepo

import (
	"context"
	"errors"
	"fmt"
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

func TestSingleAgentReleaseContractPostgresAndMySQL(t *testing.T) {
	for _, tt := range []struct{ driver, env string }{
		{"postgres", "APM_TEST_POSTGRES_DSN"},
		{"mysql", "APM_TEST_MYSQL_DSN"},
	} {
		t.Run(tt.driver, func(t *testing.T) {
			dsn := os.Getenv(tt.env)
			if dsn == "" {
				t.Skipf("%s not configured", tt.env)
			}
			ctx := context.Background()
			cfg := appconfig.Database{Driver: tt.driver, DSN: dsn, MaxOpen: 10, MaxIdle: 2, MaxLifetime: time.Minute}
			store, err := Open(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if store != nil {
					_ = store.Close()
				}
			}()
			id := newUID(t)
			p := auth.Principal{WorkspaceID: "release-workspace", CredentialID: "java-agent"}
			obs := Observation{
				SeenAt: time.Now().UTC(), Transport: "http",
				Capabilities: uint64(protobufs.AgentCapabilities_AgentCapabilities_ReportsStatus |
					protobufs.AgentCapabilities_AgentCapabilities_AcceptsRemoteConfig),
				Description: &Description{Kind: config.JavaAgentKind, Distribution: "example-java", Service: "checkout"},
			}
			if _, err := store.Observe(ctx, p, id, obs); err != nil {
				t.Fatal(err)
			}
			snapshot1, _ := config.CompileJavaSampling(0.1)
			revision1, err := store.CreateConfigRevision(ctx, p.WorkspaceID, "admin-a", snapshot1)
			if err != nil {
				t.Fatal(err)
			}
			snapshot2, _ := config.CompileJavaSampling(0.2)
			revision2, err := store.CreateConfigRevision(ctx, p.WorkspaceID, "admin-a", snapshot2)
			if err != nil {
				t.Fatal(err)
			}
			key := "release-" + id[:16]

			// Workspace and ConfigRevision mismatches cannot create release rows.
			if _, err := store.PublishAgentConfig(ctx, "other-workspace", id, revision1.ConfigRevisionID, "admin-a", ""); !errors.Is(err, ErrNotFound) {
				t.Fatalf("cross-workspace Agent access: %v", err)
			}
			if _, err := store.PublishAgentConfig(ctx, p.WorkspaceID, newUID(t), revision1.ConfigRevisionID, "admin-a", ""); !errors.Is(err, ErrNotFound) {
				t.Fatalf("unknown Agent release: %v", err)
			}
			if _, err := store.PublishAgentConfig(ctx, p.WorkspaceID, id, "00000000-0000-4000-8000-000000000000", "admin-a", ""); !errors.Is(err, ErrConfigRevisionNotFound) {
				t.Fatalf("unknown ConfigRevision release: %v", err)
			}
			if _, err := store.PublishAgentConfig(ctx, p.WorkspaceID, id, revision1.ConfigRevisionID, "admin-a", ""); err != nil {
				// A successful release here would be untracked; use key only for the intended first release.
				t.Fatal(err)
			}
			// First release above is intentional: verify a no-idempotency-key request is valid.
			var existing AgentAssignmentRecord
			if err := store.db.Where("workspace_id = ? AND instance_uid = ?", p.WorkspaceID, id).Take(&existing).Error; err != nil {
				t.Fatal(err)
			}
			if existing.DesiredGeneration != 1 {
				t.Fatalf("first Generation = %d", existing.DesiredGeneration)
			}

			first, err := store.PublishAgentConfig(ctx, p.WorkspaceID, id, revision1.ConfigRevisionID, "admin-a", key)
			if err != nil {
				t.Fatal(err)
			}
			if first.DesiredGeneration != 2 || first.Status != "PENDING" {
				t.Fatalf("unexpected release: %+v", first)
			}
			again, err := store.PublishAgentConfig(ctx, p.WorkspaceID, id, revision1.ConfigRevisionID, "admin-a", key)
			if err != nil || again.ReleaseID != first.ReleaseID || again.DesiredGeneration != first.DesiredGeneration {
				t.Fatalf("idempotent request created duplicate: %+v %v", again, err)
			}
			if _, err := store.PublishAgentConfig(ctx, p.WorkspaceID, id, revision2.ConfigRevisionID, "admin-a", key); !errors.Is(err, ErrReleaseConflict) {
				t.Fatalf("same idempotency key with different request: %v", err)
			}
			assignment, err := store.GetAgentAssignment(ctx, p.WorkspaceID, id)
			if err != nil || assignment.ReleaseID != first.ReleaseID || assignment.DesiredGeneration != 2 ||
				assignment.RowVersion != 2 || assignment.State != "PENDING" || assignment.DesiredHash != revision1.ConfigHash {
				t.Fatalf("current Desired not atomically updated: %+v %v", assignment, err)
			}

			// Failure in the last audit write must roll back Release, Target and new Assignment.
			const callback = "test:reject-audit-insert"
			callbackErr := errors.New("injected audit failure")
			if err := store.db.Callback().Create().Before("gorm:create").Register(callback, func(tx *gorm.DB) {
				if tx.Statement != nil && tx.Statement.Schema != nil && tx.Statement.Schema.Table == "config_events" {
					tx.AddError(callbackErr)
				}
			}); err != nil {
				t.Fatal(err)
			}
			_, err = store.PublishAgentConfig(ctx, p.WorkspaceID, id, revision2.ConfigRevisionID, "admin-a", "fail-"+id[:16])
			if !errors.Is(err, callbackErr) {
				t.Fatalf("audit failure not propagated: %v", err)
			}
			if err := store.db.Callback().Create().Remove(callback); err != nil {
				t.Fatal(err)
			}
			still, err := store.GetAgentAssignment(ctx, p.WorkspaceID, id)
			if err != nil || still.ReleaseID != first.ReleaseID || still.DesiredGeneration != 2 || still.DesiredHash != revision1.ConfigHash {
				t.Fatalf("rollback left an incomplete Desired: %+v %v", still, err)
			}
			for _, table := range []string{"config_releases", "release_targets", "config_events"} {
				var count int64
				if err := store.db.Table(table).Where("workspace_id = ?", p.WorkspaceID).Count(&count).Error; err != nil {
					t.Fatal(err)
				}
				if count != 2 {
					t.Fatalf("%s rows after rollback = %d; want 2", table, count)
				}
			}

			// Concurrent publishes for the same UID must produce distinct, ordered Generations.
			results := make(chan ReleaseResult, 2)
			errs := make(chan error, 2)
			var wg sync.WaitGroup
			for i := 0; i < 2; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					result, err := store.PublishAgentConfig(ctx, p.WorkspaceID, id, revision2.ConfigRevisionID,
						"admin-a", fmt.Sprintf("concurrent-%d-%s", i, id[:16]))
					results <- result
					errs <- err
				}(i)
			}
			wg.Wait()
			close(results)
			close(errs)
			for err := range errs {
				if err != nil {
					t.Fatalf("concurrent release: %v", err)
				}
			}
			gens := map[int64]bool{}
			for result := range results {
				gens[result.DesiredGeneration] = true
			}
			if !gens[3] || !gens[4] || len(gens) != 2 {
				t.Fatalf("generations not serialized: %v", gens)
			}
			current, err := store.GetAgentAssignment(ctx, p.WorkspaceID, id)
			if err != nil || current.DesiredGeneration != 4 || current.RowVersion != 4 ||
				current.DesiredHash != revision2.ConfigHash || current.State != "PENDING" {
				t.Fatalf("latest Desired wrong: %+v %v", current, err)
			}
			var pending, superseded int64
			if err := store.db.Model(&ReleaseTargetRecord{}).Where("workspace_id = ? AND instance_uid = ? AND status = ?", p.WorkspaceID, id, "PENDING").Count(&pending).Error; err != nil {
				t.Fatal(err)
			}
			if err := store.db.Model(&ReleaseTargetRecord{}).Where("workspace_id = ? AND instance_uid = ? AND status = ?", p.WorkspaceID, id, "SUPERSEDED").Count(&superseded).Error; err != nil {
				t.Fatal(err)
			}
			if pending != 1 || superseded != 3 {
				t.Fatalf("target statuses: pending=%d superseded=%d", pending, superseded)
			}
			for _, table := range []string{"config_releases", "release_targets", "config_events"} {
				var count int64
				if err := store.db.Table(table).Where("workspace_id = ?", p.WorkspaceID).Count(&count).Error; err != nil {
					t.Fatal(err)
				}
				if count != 4 {
					t.Fatalf("%s rows = %d; want 4", table, count)
				}
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			store = nil
			store, err = Open(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			restored, err := store.GetAgentAssignment(ctx, p.WorkspaceID, id)
			if err != nil || restored.ReleaseID != current.ReleaseID || restored.DesiredGeneration != 4 {
				t.Fatalf("restart lost Desired: %+v %v", restored, err)
			}
			if _, err := store.GetAgentAssignment(ctx, "other-workspace", id); !errors.Is(err, ErrNotFound) {
				t.Fatalf("cross-workspace assignment leaked: %v", err)
			}
		})
	}
}

func TestUnsupportedReleaseDoesNotCreateAssignment(t *testing.T) {
	for _, tt := range []struct{ driver, env string }{{"postgres", "APM_TEST_POSTGRES_DSN"}, {"mysql", "APM_TEST_MYSQL_DSN"}} {
		t.Run(tt.driver, func(t *testing.T) {
			dsn := os.Getenv(tt.env)
			if dsn == "" {
				t.Skip(tt.env + " not configured")
			}
			ctx := context.Background()
			store, err := Open(ctx, appconfig.Database{Driver: tt.driver, DSN: dsn, MaxOpen: 3, MaxIdle: 1, MaxLifetime: time.Minute})
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			id := newUID(t)
			p := auth.Principal{WorkspaceID: "reject-workspace", CredentialID: "nonjava-agent"}
			if _, err := store.Observe(ctx, p, id, Observation{SeenAt: time.Now().UTC(), Transport: "http", Capabilities: 3, Description: &Description{Kind: "go-apm"}}); err != nil {
				t.Fatal(err)
			}
			snapshot, _ := config.CompileJavaSampling(0.5)
			revision, err := store.CreateConfigRevision(ctx, p.WorkspaceID, "admin", snapshot)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.PublishAgentConfig(ctx, p.WorkspaceID, id, revision.ConfigRevisionID, "admin", ""); !errors.Is(err, ErrUnsupportedReleaseTarget) {
				t.Fatalf("non-Java Agent accepted Java config: %v", err)
			}
			if _, err := store.GetAgentAssignment(ctx, p.WorkspaceID, id); !errors.Is(err, ErrNotFound) {
				t.Fatalf("unsupported config wrote Desired: %v", err)
			}
		})
	}
}

func TestReleaseIdempotencyKeyAcrossConcurrentTargets(t *testing.T) {
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
			store, err := Open(ctx, appconfig.Database{Driver: tt.driver, DSN: dsn, MaxOpen: 5, MaxIdle: 2, MaxLifetime: time.Minute})
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			workspace := "concurrent-idempotency-workspace"
			uidA, uidB := newUID(t), newUID(t)
			for _, id := range []string{uidA, uidB} {
				if _, err := store.Observe(ctx, auth.Principal{WorkspaceID: workspace, CredentialID: "owner"},
					id, Observation{
						SeenAt: time.Now().UTC(), Transport: "http",
						Capabilities: uint64(protobufs.AgentCapabilities_AgentCapabilities_ReportsStatus |
							protobufs.AgentCapabilities_AgentCapabilities_AcceptsRemoteConfig),
						Description: &Description{Kind: config.JavaAgentKind},
					}); err != nil {
					t.Fatal(err)
				}
			}
			snapshot, _ := config.CompileJavaSampling(0.5)
			revision, err := store.CreateConfigRevision(ctx, workspace, "admin", snapshot)
			if err != nil {
				t.Fatal(err)
			}
			type outcome struct {
				uid string
				err error
			}
			results := make(chan outcome, 2)
			var wg sync.WaitGroup
			key := "shared-" + uidA[:16]
			for _, id := range []string{uidA, uidB} {
				wg.Add(1)
				go func(id string) {
					defer wg.Done()
					_, err := store.PublishAgentConfig(ctx, workspace, id, revision.ConfigRevisionID, "admin", key)
					results <- outcome{uid: id, err: err}
				}(id)
			}
			wg.Wait()
			close(results)
			var succeeded, conflicted int
			for result := range results {
				switch {
				case result.err == nil:
					succeeded++
				case errors.Is(result.err, ErrReleaseConflict):
					conflicted++
				default:
					t.Fatalf("unexpected idempotency race error for %s: %v", result.uid, result.err)
				}
			}
			if succeeded != 1 || conflicted != 1 {
				t.Fatalf("concurrent idempotency result: succeeded=%d conflicted=%d", succeeded, conflicted)
			}
			var count int64
			if err := store.db.Model(&ConfigReleaseRecord{}).
				Where("workspace_id = ? AND idempotency_key = ?", workspace, key).
				Count(&count).Error; err != nil {
				t.Fatal(err)
			}
			if count != 1 {
				t.Fatalf("same idempotency key produced %d Release rows", count)
			}
		})
	}
}
