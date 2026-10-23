//go:build integration

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/XiaoWeiKIN/apm-control-plane/internal/appconfig"
	"github.com/XiaoWeiKIN/apm-control-plane/internal/auth"
	"github.com/XiaoWeiKIN/apm-control-plane/internal/config"
	"github.com/XiaoWeiKIN/apm-control-plane/internal/storage/gormrepo"
	"github.com/open-telemetry/opamp-go/protobufs"
)

func TestRollbackAdminAPIPostgresAndMySQL(t *testing.T) {
	for _, tt := range []struct{ driver, env string }{
		{"postgres", "APM_TEST_POSTGRES_DSN"},
		{"mysql", "APM_TEST_MYSQL_DSN"},
	} {
		t.Run(tt.driver, func(t *testing.T) {
			dsn := os.Getenv(tt.env)
			if dsn == "" { t.Skip(tt.env + " not configured") }
			ctx := context.Background()
			store, err := gormrepo.Open(ctx, appconfig.Database{
				Driver: tt.driver, DSN: dsn, MaxOpen: 8, MaxIdle: 2, MaxLifetime: time.Minute,
			})
			if err != nil { t.Fatal(err) }
			defer store.Close()
			const workspace = "p33-rollback-http"
			uids := make([]string, 2)
			for i := range uids {
				raw := make([]byte, 16)
				if _, err := rand.Read(raw); err != nil { t.Fatal(err) }
				uids[i] = hex.EncodeToString(raw)
				if _, err := store.Observe(ctx, auth.Principal{WorkspaceID: workspace, CredentialID: "agent"},
					uids[i], gormrepo.Observation{
						SeenAt: time.Now().UTC(), Transport: "http",
						Capabilities: uint64(protobufs.AgentCapabilities_AgentCapabilities_AcceptsRemoteConfig),
						Description: &gormrepo.Description{Kind: config.JavaAgentKind},
					}); err != nil { t.Fatal(err) }
			}
			first, _ := config.CompileJavaSampling(0.8)
			second, _ := config.CompileJavaSampling(0.1)
			a, err := store.CreateConfigRevision(ctx, workspace, "admin", first)
			if err != nil { t.Fatal(err) }
			b, err := store.CreateConfigRevision(ctx, workspace, "admin", second)
			if err != nil { t.Fatal(err) }
			source, err := store.PublishRollout(ctx, workspace, "admin", a.ConfigRevisionID, uids, 1, "")
			if err != nil { t.Fatal(err) }
			creds := auth.New([]appconfig.Credential{
				{ID: "admin", WorkspaceID: workspace, Token: "p33-admin-long"},
				{ID: "other", WorkspaceID: "p33-other", Token: "p33-other-long"},
			})
			wakes := make([]string, 0, 1)
			h := adminRolloutHandler(store, creds, func(ws, uid string) {
				if ws != workspace { t.Errorf("wrong Workspace wake: %s", ws) }
				wakes = append(wakes, uid)
			})
			mux := http.NewServeMux()
			mux.Handle("POST /api/v1/rollouts/{release_id}/rollback", h)
			server := httptest.NewServer(mux)
			defer server.Close()
			path := "/api/v1/rollouts/" + source.ReleaseID + "/rollback"
			call := func(token, body string, status int) gormrepo.RolloutView {
				t.Helper()
				req, err := http.NewRequest(http.MethodPost, server.URL+path, bytes.NewBufferString(body))
				if err != nil { t.Fatal(err) }
				if token != "" { req.Header.Set("Authorization", "Bearer "+token) }
				req.Header.Set("Idempotency-Key", "p33-http-"+uids[0][:16])
				resp, err := server.Client().Do(req)
				if err != nil { t.Fatal(err) }
				defer resp.Body.Close()
				wire, err := io.ReadAll(resp.Body)
				if err != nil { t.Fatal(err) }
				if resp.StatusCode != status {
					t.Fatalf("HTTP %d want %d: %s", resp.StatusCode, status, wire)
				}
				var view gormrepo.RolloutView
				if status == http.StatusAccepted {
					if err := json.Unmarshal(wire, &view); err != nil { t.Fatal(err) }
				}
				return view
			}
			body, err := json.Marshal(map[string]any{
				"config_revision_id": b.ConfigRevisionID, "row_version": source.RowVersion, "batch_size": 1,
			})
			if err != nil { t.Fatal(err) }
			call("", string(body), http.StatusUnauthorized)
			call("p33-other-long", string(body), http.StatusNotFound)
			call("p33-admin-long", `{"config_revision_id":"bad","row_version":1,"batch_size":1,"unexpected":true}`, http.StatusBadRequest)
			result := call("p33-admin-long", string(body), http.StatusAccepted)
			if result.RollbackOfReleaseID == nil || *result.RollbackOfReleaseID != source.ReleaseID ||
				len(result.Activated) != 1 || len(wakes) != 1 {
				t.Fatalf("HTTP rollback did not freeze and wake first batch: %+v wakes=%v", result, wakes)
			}
			replay := call("p33-admin-long", string(body), http.StatusAccepted)
			if replay.ReleaseID != result.ReleaseID || len(replay.Activated) != 0 || len(wakes) != 1 {
				t.Fatalf("idempotent replay caused duplicate push: %+v wakes=%v", replay, wakes)
			}
		})
	}
}
