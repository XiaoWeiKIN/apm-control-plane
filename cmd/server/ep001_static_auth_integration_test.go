//go:build integration

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/XiaoWeiKIN/apm-control-plane/internal/appconfig"
	"github.com/XiaoWeiKIN/apm-control-plane/internal/auth"
	"github.com/XiaoWeiKIN/apm-control-plane/internal/config"
	"github.com/XiaoWeiKIN/apm-control-plane/internal/policy"
	"github.com/XiaoWeiKIN/apm-control-plane/internal/storage/gormrepo"
	"github.com/open-telemetry/opamp-go/protobufs"
	"google.golang.org/protobuf/proto"
)

// TestEP001StaticCredentialBoundariesPostgresAndMySQL verifies the current
// scoped-Bearer security contract; it is not a production IAM/revocation test.
func TestEP001StaticCredentialBoundariesPostgresAndMySQL(t *testing.T) {
	for _, tc := range []struct{ driver, env string }{
		{"postgres", "APM_TEST_POSTGRES_DSN"},
		{"mysql", "APM_TEST_MYSQL_DSN"},
	} {
		t.Run(tc.driver, func(t *testing.T) {
			dsn := os.Getenv(tc.env)
			if dsn == "" {
				t.Skip(tc.env + " not configured")
			}
			ctx := context.Background()
			store, err := gormrepo.Open(ctx, appconfig.Database{
				Driver: tc.driver, DSN: dsn, MaxOpen: 8, MaxIdle: 2, MaxLifetime: time.Minute,
			})
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()

			workspace := "ep001-static-auth-" + tc.driver
			owner := auth.Principal{WorkspaceID: workspace, CredentialID: "agent-owner"}
			uids := make([]string, 2)
			rawUIDs := make([][]byte, 2)
			for i := range uids {
				raw := make([]byte, 16)
				if _, err := rand.Read(raw); err != nil {
					t.Fatal(err)
				}
				rawUIDs[i] = raw
				uids[i] = hex.EncodeToString(raw)
				if _, err := store.Observe(ctx, owner, uids[i], gormrepo.Observation{
					SeenAt: time.Now().UTC(), Transport: "http",
					Capabilities: uint64(protobufs.AgentCapabilities_AgentCapabilities_AcceptsRemoteConfig),
					Description: &gormrepo.Description{
						Kind: config.JavaAgentKind, Service: "checkout", Environment: "prod",
					},
				}); err != nil {
					t.Fatal(err)
				}
			}
			snapshot, err := config.CompileJavaSampling(0.3)
			if err != nil {
				t.Fatal(err)
			}
			revision, err := store.CreateConfigRevision(ctx, workspace, "admin-owner", snapshot)
			if err != nil {
				t.Fatal(err)
			}
			createdPolicy, policyRevision, err := store.CreatePolicy(ctx, workspace, "admin-owner", "role-boundary", 0.4)
			if err != nil {
				t.Fatal(err)
			}
			binding, err := store.CreatePolicyBinding(ctx, workspace, "admin-owner", gormrepo.BindingInput{
				PolicyRevisionID: policyRevision.PolicyRevisionID,
				ScopeType:        policy.ScopeService, Selector: policy.Selector{ServiceName: "checkout"},
				Priority: 10,
			})
			if err != nil {
				t.Fatal(err)
			}
			rollout, err := store.PublishRollout(ctx, workspace, "admin-owner",
				revision.ConfigRevisionID, uids, 1, "")
			if err != nil {
				t.Fatal(err)
			}

			// 1. 使用生产装配的相同两套 Authenticator 和管理 API 路由。
			const agentToken = "ep001-agent-token-long"
			const adminToken = "ep001-admin-token-long"
			const otherAdminToken = "ep001-other-admin-token-long"
			adminAuth := auth.New([]appconfig.Credential{
				{ID: "admin-owner", WorkspaceID: workspace, Token: adminToken},
				{ID: "other-admin", WorkspaceID: "ep001-foreign", Token: otherAdminToken},
			})
			agentAuth := auth.New([]appconfig.Credential{
				{ID: owner.CredentialID, WorkspaceID: workspace, Token: agentToken},
			})
			reg := newRegistry(store, time.Second)
			reg.delivery = store
			gateway, connCtx, err := makeGateway(reg, agentAuth,
				slog.New(slog.NewTextHandler(io.Discard, nil)))
			if err != nil {
				t.Fatal(err)
			}
			mux := http.NewServeMux()
			mux.HandleFunc("/v1/opamp", gateway)
			mux.Handle("POST /api/v1/agents/{uid}/releases", adminReleaseHandler(store, adminAuth, nil))
			mux.Handle("POST /api/v1/agents/{uid}/policy-releases", adminPolicyReleaseHandler(store, adminAuth, nil))
			rolloutHandler := adminRolloutHandler(store, adminAuth, nil)
			mux.Handle("POST /api/v1/rollouts", rolloutHandler)
			mux.Handle("POST /api/v1/policy-rollouts", rolloutHandler)
			mux.Handle("POST /api/v1/rollouts/{release_id}/advance", rolloutHandler)
			mux.Handle("POST /api/v1/rollouts/{release_id}/resume", rolloutHandler)
			mux.Handle("POST /api/v1/rollouts/{release_id}/rollback", rolloutHandler)
			mux.Handle("GET /api/v1/rollouts/{release_id}", rolloutHandler)
			policyHandler := policyAPI(store, adminAuth)
			mux.Handle("POST /api/v1/policies", policyHandler)
			mux.Handle("POST /api/v1/policies/{policy_id}/revisions", policyHandler)
			mux.Handle("POST /api/v1/policy-bindings", policyHandler)
			mux.Handle("PUT /api/v1/policy-bindings/{binding_id}", policyHandler)
			mux.Handle("GET /api/v1/agents/{uid}/policy-resolution", policyHandler)
			mux.Handle("/api/", adminHandler(reg, adminAuth))
			server := httptest.NewUnstartedServer(mux)
			server.Config.ConnContext = connCtx
			server.Start()
			defer server.Close()
			defer reg.stop()

			call := func(method, path, token, body string, expected int) []byte {
				t.Helper()
				req, err := http.NewRequest(method, server.URL+path, bytes.NewBufferString(body))
				if err != nil {
					t.Fatal(err)
				}
				if token != "" {
					req.Header.Set("Authorization", "Bearer "+token)
				}
				resp, err := server.Client().Do(req)
				if err != nil {
					t.Fatal(err)
				}
				defer resp.Body.Close()
				data, err := io.ReadAll(resp.Body)
				if err != nil {
					t.Fatal(err)
				}
				if resp.StatusCode != expected {
					t.Fatalf("%s %s: HTTP %d want %d: %s", method, path, resp.StatusCode, expected, data)
				}
				return data
			}

			// 2. 对所有受保护路由验证缺少凭证、Agent 凭证与跨 Workspace Admin。
			bindingJSON := fmt.Sprintf(
				`{"policy_revision_id":%q,"scope_type":"SERVICE","selector":{"service_name":"checkout"},"priority":10}`,
				policyRevision.PolicyRevisionID)
			updateJSON := fmt.Sprintf(
				`{"policy_revision_id":%q,"scope_type":"SERVICE","selector":{"service_name":"checkout"},"priority":10,"enabled":false,"row_version":%d}`,
				policyRevision.PolicyRevisionID, binding.RowVersion)
			rolloutJSON := fmt.Sprintf(
				`{"config_revision_id":%q,"instance_uids":[%q,%q],"batch_size":1}`,
				revision.ConfigRevisionID, uids[0], uids[1])
			rollbackJSON := fmt.Sprintf(
				`{"config_revision_id":%q,"row_version":%d,"batch_size":1}`,
				revision.ConfigRevisionID, rollout.RowVersion)
			policyJSON := fmt.Sprintf(`{"policy_revision_id":%q}`, policyRevision.PolicyRevisionID)
			type endpoint struct {
				name, method, path, body string
				foreignStatus            int
			}
			paths := []endpoint{
				{"list-agents", "GET", "/api/v1/agents", "", http.StatusOK},
				{"get-agent", "GET", "/api/v1/agents/" + uids[0], "", http.StatusNotFound},
				{"legacy-sampling", "PUT", "/api/v1/agents/" + uids[0] + "/sampling", `{"sampling_rate":0.2}`, http.StatusNotFound},
				{"direct-release", "POST", "/api/v1/agents/" + uids[0] + "/releases",
					fmt.Sprintf(`{"config_revision_id":%q}`, revision.ConfigRevisionID), http.StatusNotFound},
				{"policy-release", "POST", "/api/v1/agents/" + uids[0] + "/policy-releases", policyJSON, http.StatusNotFound},
				{"create-policy", "POST", "/api/v1/policies", `{"name":"unauthorized","sampling_rate":0.2}`, 0},
				{"policy-revision", "POST", "/api/v1/policies/" + createdPolicy.PolicyID + "/revisions",
					`{"sampling_rate":0.2}`, http.StatusNotFound},
				{"create-binding", "POST", "/api/v1/policy-bindings", bindingJSON, http.StatusNotFound},
				{"update-binding", "PUT", "/api/v1/policy-bindings/" + binding.BindingID, updateJSON, http.StatusNotFound},
				{"policy-resolution", "GET", "/api/v1/agents/" + uids[0] + "/policy-resolution", "", http.StatusNotFound},
				{"create-rollout", "POST", "/api/v1/rollouts", rolloutJSON, http.StatusNotFound},
				{"policy-rollout", "POST", "/api/v1/policy-rollouts",
					fmt.Sprintf(`{"policy_revision_id":%q,"service_name":"checkout","batch_size":1}`, policyRevision.PolicyRevisionID),
					http.StatusNotFound},
				{"get-rollout", "GET", "/api/v1/rollouts/" + rollout.ReleaseID, "", http.StatusNotFound},
				{"advance-rollout", "POST", "/api/v1/rollouts/" + rollout.ReleaseID + "/advance", "", http.StatusNotFound},
				{"resume-rollout", "POST", "/api/v1/rollouts/" + rollout.ReleaseID + "/resume",
					fmt.Sprintf(`{"row_version":%d}`, rollout.RowVersion), http.StatusNotFound},
				{"rollback", "POST", "/api/v1/rollouts/" + rollout.ReleaseID + "/rollback", rollbackJSON, http.StatusNotFound},
			}
			for _, ep := range paths {
				t.Run(ep.name, func(t *testing.T) {
					call(ep.method, ep.path, "", ep.body, http.StatusUnauthorized)
					call(ep.method, ep.path, agentToken, ep.body, http.StatusUnauthorized)
					if ep.foreignStatus != 0 {
						raw := call(ep.method, ep.path, otherAdminToken, ep.body, ep.foreignStatus)
						if ep.name == "list-agents" {
							var agents []any
							if err := json.Unmarshal(raw, &agents); err != nil || len(agents) != 0 {
								t.Fatalf("foreign Workspace returned Agent inventory: %s %v", raw, err)
							}
						}
					}
				})
			}

			// 3. Admin Token 不能认证 OpAMP Agent 连接或冒充已存在 UID。
			opamp, err := proto.Marshal(&protobufs.AgentToServer{InstanceUid: rawUIDs[0], SequenceNum: 1})
			if err != nil {
				t.Fatal(err)
			}
			req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/opamp", bytes.NewReader(opamp))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer "+adminToken)
			req.Header.Set("Content-Type", "application/x-protobuf")
			resp, err := server.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("admin token authenticated as Agent: HTTP %d", resp.StatusCode)
			}

			// 4. 所有拒绝之后验证原冻结 Release/Desired 与策略内容均未被修改。
			view, err := store.GetRollout(ctx, workspace, rollout.ReleaseID)
			if err != nil || view.Status != "RUNNING" || view.RowVersion != rollout.RowVersion {
				t.Fatalf("rejected requests changed rollout: %+v %v", view, err)
			}
			for i, target := range view.Targets {
				want := "QUEUED"
				if i == 0 {
					want = "PENDING"
				}
				if target.Status != want || target.ConfigRevisionID != revision.ConfigRevisionID {
					t.Fatalf("rejected requests mutated target[%d]: %+v", i, target)
				}
			}
			current, err := store.GetAgentAssignment(ctx, workspace, rollout.Activated[0])
			if err != nil || current.ReleaseID != rollout.ReleaseID || current.DesiredGeneration != 1 {
				t.Fatalf("rejected requests changed Desired: %+v %v", current, err)
			}
			if got := call("GET", "/api/v1/agents/"+uids[0], adminToken, "", http.StatusOK); len(got) == 0 {
				t.Fatal("authorized Admin lost Agent read")
			}
			if got := call("GET", "/api/v1/rollouts/"+rollout.ReleaseID, adminToken, "", http.StatusOK); len(got) == 0 {
				t.Fatal("authorized Admin lost Release read")
			}
		})
	}
}
