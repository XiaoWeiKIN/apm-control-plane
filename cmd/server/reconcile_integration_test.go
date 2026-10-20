//go:build integration

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
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
	"github.com/XiaoWeiKIN/apm-control-plane/internal/storage/gormrepo"
	"github.com/open-telemetry/opamp-go/protobufs"
	"google.golang.org/protobuf/proto"
)

func TestDurableRemoteConfigHTTPAckPostgresAndMySQL(t *testing.T) {
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
			workspace := "remote-config-http"
			rawUID := make([]byte, 16)
			if _, err := rand.Read(rawUID); err != nil {
				t.Fatal(err)
			}
			uid := hex.EncodeToString(rawUID)
			p := auth.Principal{WorkspaceID: workspace, CredentialID: "java-owner"}
			reg := newRegistry(store, time.Second)
			reg.delivery = store
			gateway, connContext, err := makeGateway(reg, auth.New([]appconfig.Credential{
				{ID: p.CredentialID, WorkspaceID: workspace, Token: "p23-agent-token-long"},
			}), slog.New(slog.NewTextHandler(io.Discard, nil)))
			if err != nil {
				t.Fatal(err)
			}
			mux := http.NewServeMux()
			mux.HandleFunc("/v1/opamp", gateway)
			server := httptest.NewUnstartedServer(mux)
			server.Config.ConnContext = connContext
			server.Start()
			defer server.Close()
			capabilities := uint64(
				protobufs.AgentCapabilities_AgentCapabilities_AcceptsRemoteConfig |
					protobufs.AgentCapabilities_AgentCapabilities_ReportsRemoteConfig |
					protobufs.AgentCapabilities_AgentCapabilities_ReportsStatus)
			send := func(seq uint64, status *protobufs.RemoteConfigStatus) *protobufs.ServerToAgent {
				t.Helper()
				msg := &protobufs.AgentToServer{
					InstanceUid: rawUID, SequenceNum: seq, Capabilities: capabilities,
					RemoteConfigStatus: status,
					AgentDescription: &protobufs.AgentDescription{IdentifyingAttributes: []*protobufs.KeyValue{
						{Key: "agent.kind", Value: &protobufs.AnyValue{Value: &protobufs.AnyValue_StringValue{StringValue: config.JavaAgentKind}}},
						{Key: "service.name", Value: &protobufs.AnyValue{Value: &protobufs.AnyValue_StringValue{StringValue: "checkout"}}},
					}},
				}
				wire, err := proto.Marshal(msg)
				if err != nil {
					t.Fatal(err)
				}
				req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/opamp", bytes.NewReader(wire))
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("Authorization", "Bearer p23-agent-token-long")
				req.Header.Set("Content-Type", "application/x-protobuf")
				res, err := server.Client().Do(req)
				if err != nil {
					t.Fatal(err)
				}
				defer res.Body.Close()
				body, err := io.ReadAll(res.Body)
				if err != nil || res.StatusCode != http.StatusOK {
					t.Fatalf("OpAMP status=%d body=%s error=%v", res.StatusCode, body, err)
				}
				var reply protobufs.ServerToAgent
				if err := proto.Unmarshal(body, &reply); err != nil {
					t.Fatal(err)
				}
				if reply.ErrorResponse != nil {
					t.Fatalf("OpAMP error: %v", reply.ErrorResponse)
				}
				return &reply
			}
			if got := send(1, nil); got.RemoteConfig != nil {
				t.Fatal("unassigned Agent received config")
			}
			snapshot1, err := config.CompileJavaSampling(0.2)
			if err != nil {
				t.Fatal(err)
			}
			rev1, err := store.CreateConfigRevision(ctx, workspace, "admin", snapshot1)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.PublishAgentConfig(ctx, workspace, uid, rev1.ConfigRevisionID, "admin", ""); err != nil {
				t.Fatal(err)
			}
			got1 := send(2, nil).RemoteConfig
			if got1 == nil || hex.EncodeToString(got1.ConfigHash) != rev1.ConfigHash ||
				!proto.Equal(got1.Config, mustConfigMap(t, snapshot1.ConfigMapBytes())) {
				t.Fatalf("remote config did not match immutable revision: %+v", got1)
			}
			if got := send(3, nil); got.RemoteConfig != nil {
				t.Fatal("unacknowledged config offered repeatedly before retry")
			}
			applied1 := &protobufs.RemoteConfigStatus{
				Status:               protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLIED,
				LastRemoteConfigHash: got1.ConfigHash,
			}
			if got := send(4, applied1); got.RemoteConfig != nil {
				t.Fatal("APPLIED config was re-offered")
			}
			a, err := store.GetAgentAssignment(ctx, workspace, uid)
			if err != nil || a.State != "APPLIED" || a.ObservedHash == nil || *a.ObservedHash != rev1.ConfigHash {
				t.Fatalf("ACK not persisted: %+v %v", a, err)
			}
			// A later contradictory Agent report invalidates the prior applied state.
			if got := send(5, &protobufs.RemoteConfigStatus{
				Status:               protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLIED,
				LastRemoteConfigHash: []byte{0x01},
			}); got.RemoteConfig != nil {
				t.Fatal("unexpected config before retry deadline")
			}
			a, err = store.GetAgentAssignment(ctx, workspace, uid)
			if err != nil || a.State != "PENDING" {
				t.Fatalf("regressed Agent report left stale APPLIED state: %+v %v", a, err)
			}
			snapshot2, err := config.CompileJavaSampling(0.6)
			if err != nil {
				t.Fatal(err)
			}
			rev2, err := store.CreateConfigRevision(ctx, workspace, "admin", snapshot2)
			if err != nil {
				t.Fatal(err)
			}
			rel2, err := store.PublishAgentConfig(ctx, workspace, uid, rev2.ConfigRevisionID, "admin", "")
			if err != nil {
				t.Fatal(err)
			}
			got2 := send(6, applied1).RemoteConfig
			if got2 == nil || hex.EncodeToString(got2.ConfigHash) != rev2.ConfigHash {
				t.Fatalf("stale ACK prevented latest ConfigRevision: %+v", got2)
			}
			a, err = store.GetAgentAssignment(ctx, workspace, uid)
			if err != nil || a.State != "WAITING_ACK" || a.DesiredGeneration != rel2.DesiredGeneration ||
				a.ObservedHash == nil || *a.ObservedHash != rev1.ConfigHash {
				t.Fatalf("stale ACK overwrote new release: %+v %v", a, err)
			}
			applied2 := &protobufs.RemoteConfigStatus{
				Status:               protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLIED,
				LastRemoteConfigHash: got2.ConfigHash,
			}
			send(7, applied2)
			a, err = store.GetAgentAssignment(ctx, workspace, uid)
			if err != nil || a.State != "APPLIED" || a.DesiredGeneration != rel2.DesiredGeneration {
				t.Fatalf("latest ACK not converged: %+v %v", a, err)
			}
			if config, assigned, err := store.ReconcileAgentConfig(ctx, "different-workspace", uid, nil, true); err != nil || assigned || config != nil {
				t.Fatalf("cross-workspace config leaked: %+v %t %v", config, assigned, err)
			}
		})
	}
}

func mustConfigMap(t *testing.T, data []byte) *protobufs.AgentConfigMap {
	t.Helper()
	var result protobufs.AgentConfigMap
	if err := proto.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	return &result
}
