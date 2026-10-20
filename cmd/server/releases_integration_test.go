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

func TestAdminReleaseHTTPPersistencePostgresAndMySQL(t *testing.T) {
	for _, tt := range []struct{ driver, env string }{{"postgres", "APM_TEST_POSTGRES_DSN"}, {"mysql", "APM_TEST_MYSQL_DSN"}} {
		t.Run(tt.driver, func(t *testing.T) {
			dsn := os.Getenv(tt.env)
			if dsn == "" {
				t.Skip(tt.env + " not configured")
			}
			ctx := context.Background()
			store, err := gormrepo.Open(ctx, appconfig.Database{Driver: tt.driver, DSN: dsn, MaxOpen: 5, MaxIdle: 1, MaxLifetime: time.Minute})
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			uidBytes := make([]byte, 16)
			if _, err := rand.Read(uidBytes); err != nil {
				t.Fatal(err)
			}
			uid := hex.EncodeToString(uidBytes)
			workspace := "api-release-workspace"
			p := auth.Principal{WorkspaceID: workspace, CredentialID: "java-owner"}
			if _, err := store.Observe(ctx, p, uid, gormrepo.Observation{
				SeenAt: time.Now().UTC(), Transport: "http",
				Capabilities: uint64(protobufs.AgentCapabilities_AgentCapabilities_ReportsStatus |
					protobufs.AgentCapabilities_AgentCapabilities_AcceptsRemoteConfig),
				Description: &gormrepo.Description{Kind: config.JavaAgentKind, Service: "checkout"},
			}); err != nil {
				t.Fatal(err)
			}
			snapshot, err := config.CompileJavaSampling(0.25)
			if err != nil {
				t.Fatal(err)
			}
			revision, err := store.CreateConfigRevision(ctx, workspace, "admin-a", snapshot)
			if err != nil {
				t.Fatal(err)
			}
			mux := http.NewServeMux()
			mux.Handle("POST /api/v1/agents/{uid}/releases", adminReleaseHandler(store, auth.New([]appconfig.Credential{
				{ID: "admin-a", WorkspaceID: workspace, Token: "admin-a-test-secret-long"},
				{ID: "admin-b", WorkspaceID: "another-workspace", Token: "admin-b-test-secret-long"},
			}), nil))
			server := httptest.NewServer(mux)
			defer server.Close()
			post := func(token, body, key string) (int, gormrepo.ReleaseResult) {
				t.Helper()
				req, err := http.NewRequest(http.MethodPost, server.URL+"/api/v1/agents/"+uid+"/releases", bytes.NewBufferString(body))
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("Authorization", "Bearer "+token)
				req.Header.Set("Idempotency-Key", key)
				res, err := server.Client().Do(req)
				if err != nil {
					t.Fatal(err)
				}
				defer res.Body.Close()
				var result gormrepo.ReleaseResult
				if res.StatusCode == http.StatusAccepted {
					if err := json.NewDecoder(res.Body).Decode(&result); err != nil {
						t.Fatal(err)
					}
				}
				return res.StatusCode, result
			}
			body := `{"config_revision_id":"` + revision.ConfigRevisionID + `"}`
			if code, _ := post("admin-b-test-secret-long", body, ""); code != http.StatusNotFound {
				t.Fatalf("another Workspace published Agent: HTTP %d", code)
			}
			if code, _ := post("admin-a-test-secret-long", `{"config_revision_id":"invalid"}`, ""); code != http.StatusBadRequest {
				t.Fatalf("invalid Revision accepted: HTTP %d", code)
			}
			key := "api-" + uid[:16]
			code, release := post("admin-a-test-secret-long", body, key)
			if code != http.StatusAccepted || release.ReleaseID == "" || release.DesiredGeneration != 1 || release.Status != "PENDING" {
				t.Fatalf("release API did not persist Desired: HTTP %d %+v", code, release)
			}
			code, replay := post("admin-a-test-secret-long", body, key)
			if code != http.StatusAccepted || replay.ReleaseID != release.ReleaseID || replay.DesiredGeneration != 1 {
				t.Fatalf("replay generated duplicate Release: HTTP %d %+v", code, replay)
			}
			assignment, err := store.GetAgentAssignment(ctx, workspace, uid)
			if err != nil || assignment.ReleaseID != release.ReleaseID || assignment.DesiredGeneration != 1 ||
				assignment.State != "PENDING" || assignment.DesiredHash != revision.ConfigHash {
				t.Fatalf("API release missing Assignment: %+v %v", assignment, err)
			}
		})
	}
}
