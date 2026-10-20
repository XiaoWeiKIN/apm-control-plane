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
				Description: &Description{Kind: config.JavaAgentKind},
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
