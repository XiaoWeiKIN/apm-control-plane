//go:build integration

package gormrepo

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/XiaoWeiKIN/apm-control-plane/internal/appconfig"
	"github.com/XiaoWeiKIN/apm-control-plane/internal/auth"
	"github.com/XiaoWeiKIN/apm-control-plane/internal/config"
	"github.com/open-telemetry/opamp-go/protobufs"
)

func TestConcurrentReleaseAndACKPreservesLatestDesiredPostgresAndMySQL(t *testing.T) {
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
			p := auth.Principal{WorkspaceID: "ack-race-workspace", CredentialID: "java-agent"}
			uid := newUID(t)
			if _, err := store.Observe(ctx, p, uid, Observation{
				SeenAt: time.Now().UTC(), Transport: "websocket",
				Capabilities: uint64(protobufs.AgentCapabilities_AgentCapabilities_AcceptsRemoteConfig),
				Description:  &Description{Kind: config.JavaAgentKind},
			}); err != nil {
				t.Fatal(err)
			}
			var revisions [2]ConfigRevisionRecord
			for i, rate := range []float64{0.15, 0.85} {
				snapshot, err := config.CompileJavaSampling(rate)
				if err != nil {
					t.Fatal(err)
				}
				revisions[i], err = store.CreateConfigRevision(ctx, p.WorkspaceID, "admin", snapshot)
				if err != nil {
					t.Fatal(err)
				}
			}
			if _, err := store.PublishAgentConfig(ctx, p.WorkspaceID, uid, revisions[0].ConfigRevisionID, "admin", ""); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 6; i++ {
				current, err := store.GetAgentAssignment(ctx, p.WorkspaceID, uid)
				if err != nil {
					t.Fatal(err)
				}
				if offered, exists, err := store.ReconcileAgentConfig(ctx, p.WorkspaceID, uid, nil, true); err != nil || !exists || offered == nil {
					t.Fatalf("failed to offer generation %d: %+v %t %v", current.DesiredGeneration, offered, exists, err)
				}
				next := revisions[(i+1)%2]
				start := make(chan struct{})
				var wg sync.WaitGroup
				var publishErr, ackErr error
				wg.Add(2)
				go func() {
					defer wg.Done()
					<-start
					_, publishErr = store.PublishAgentConfig(ctx, p.WorkspaceID, uid, next.ConfigRevisionID, "admin", "")
				}()
				go func() {
					defer wg.Done()
					<-start
					_, _, ackErr = store.ReconcileAgentConfig(ctx, p.WorkspaceID, uid,
						&ConfigStatus{Hash: current.DesiredHash, Status: "APPLIED"}, false)
				}()
				close(start)
				wg.Wait()
				if publishErr != nil || ackErr != nil {
					t.Fatalf("release/ACK race: publish=%v ack=%v", publishErr, ackErr)
				}
				latest, err := store.GetAgentAssignment(ctx, p.WorkspaceID, uid)
				if err != nil {
					t.Fatal(err)
				}
				if latest.DesiredGeneration != current.DesiredGeneration+1 ||
					latest.DesiredHash != next.ConfigHash || latest.State != "PENDING" {
					t.Fatalf("old ACK corrupted new Desired: before=%+v after=%+v", current, latest)
				}
			}
		})
	}
}

func TestFailedACKPersistsReleaseDiagnosticsPostgresAndMySQL(t *testing.T) {
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
				Driver: tt.driver, DSN: dsn, MaxOpen: 5, MaxIdle: 2, MaxLifetime: time.Minute,
			})
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			workspace, uid := "failed-ack-diagnostics", newUID(t)
			if _, err := store.Observe(ctx, auth.Principal{WorkspaceID: workspace, CredentialID: "java-agent"},
				uid, Observation{
					SeenAt: time.Now().UTC(), Transport: "http",
					Capabilities: uint64(protobufs.AgentCapabilities_AgentCapabilities_AcceptsRemoteConfig),
					Description: &Description{Kind: config.JavaAgentKind},
				}); err != nil {
				t.Fatal(err)
			}
			snapshot, err := config.CompileJavaSampling(0.25)
			if err != nil {
				t.Fatal(err)
			}
			revision, err := store.CreateConfigRevision(ctx, workspace, "admin", snapshot)
			if err != nil {
				t.Fatal(err)
			}
			release, err := store.PublishAgentConfig(ctx, workspace, uid, revision.ConfigRevisionID, "admin", "")
			if err != nil {
				t.Fatal(err)
			}
			if offered, assigned, err := store.ReconcileAgentConfig(ctx, workspace, uid, nil, true); err != nil || !assigned || offered == nil {
				t.Fatalf("offer failed: %+v %t %v", offered, assigned, err)
			}
			for _, message := range []string{"apply failed", "agent rejected sampling"} {
				if _, _, err := store.ReconcileAgentConfig(ctx, workspace, uid,
					&ConfigStatus{Hash: revision.ConfigHash, Status: "FAILED", Error: message}, false); err != nil {
					t.Fatal(err)
				}
				assignment, err := store.GetAgentAssignment(ctx, workspace, uid)
				if err != nil || assignment.State != "FAILED" || assignment.LastError != message {
					t.Fatalf("assignment failed diagnostics: %+v %v", assignment, err)
				}
				var target ReleaseTargetRecord
				if err := store.db.Where("workspace_id = ? AND release_id = ? AND instance_uid = ?",
					workspace, release.ReleaseID, uid).Take(&target).Error; err != nil {
					t.Fatal(err)
				}
				if target.Status != "FAILED" || target.LastError != message {
					t.Fatalf("target failed diagnostics: %+v", target)
				}
				var event ConfigEventRecord
				if err := store.db.Where("workspace_id = ? AND release_id = ? AND event_type = ?",
					workspace, release.ReleaseID, "CONFIG_FAILED").Order("occurred_at DESC").Take(&event).Error; err != nil {
					t.Fatal(err)
				}
				if event.DetailsJSON != `{"error":"`+message+`"}` {
					t.Fatalf("audit diagnostic = %q", event.DetailsJSON)
				}
			}
			next, err := store.PublishAgentConfig(ctx, workspace, uid, revision.ConfigRevisionID, "admin", "")
			if err != nil {
				t.Fatal(err)
			}
			assignment, err := store.GetAgentAssignment(ctx, workspace, uid)
			if err != nil || assignment.DesiredGeneration != next.DesiredGeneration ||
				assignment.State != "PENDING" || assignment.LastError != "" {
				t.Fatalf("new release retained stale failure: %+v %v", assignment, err)
			}
		})
	}
}
