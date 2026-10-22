//go:build integration

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
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

func TestRolloutAdminHTTPPostgresAndMySQL(t *testing.T) {
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
			store, err := gormrepo.Open(ctx, appconfig.Database{
				Driver: tt.driver, DSN: dsn, MaxOpen: 8, MaxIdle: 2, MaxLifetime: time.Minute,
			})
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			workspace := "p3-rollout-http"
			owner := auth.Principal{WorkspaceID: workspace, CredentialID: "java-owner"}
			uids := make([]string, 3)
			for i := range uids {
				raw := make([]byte, 16)
				if _, err := rand.Read(raw); err != nil {
					t.Fatal(err)
				}
				uids[i] = hex.EncodeToString(raw)
				if _, err := store.Observe(ctx, owner, uids[i], gormrepo.Observation{
					SeenAt: time.Now().UTC(), Transport: "http",
					Capabilities: uint64(protobufs.AgentCapabilities_AgentCapabilities_AcceptsRemoteConfig),
					Description:  &gormrepo.Description{Kind: config.JavaAgentKind},
				}); err != nil {
					t.Fatal(err)
				}
			}
			snapshot, err := config.CompileJavaSampling(0.5)
			if err != nil {
				t.Fatal(err)
			}
			revision, err := store.CreateConfigRevision(ctx, workspace, "admin", snapshot)
			if err != nil {
				t.Fatal(err)
			}
			creds := auth.New([]appconfig.Credential{
				{ID: "admin", WorkspaceID: workspace, Token: "rollout-admin-a-long"},
				{ID: "other", WorkspaceID: "p3-other", Token: "rollout-admin-b-long"},
			})
			var waked []string
			h := adminRolloutHandler(store, creds, func(ws, uid string) {
				if ws != workspace {
					t.Errorf("cross Workspace notification: %s", ws)
				}
				waked = append(waked, uid)
			})
			mux := http.NewServeMux()
			mux.Handle("POST /api/v1/rollouts", h)
			mux.Handle("POST /api/v1/rollouts/{release_id}/advance", h)
			mux.Handle("GET /api/v1/rollouts/{release_id}", h)
			server := httptest.NewServer(mux)
			defer server.Close()
			call := func(method, path, token, payload string, status int, idempotencyKey ...string) gormrepo.RolloutView {
				t.Helper()
				req, err := http.NewRequest(method, server.URL+path, bytes.NewBufferString(payload))
				if err != nil {
					t.Fatal(err)
				}
				if token != "" {
					req.Header.Set("Authorization", "Bearer "+token)
				}
				if len(idempotencyKey) > 0 {
					req.Header.Set("Idempotency-Key", idempotencyKey[0])
				}
				resp, err := server.Client().Do(req)
				if err != nil {
					t.Fatal(err)
				}
				defer resp.Body.Close()
				if resp.StatusCode != status {
					t.Fatalf("%s %s HTTP %d, want %d", method, path, resp.StatusCode, status)
				}
				var view gormrepo.RolloutView
				if resp.StatusCode == 200 || resp.StatusCode == 202 {
					if err := json.NewDecoder(resp.Body).Decode(&view); err != nil {
						t.Fatal(err)
					}
				}
				return view
			}
			command, err := json.Marshal(map[string]any{
				"config_revision_id": revision.ConfigRevisionID, "instance_uids": uids, "batch_size": 1,
			})
			if err != nil {
				t.Fatal(err)
			}
			call("POST", "/api/v1/rollouts", "", string(command), 401)
			call("POST", "/api/v1/rollouts", "rollout-admin-b-long", string(command), 404)
			call("POST", "/api/v1/rollouts", "rollout-admin-a-long", `{"batch_size":1,"unexpected":true}`, 400)
			result := call("POST", "/api/v1/rollouts", "rollout-admin-a-long", string(command), 202)
			if result.ReleaseID == "" || len(result.Targets) != 3 || len(result.Activated) != 1 || len(waked) != 1 {
				t.Fatalf("unexpected first batch: %+v wake=%v", result, waked)
			}
			id := result.ReleaseID
			call("GET", "/api/v1/rollouts/"+id, "rollout-admin-b-long", "", 404)
			call("POST", "/api/v1/rollouts/"+id+"/advance", "rollout-admin-a-long", "", 409)
			for batch := 0; batch < 3; batch++ {
				view := call("GET", "/api/v1/rollouts/"+id, "rollout-admin-a-long", "", 200)
				target := view.Targets[batch]
				offered, _, err := store.ReconcileAgentConfig(ctx, workspace, target.InstanceUID, nil, true)
				if err != nil || offered == nil {
					t.Fatalf("batch %d not offered: %v", batch, err)
				}
				if _, _, err := store.ReconcileAgentConfig(ctx, workspace, target.InstanceUID,
					&gormrepo.ConfigStatus{Hash: offered.ConfigHash, Status: "APPLIED"}, false); err != nil {
					t.Fatal(err)
				}
				if batch < 2 {
					next := call("POST", "/api/v1/rollouts/"+id+"/advance", "rollout-admin-a-long", "", 200)
					if len(next.Activated) != 1 || len(waked) != batch+2 {
						t.Fatalf("wrong notified batch: %+v wakes=%v", next, waked)
					}
				}
			}
			final := call("GET", "/api/v1/rollouts/"+id, "rollout-admin-a-long", "", 200)
			if final.Status != "COMPLETED" || len(waked) != 3 {
				t.Fatalf("rollout did not complete: %+v wakes=%v", final, waked)
			}

			// PAUSED 是合法查询和幂等重试结果，不应错误地映射为 HTTP 409。
			pausedKey := "paused-" + id[:12]
			paused := call("POST", "/api/v1/rollouts", "rollout-admin-a-long",
				string(command), http.StatusAccepted, pausedKey)
			if len(paused.Activated) != 1 {
				t.Fatalf("second rollout did not activate first batch: %+v", paused)
			}
			offered, _, err := store.ReconcileAgentConfig(ctx, workspace, paused.Activated[0], nil, true)
			if err != nil || offered == nil {
				t.Fatalf("paused release offer missing: %v", err)
			}
			if _, _, err := store.ReconcileAgentConfig(ctx, workspace, paused.Activated[0],
				&gormrepo.ConfigStatus{Hash: offered.ConfigHash, Status: "FAILED", Error: "rejected"}, false); err != nil {
				t.Fatal(err)
			}
			got := call("GET", "/api/v1/rollouts/"+paused.ReleaseID, "rollout-admin-a-long",
				"", http.StatusOK)
			if got.Status != "PAUSED" {
				t.Fatalf("GET failed to return paused state: %+v", got)
			}
			wakes := len(waked)
			replayed := call("POST", "/api/v1/rollouts", "rollout-admin-a-long",
				string(command), http.StatusAccepted, pausedKey)
			if replayed.Status != "PAUSED" || replayed.ReleaseID != paused.ReleaseID ||
				len(replayed.Activated) != 0 || len(waked) != wakes {
				t.Fatalf("idempotent replay changed paused rollout: %+v wakes=%v", replayed, waked)
			}
			call("POST", "/api/v1/rollouts/"+paused.ReleaseID+"/advance",
				"rollout-admin-a-long", "", http.StatusConflict)
		})
	}
}
