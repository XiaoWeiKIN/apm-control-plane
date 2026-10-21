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
	"github.com/XiaoWeiKIN/apm-control-plane/internal/storage/gormrepo"
	"github.com/gorilla/websocket"
	"github.com/open-telemetry/opamp-go/protobufs"
	"google.golang.org/protobuf/proto"
)

func TestWebSocketPushAndReconnectPostgresAndMySQL(t *testing.T) {
	for _, tt := range []struct{ driver, env string }{
		{"postgres", "APM_TEST_POSTGRES_DSN"},
		{"mysql", "APM_TEST_MYSQL_DSN"},
	} {
		t.Run(tt.driver, func(t *testing.T) {
			dsn := os.Getenv(tt.env)
			if dsn == "" {
				t.Skip(tt.env + " not set")
			}
			ctx := context.Background()
			store, err := gormrepo.Open(ctx, appconfig.Database{
				Driver: tt.driver, DSN: dsn, MaxOpen: 8, MaxIdle: 2, MaxLifetime: time.Minute,
			})
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			const workspace = "ws-push-contract"
			uidBytes := make([]byte, 16)
			if _, err := rand.Read(uidBytes); err != nil {
				t.Fatal(err)
			}
			uid := hex.EncodeToString(uidBytes)
			reg := newRegistry(store, time.Second)
			reg.delivery = store
			log := slog.New(slog.NewTextHandler(os.Stderr, nil))
			handler, cc, err := makeGateway(reg, auth.New([]appconfig.Credential{
				{ID: "java-credential", WorkspaceID: workspace, Token: "ws-agent-token-long"},
			}), log)
			if err != nil {
				t.Fatal(err)
			}
			mux := http.NewServeMux()
			mux.HandleFunc("/v1/opamp", handler)
			mux.Handle("POST /api/v1/agents/{uid}/releases", adminReleaseHandler(store,
				auth.New([]appconfig.Credential{
					{ID: "admin-credential", WorkspaceID: workspace, Token: "ws-admin-token-long"},
				}), reg.wakeAgent))
			server := httptest.NewUnstartedServer(mux)
			server.Config.ConnContext = cc
			server.Start()
			defer server.Close()
			defer reg.stop()

			connect := func() *websocket.Conn {
				t.Helper()
				wsURL := "ws" + server.URL[len("http"):] + "/v1/opamp"
				conn, response, err := websocket.DefaultDialer.Dial(wsURL,
					http.Header{"Authorization": {"Bearer ws-agent-token-long"}})
				if err != nil {
					t.Fatalf("websocket dial: %v, HTTP response: %v", err, response)
				}
				return conn
			}
			send := func(conn *websocket.Conn, seq uint64, status *protobufs.RemoteConfigStatus) {
				t.Helper()
				msg := &protobufs.AgentToServer{
					InstanceUid: uidBytes, SequenceNum: seq,
					Capabilities: uint64(protobufs.AgentCapabilities_AgentCapabilities_AcceptsRemoteConfig |
						protobufs.AgentCapabilities_AgentCapabilities_ReportsRemoteConfig |
						protobufs.AgentCapabilities_AgentCapabilities_ReportsStatus),
					RemoteConfigStatus: status,
					AgentDescription: &protobufs.AgentDescription{
						IdentifyingAttributes: []*protobufs.KeyValue{
							{Key: "agent.kind", Value: &protobufs.AnyValue{
								Value: &protobufs.AnyValue_StringValue{StringValue: config.JavaAgentKind}}},
						},
					},
				}
				body, err := proto.Marshal(msg)
				if err != nil {
					t.Fatal(err)
				}
				if err := conn.WriteMessage(websocket.BinaryMessage, append([]byte{0}, body...)); err != nil {
					t.Fatal(err)
				}
			}
			receive := func(conn *websocket.Conn, hash string) {
				t.Helper()
				deadline := time.Now().Add(25 * time.Second)
				for {
					if err := conn.SetReadDeadline(deadline); err != nil {
						t.Fatal(err)
					}
					_, wire, err := conn.ReadMessage()
					if err != nil {
						t.Fatalf("websocket push missing: %v", err)
					}
					if len(wire) == 0 || wire[0] != 0 {
						t.Fatalf("unexpected OpAMP WS frame header: %x", wire)
					}
					var reply protobufs.ServerToAgent
					if err := proto.Unmarshal(wire[1:], &reply); err != nil {
						t.Fatal(err)
					}
					if reply.ErrorResponse != nil {
						t.Fatalf("OpAMP rejected message: %v", reply.ErrorResponse)
					}
					if reply.RemoteConfig == nil {
						continue
					}
					if hex.EncodeToString(reply.RemoteConfig.ConfigHash) != hash {
						t.Fatalf("wrong desired Hash: got %x, want %s", reply.RemoteConfig.ConfigHash, hash)
					}
					if reply.RemoteConfig.Config.GetConfigMap()["apm/sampling.json"] == nil {
						t.Fatal("missing full Java ConfigMap")
					}
					return
				}
			}
			publish := func(rate float64) gormrepo.ConfigRevisionRecord {
				t.Helper()
				snapshot, err := config.CompileJavaSampling(rate)
				if err != nil {
					t.Fatal(err)
				}
				revision, err := store.CreateConfigRevision(ctx, workspace, "admin-credential", snapshot)
				if err != nil {
					t.Fatal(err)
				}
				body, err := json.Marshal(map[string]string{"config_revision_id": revision.ConfigRevisionID})
				if err != nil {
					t.Fatal(err)
				}
				req, err := http.NewRequest(http.MethodPost,
					server.URL+"/api/v1/agents/"+uid+"/releases", bytes.NewReader(body))
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("Authorization", "Bearer ws-admin-token-long")
				res, err := server.Client().Do(req)
				if err != nil {
					t.Fatal(err)
				}
				defer res.Body.Close()
				if res.StatusCode != http.StatusAccepted {
					wire, _ := io.ReadAll(res.Body)
					t.Fatalf("release failed: %d %s", res.StatusCode, wire)
				}
				return revision
			}
			assignmentState := func(want string) {
				t.Helper()
				deadline := time.Now().Add(5 * time.Second)
				for time.Now().Before(deadline) {
					item, err := store.GetAgentAssignment(ctx, workspace, uid)
					if err == nil && item.State == want {
						return
					}
					time.Sleep(20 * time.Millisecond)
				}
				item, err := store.GetAgentAssignment(ctx, workspace, uid)
				t.Fatalf("assignment state: got %+v err=%v; want %s", item, err, want)
			}

			// 1. 首次注册没有 Desired。Admin 发布后无须 Agent 再发消息即可收到 WS Push。
			conn := connect()
			send(conn, 1, nil)
			// Register must complete before publishing; read inventory instead of assuming WS processing speed.
			deadline := time.Now().Add(5 * time.Second)
			for {
				_, err := store.Get(ctx, workspace, uid)
				if err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("WS registration failed: %v", err)
				}
				time.Sleep(20 * time.Millisecond)
			}
			first := publish(0.2)
			receive(conn, first.ConfigHash)
			assignmentState("WAITING_ACK")
			send(conn, 2, &protobufs.RemoteConfigStatus{
				Status:               protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLIED,
				LastRemoteConfigHash: mustHash(t, first.ConfigHash),
			})
			assignmentState("APPLIED")

			// 2. 同一 WS 无须 Agent 请求，第二次发布产生新 Hash；旧 ACK 不可覆盖。
			second := publish(0.6)
			receive(conn, second.ConfigHash)
			send(conn, 3, &protobufs.RemoteConfigStatus{
				Status:               protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLIED,
				LastRemoteConfigHash: mustHash(t, first.ConfigHash),
			})
			assignmentState("WAITING_ACK")

			// 3. 离线发布后重启 Gateway 与 Store；新会话只能从数据库恢复 Desired。
			_ = conn.Close()
			third := publish(0.9)
			if err := reg.stop(); err != nil {
				t.Fatal(err)
			}
			server.Close()
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			store, err = gormrepo.Open(ctx, appconfig.Database{
				Driver: tt.driver, DSN: dsn, MaxOpen: 8, MaxIdle: 2, MaxLifetime: time.Minute,
			})
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			reg = newRegistry(store, time.Second)
			reg.delivery = store
			defer reg.stop()
			handler, cc, err = makeGateway(reg, auth.New([]appconfig.Credential{
				{ID: "java-credential", WorkspaceID: workspace, Token: "ws-agent-token-long"},
			}), log)
			if err != nil {
				t.Fatal(err)
			}
			mux = http.NewServeMux()
			mux.HandleFunc("/v1/opamp", handler)
			server = httptest.NewUnstartedServer(mux)
			server.Config.ConnContext = cc
			server.Start()
			defer server.Close()
			restored, err := store.GetAgentAssignment(ctx, workspace, uid)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("restored desired state=%s hash=%s retry=%v", restored.State, restored.DesiredHash, restored.NextRetryAt)
			reconnected := connect()
			defer reconnected.Close()
			send(reconnected, 1, nil)
			receive(reconnected, third.ConfigHash)
			send(reconnected, 2, &protobufs.RemoteConfigStatus{
				Status:               protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLIED,
				LastRemoteConfigHash: mustHash(t, third.ConfigHash),
			})
			assignmentState("APPLIED")
		})
	}
}

func mustHash(t *testing.T, value string) []byte {
	t.Helper()
	decoded, err := hex.DecodeString(value)
	if err != nil {
		t.Fatal(err)
	}
	return decoded
}
