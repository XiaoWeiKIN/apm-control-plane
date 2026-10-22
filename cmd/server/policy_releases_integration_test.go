//go:build integration

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
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

func TestPolicyReleaseOpAMPHTTPPostgresAndMySQL(t *testing.T) {
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
			workspace := "p25-api-workspace"
			rawUID := make([]byte, 16)
			if _, err := rand.Read(rawUID); err != nil {
				t.Fatal(err)
			}
			uid := hex.EncodeToString(rawUID)
			agentAuth := auth.New([]appconfig.Credential{{
				ID: "java-agent", WorkspaceID: workspace, Token: "p25-agent-token-long",
			}})
			adminAuth := auth.New([]appconfig.Credential{
				{ID: "admin-a", WorkspaceID: workspace, Token: "p25-admin-token-long"},
				{ID: "admin-b", WorkspaceID: "another-workspace", Token: "p25-other-admin-token-long"},
			})
			reg := newRegistry(store, time.Second)
			reg.delivery = store
			handler, connectionContext, err := makeGateway(reg, agentAuth, slog.New(slog.NewTextHandler(io.Discard, nil)))
			if err != nil {
				t.Fatal(err)
			}
			mux := http.NewServeMux()
			mux.HandleFunc("/v1/opamp", handler)
			mux.Handle("POST /api/v1/agents/{uid}/policy-releases",
				adminPolicyReleaseHandler(store, adminAuth, reg.wakeAgent))
			server := httptest.NewUnstartedServer(mux)
			server.Config.ConnContext = connectionContext
			server.Start()
			defer server.Close()
			defer reg.stop()

			send := func(seq uint64, status *protobufs.RemoteConfigStatus) *protobufs.ServerToAgent {
				t.Helper()
				msg := &protobufs.AgentToServer{
					InstanceUid: rawUID, SequenceNum: seq,
					Capabilities: uint64(protobufs.AgentCapabilities_AgentCapabilities_AcceptsRemoteConfig |
						protobufs.AgentCapabilities_AgentCapabilities_ReportsRemoteConfig |
						protobufs.AgentCapabilities_AgentCapabilities_ReportsStatus),
					RemoteConfigStatus: status,
					AgentDescription: &protobufs.AgentDescription{IdentifyingAttributes: []*protobufs.KeyValue{
						{Key: "agent.kind", Value: &protobufs.AnyValue{Value: &protobufs.AnyValue_StringValue{StringValue: config.JavaAgentKind}}},
						{Key: "service.name", Value: &protobufs.AnyValue{Value: &protobufs.AnyValue_StringValue{StringValue: "checkout"}}},
					}},
				}
				payload, err := proto.Marshal(msg)
				if err != nil {
					t.Fatal(err)
				}
				req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/opamp", bytes.NewReader(payload))
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("Authorization", "Bearer p25-agent-token-long")
				req.Header.Set("Content-Type", "application/x-protobuf")
				resp, err := server.Client().Do(req)
				if err != nil {
					t.Fatal(err)
				}
				defer resp.Body.Close()
				wire, err := io.ReadAll(resp.Body)
				if err != nil || resp.StatusCode != http.StatusOK {
					t.Fatalf("OpAMP response HTTP %d: %s %v", resp.StatusCode, wire, err)
				}
				var reply protobufs.ServerToAgent
				if err := proto.Unmarshal(wire, &reply); err != nil {
					t.Fatal(err)
				}
				if reply.ErrorResponse != nil {
					t.Fatalf("OpAMP processing rejected: %s", reply.ErrorResponse.Type)
				}
				return &reply
			}
			if got := send(1, nil); got.RemoteConfig != nil {
				t.Fatal("unassigned Agent received config")
			}

			_, revision, err := store.CreatePolicy(ctx, workspace, "admin-a", "checkout", 0.35)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.CreatePolicyBinding(ctx, workspace, "admin-a", gormrepo.BindingInput{
				PolicyRevisionID: revision.PolicyRevisionID, ScopeType: policy.ScopeService,
				Selector: policy.Selector{ServiceName: "checkout"}, Priority: 10,
			}); err != nil {
				t.Fatal(err)
			}
			body, err := json.Marshal(map[string]string{"policy_revision_id": revision.PolicyRevisionID})
			if err != nil {
				t.Fatal(err)
			}
			post := func(token string, contents []byte, want int) gormrepo.ReleaseResult {
				t.Helper()
				req, err := http.NewRequest(http.MethodPost, server.URL+"/api/v1/agents/"+uid+"/policy-releases",
					bytes.NewReader(contents))
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("Authorization", "Bearer "+token)
				req.Header.Set("Idempotency-Key", "p25-api-"+uid[:16])
				resp, err := server.Client().Do(req)
				if err != nil {
					t.Fatal(err)
				}
				defer resp.Body.Close()
				if resp.StatusCode != want {
					text, _ := io.ReadAll(resp.Body)
					t.Fatalf("policy publish returned HTTP %d, want %d: %s", resp.StatusCode, want, text)
				}
				var release gormrepo.ReleaseResult
				if want == http.StatusAccepted {
					if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
						t.Fatal(err)
					}
				}
				return release
			}
			post("p25-other-admin-token-long", body, http.StatusNotFound)
			post("p25-admin-token-long", []byte(`{"policy_revision_id":"invalid"}`), http.StatusBadRequest)
			release := post("p25-admin-token-long", body, http.StatusAccepted)
			if release.ReleaseID == "" || release.DesiredGeneration != 1 {
				t.Fatalf("unexpected release: %+v", release)
			}
			again := post("p25-admin-token-long", body, http.StatusAccepted)
			if again.ConfigRevisionID != release.ConfigRevisionID || again.ReleaseID != release.ReleaseID {
				t.Fatalf("HTTP idempotency did not preserve release: %+v %+v", release, again)
			}
			remote := send(2, nil).RemoteConfig
			if remote == nil {
				t.Fatal("HTTP OpAMP did not deliver Policy-derived ConfigMap")
			}
			saved, err := store.GetConfigRevision(ctx, workspace, release.ConfigRevisionID)
			if err != nil || !bytes.Equal(remote.ConfigHash, mustHash(t, saved.ConfigHash)) ||
				remote.Config.GetConfigMap()["apm/sampling.json"] == nil {
				t.Fatalf("OpAMP ConfigMap/hash differs from durable policy compilation: %+v %v", remote, err)
			}
			applied := &protobufs.RemoteConfigStatus{
				Status: protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLIED,
				LastRemoteConfigHash: remote.ConfigHash,
			}
			send(3, applied)
			state, err := store.GetAgentAssignment(ctx, workspace, uid)
			if err != nil || state.State != "APPLIED" || state.ReleaseID != release.ReleaseID {
				t.Fatalf("policy release did not converge: %+v %v", state, err)
			}
		})
	}
}
