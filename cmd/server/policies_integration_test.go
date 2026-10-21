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

func TestPolicyAPIWorkspaceAndDryRunPostgresAndMySQL(t *testing.T) {
	for _, tt := range []struct{ driver, env string }{
		{"postgres", "APM_TEST_POSTGRES_DSN"},
		{"mysql", "APM_TEST_MYSQL_DSN"},
	} {
		t.Run(tt.driver, func(t *testing.T) {
			dsn := os.Getenv(tt.env)
			if dsn == "" {
				t.Skip(tt.env + " is not set")
			}
			ctx := context.Background()
			store, err := gormrepo.Open(ctx, appconfig.Database{
				Driver: tt.driver, DSN: dsn, MaxOpen: 8, MaxIdle: 2, MaxLifetime: time.Minute,
			})
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			uidBytes := make([]byte, 16)
			if _, err := rand.Read(uidBytes); err != nil {
				t.Fatal(err)
			}
			uid := hex.EncodeToString(uidBytes)
			if _, err := store.Observe(ctx, auth.Principal{WorkspaceID: "policy-http-a", CredentialID: "java-agent"}, uid, gormrepo.Observation{
				SeenAt: time.Now().UTC(), Transport: "http",
				Capabilities: uint64(protobufs.AgentCapabilities_AgentCapabilities_AcceptsRemoteConfig),
				Description:  &gormrepo.Description{Kind: config.JavaAgentKind, Environment: "prod", Service: "checkout"},
			}); err != nil {
				t.Fatal(err)
			}
			authenticator := auth.New([]appconfig.Credential{
				{ID: "admin-a", WorkspaceID: "policy-http-a", Token: "admin-policy-a-secret-long"},
				{ID: "admin-b", WorkspaceID: "policy-http-b", Token: "admin-policy-b-secret-long"},
			})
			h := policyAPI(store, authenticator)
			mux := http.NewServeMux()
			mux.Handle("POST /api/v1/policies", h)
			mux.Handle("POST /api/v1/policies/{policy_id}/revisions", h)
			mux.Handle("POST /api/v1/policy-bindings", h)
			mux.Handle("PUT /api/v1/policy-bindings/{binding_id}", h)
			mux.Handle("GET /api/v1/agents/{uid}/policy-resolution", h)
			server := httptest.NewServer(mux)
			defer server.Close()
			request := func(method, path, token, body string, expected int) []byte {
				t.Helper()
				req, err := http.NewRequest(method, server.URL+path, bytes.NewBufferString(body))
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("Authorization", "Bearer "+token)
				resp, err := server.Client().Do(req)
				if err != nil {
					t.Fatal(err)
				}
				defer resp.Body.Close()
				raw, err := io.ReadAll(resp.Body)
				if err != nil {
					t.Fatal(err)
				}
				if resp.StatusCode != expected {
					t.Fatalf("%s %s: expected %d got %d: %s", method, path, expected, resp.StatusCode, raw)
				}
				return raw
			}
			if body := request("GET", "/api/v1/agents/"+uid+"/policy-resolution", "bad-token", "", 401); len(body) == 0 {
				t.Fatal("unauthorized result should return an error")
			}
			request("GET", "/api/v1/agents/"+uid+"/policy-resolution", "admin-policy-b-secret-long", "", 404)
			raw := request("POST", "/api/v1/policies", "admin-policy-a-secret-long",
				`{"name":"checkout-prod","sampling_rate":0.15}`, 201)
			var created struct {
				Policy   gormrepo.PolicyRecord         `json:"policy"`
				Revision gormrepo.PolicyRevisionRecord `json:"revision"`
			}
			if err := json.Unmarshal(raw, &created); err != nil {
				t.Fatal(err)
			}
			if created.Policy.PolicyID == "" || created.Revision.RevisionNumber != 1 {
				t.Fatalf("bad policy creation: %s", raw)
			}
			request("POST", "/api/v1/policies/"+created.Policy.PolicyID+"/revisions",
				"admin-policy-b-secret-long", `{"sampling_rate":0.3}`, 404)
			r := request("POST", "/api/v1/policy-bindings", "admin-policy-a-secret-long",
				`{"policy_revision_id":"`+created.Revision.PolicyRevisionID+`","scope_type":"SERVICE","selector":{"service_name":"checkout","environment":"prod"},"priority":9}`, 201)
			request("POST", "/api/v1/policy-bindings", "admin-policy-a-secret-long",
				`{"policy_revision_id":"`+created.Revision.PolicyRevisionID+`","scope_type":"WORKSPACE","selector":{},"priority":0,"enabled":false}`, 400)
			var binding gormrepo.PolicyBindingRecord
			if err := json.Unmarshal(r, &binding); err != nil {
				t.Fatal(err)
			}
			if binding.BindingID == "" || !binding.Enabled {
				t.Fatalf("binding not enabled: %+v", binding)
			}
			request("POST", "/api/v1/policy-bindings", "admin-policy-a-secret-long",
				`{"policy_revision_id":"`+created.Revision.PolicyRevisionID+`","scope_type":"ENVIRONMENT","selector":{"service_name":"checkout"},"priority":9}`, 400)
			resolved := request("GET", "/api/v1/agents/"+uid+"/policy-resolution", "admin-policy-a-secret-long", "", 200)
			var resolution gormrepo.PolicyResolution
			if err := json.Unmarshal(resolved, &resolution); err != nil {
				t.Fatal(err)
			}
			if resolution.PolicyRevisionID != created.Revision.PolicyRevisionID || resolution.BindingID != binding.BindingID {
				t.Fatalf("incorrect resolution: %s", resolved)
			}
			if _, err := store.GetAgentAssignment(ctx, "policy-http-a", uid); err != gormrepo.ErrNotFound {
				t.Fatalf("dry-run created an Assignment: %v", err)
			}
			request("PUT", "/api/v1/policy-bindings/"+binding.BindingID, "admin-policy-a-secret-long",
				`{"policy_revision_id":"`+created.Revision.PolicyRevisionID+`","scope_type":"SERVICE","selector":{"service_name":"checkout","environment":"prod"},"priority":9,"enabled":false,"row_version":1}`, 200)
			request("GET", "/api/v1/agents/"+uid+"/policy-resolution", "admin-policy-a-secret-long", "", 404)
		})
	}
}
