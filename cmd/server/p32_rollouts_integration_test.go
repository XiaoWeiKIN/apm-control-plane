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
	"strconv"
	"testing"
	"time"

	"github.com/XiaoWeiKIN/apm-control-plane/internal/appconfig"
	"github.com/XiaoWeiKIN/apm-control-plane/internal/auth"
	"github.com/XiaoWeiKIN/apm-control-plane/internal/config"
	"github.com/XiaoWeiKIN/apm-control-plane/internal/policy"
	"github.com/XiaoWeiKIN/apm-control-plane/internal/storage/gormrepo"
	"github.com/open-telemetry/opamp-go/protobufs"
)

func TestPolicyRolloutAndResumeAPIPostgresAndMySQL(t *testing.T) {
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
			const workspace = "p32-policy-rollout-api"
			for i := 0; i < 2; i++ {
				uid := make([]byte, 16)
				if _, err := rand.Read(uid); err != nil { t.Fatal(err) }
				if _, err := store.Observe(ctx, auth.Principal{WorkspaceID: workspace, CredentialID: "agent"},
					hex.EncodeToString(uid), gormrepo.Observation{
						SeenAt: time.Now().UTC(), Transport: "http",
						Capabilities: uint64(protobufs.AgentCapabilities_AgentCapabilities_AcceptsRemoteConfig),
						Description: &gormrepo.Description{Kind: config.JavaAgentKind, Service: "checkout", Environment: "prod"},
					}); err != nil { t.Fatal(err) }
			}
			_, revision, err := store.CreatePolicy(ctx, workspace, "admin", "api-policy", 0.3)
			if err != nil { t.Fatal(err) }
			if _, err := store.CreatePolicyBinding(ctx, workspace, "admin", gormrepo.BindingInput{
				PolicyRevisionID: revision.PolicyRevisionID, ScopeType: policy.ScopeService,
				Selector: policy.Selector{ServiceName: "checkout"}, Priority: 1,
			}); err != nil { t.Fatal(err) }

			authenticator := auth.New([]appconfig.Credential{
				{ID: "admin", WorkspaceID: workspace, Token: "p32-admin-policy-long"},
				{ID: "other", WorkspaceID: "p32-other-workspace", Token: "p32-other-admin-long"},
			})
			wakes := make([]string, 0, 2)
			h := adminRolloutHandler(store, authenticator, func(ws, uid string) {
				if ws != workspace { t.Errorf("cross-workspace wake: %s", ws) }
				wakes = append(wakes, uid)
			})
			mux := http.NewServeMux()
			mux.Handle("POST /api/v1/policy-rollouts", h)
			mux.Handle("POST /api/v1/rollouts/{release_id}/resume", h)
			mux.Handle("POST /api/v1/rollouts/{release_id}/advance", h)
			mux.Handle("GET /api/v1/rollouts/{release_id}", h)
			server := httptest.NewServer(mux)
			defer server.Close()
			call := func(method, path, token, payload string, expected int) gormrepo.RolloutView {
				t.Helper()
				req, err := http.NewRequest(method, server.URL+path, bytes.NewBufferString(payload))
				if err != nil { t.Fatal(err) }
				if token != "" { req.Header.Set("Authorization", "Bearer "+token) }
				resp, err := server.Client().Do(req)
				if err != nil { t.Fatal(err) }
				defer resp.Body.Close()
				raw, err := io.ReadAll(resp.Body)
				if err != nil { t.Fatal(err) }
				if resp.StatusCode != expected {
					t.Fatalf("%s %s: HTTP %d want %d: %s", method, path, resp.StatusCode, expected, raw)
				}
				var view gormrepo.RolloutView
				if expected == http.StatusOK || expected == http.StatusAccepted {
					if err := json.Unmarshal(raw, &view); err != nil { t.Fatal(err) }
				}
				return view
			}
			body, err := json.Marshal(map[string]any{
				"policy_revision_id": revision.PolicyRevisionID, "environment": "prod",
				"service_name": "checkout", "batch_size": 1,
			})
			if err != nil { t.Fatal(err) }
			call("POST", "/api/v1/policy-rollouts", "", string(body), http.StatusUnauthorized)
			call("POST", "/api/v1/policy-rollouts", "p32-other-admin-long", string(body), http.StatusNotFound)
			call("POST", "/api/v1/policy-rollouts", "p32-admin-policy-long", `{"unexpected":true}`, http.StatusBadRequest)
			release := call("POST", "/api/v1/policy-rollouts", "p32-admin-policy-long", string(body), http.StatusAccepted)
			if release.ReleaseID == "" || len(release.Targets) != 2 || len(release.Activated) != 1 || len(wakes) != 1 {
				t.Fatalf("wrong policy rollout: %+v wakes=%v", release, wakes)
			}
			id := release.ReleaseID
			path := "/api/v1/rollouts/"+id
			call("GET", path, "p32-other-admin-long", "", http.StatusNotFound)
			call("POST", path+"/resume", "p32-admin-policy-long", `{"row_version":0}`, http.StatusBadRequest)
			call("POST", path+"/resume", "p32-admin-policy-long", `{"row_version":1}`, http.StatusConflict)

			first := release.Activated[0]
			offered, _, err := store.ReconcileAgentConfig(ctx, workspace, first, nil, true)
			if err != nil || offered == nil { t.Fatalf("initial ConfigMap not offered: %v", err) }
			if _, _, err := store.ReconcileAgentConfig(ctx, workspace, first,
				&gormrepo.ConfigStatus{Hash: offered.ConfigHash, Status: "FAILED"}, false); err != nil { t.Fatal(err) }
			paused := call("GET", path, "p32-admin-policy-long", "", http.StatusOK)
			if paused.Status != "PAUSED" { t.Fatalf("FAILED did not pause: %+v", paused) }
			call("POST", path+"/resume", "p32-other-admin-long",
				`{"row_version":`+itoa(pauseNumber(paused.RowVersion))+`}`, http.StatusNotFound)
			call("POST", path+"/resume", "p32-admin-policy-long",
				`{"row_version":`+itoa(pauseNumber(paused.RowVersion))+`}`, http.StatusConflict)

			if _, _, err := store.ReconcileAgentConfig(ctx, workspace, first,
				&gormrepo.ConfigStatus{Hash: offered.ConfigHash, Status: "APPLIED"}, false); err != nil { t.Fatal(err) }
			// A late ACK does not clear the administrative pause by itself.
			still := call("GET", path, "p32-admin-policy-long", "", http.StatusOK)
			if still.Status != "PAUSED" { t.Fatalf("automatic resume after ACK: %+v", still) }
			resumed := call("POST", path+"/resume", "p32-admin-policy-long",
				`{"row_version":`+itoa(pauseNumber(still.RowVersion))+`}`, http.StatusOK)
			if resumed.Status != "RUNNING" || len(resumed.Activated) != 0 || len(wakes) != 1 {
				t.Fatalf("resume sent config before Advance: %+v wakes=%v", resumed, wakes)
			}
			advanced := call("POST", path+"/advance", "p32-admin-policy-long", "", http.StatusOK)
			if len(advanced.Activated) != 1 || len(wakes) != 2 { t.Fatalf("advance missed wake: %+v %v", advanced, wakes) }
			second := advanced.Activated[0]
			offered, _, err = store.ReconcileAgentConfig(ctx, workspace, second, nil, true)
			if err != nil || offered == nil { t.Fatalf("next batch ConfigMap: %v", err) }
			if _, _, err := store.ReconcileAgentConfig(ctx, workspace, second,
				&gormrepo.ConfigStatus{Hash: offered.ConfigHash, Status: "APPLIED"}, false); err != nil { t.Fatal(err) }
			done := call("GET", path, "p32-admin-policy-long", "", http.StatusOK)
			if done.Status != "COMPLETED" { t.Fatalf("policy rollout did not complete: %+v", done) }
		})
	}
}
