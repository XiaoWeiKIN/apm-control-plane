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
	"github.com/XiaoWeiKIN/apm-control-plane/internal/storage/gormrepo"
	"github.com/open-telemetry/opamp-go/protobufs"
	"google.golang.org/protobuf/proto"
)

func TestAgentRegistrationThroughOpAMPToDatabaseAndAPI(t *testing.T) {
	tests := []struct{ driver, env string }{
		{"postgres", "APM_TEST_POSTGRES_DSN"},
		{"mysql", "APM_TEST_MYSQL_DSN"},
	}
	for _, tt := range tests {
		t.Run(tt.driver, func(t *testing.T) {
			dsn := os.Getenv(tt.env)
			if dsn == "" {
				t.Skip(tt.env + " not set")
			}
			ctx := context.Background()
			cfg := appconfig.Database{Driver: tt.driver, DSN: dsn, MaxOpen: 4, MaxIdle: 1, MaxLifetime: time.Minute}
			store, err := gormrepo.Open(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			reg := newRegistry(store, 1*time.Millisecond)
			agentAuth := auth.New([]appconfig.Credential{{ID: "agent-a", TenantID: "team-one", Token: "long-agent-secret-for-test"}})
			adminAuth := auth.New([]appconfig.Credential{
				{ID: "admin-a", TenantID: "team-one", Token: "long-admin-secret-for-test"},
				{ID: "admin-b", TenantID: "team-two", Token: "other-long-admin-token"},
			})
			handler, connContext, err := makeGateway(reg, agentAuth, slog.New(slog.NewTextHandler(io.Discard, nil)))
			if err != nil {
				t.Fatal(err)
			}
			mux := http.NewServeMux()
			mux.HandleFunc("/v1/opamp", handler)
			mux.Handle("/api/", adminHandler(reg, adminAuth))
			server := httptest.NewUnstartedServer(mux)
			server.Config.ConnContext = connContext
			server.Start()
			defer server.Close()
			rawUID := make([]byte, 16)
			if _, err := rand.Read(rawUID); err != nil {
				t.Fatal(err)
			}
			uid := hex.EncodeToString(rawUID)
			msg := &protobufs.AgentToServer{
				InstanceUid: rawUID, SequenceNum: 1,
				Capabilities: uint64(protobufs.AgentCapabilities_AgentCapabilities_ReportsRemoteConfig),
				AgentDescription: &protobufs.AgentDescription{IdentifyingAttributes: []*protobufs.KeyValue{
					{Key: "service.name", Value: &protobufs.AnyValue{Value: &protobufs.AnyValue_StringValue{StringValue: "payments"}}},
					{Key: "agent.kind", Value: &protobufs.AnyValue{Value: &protobufs.AnyValue_StringValue{StringValue: "opamp-test-agent"}}},
					{Key: "telemetry.distro.version", Value: &protobufs.AnyValue{Value: &protobufs.AnyValue_StringValue{StringValue: "0.1.0"}}},
				}},
			}
			payload, err := proto.Marshal(msg)
			if err != nil {
				t.Fatal(err)
			}
			request, err := http.NewRequest(http.MethodPost, server.URL+"/v1/opamp", bytes.NewReader(payload))
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Authorization", "Bearer long-agent-secret-for-test")
			request.Header.Set("Content-Type", "application/x-protobuf")
			response, err := server.Client().Do(request)
			if err != nil {
				t.Fatal(err)
			}
			wire, err := io.ReadAll(response.Body)
			_ = response.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != http.StatusOK {
				t.Fatalf("OpAMP HTTP status: %d %s", response.StatusCode, wire)
			}
			var reply protobufs.ServerToAgent
			if err := proto.Unmarshal(wire, &reply); err != nil {
				t.Fatalf("OpAMP response decode: %v", err)
			}

			get := func(token string) (int, map[string]any) {
				t.Helper()
				req, err := http.NewRequest(http.MethodGet, server.URL+"/api/v1/agents/"+uid, nil)
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("Authorization", "Bearer "+token)
				res, err := server.Client().Do(req)
				if err != nil {
					t.Fatal(err)
				}
				defer res.Body.Close()
				var body map[string]any
				if res.StatusCode == 200 {
					if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
						t.Fatal(err)
					}
				}
				return res.StatusCode, body
			}
			code, state := get("long-admin-secret-for-test")
			if code != http.StatusOK || state["service_name"] != "payments" || state["agent_kind"] != "opamp-test-agent" {
				t.Fatalf("API did not reflect OpAMP registration: %d %+v", code, state)
			}
			if code, _ := get("other-long-admin-token"); code != http.StatusNotFound {
				t.Fatalf("cross tenant API leaked Agent: %d", code)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := gormrepo.Open(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			persisted, err := reopened.Get(ctx, "team-one", uid)
			if err != nil || persisted.ServiceName != "payments" || persisted.DistributionVersion != "0.1.0" || persisted.LastSeen.IsZero() {
				t.Fatalf("restart recovery failed: %+v %v", persisted, err)
			}
		})
	}
}
