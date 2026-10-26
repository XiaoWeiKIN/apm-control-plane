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
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/XiaoWeiKIN/apm-control-plane/internal/appconfig"
	"github.com/XiaoWeiKIN/apm-control-plane/internal/config"
	"github.com/XiaoWeiKIN/apm-control-plane/internal/storage/gormrepo"
	"github.com/open-telemetry/opamp-go/protobufs"
	"google.golang.org/protobuf/proto"
)

// TestControlPlaneProcessRestartPostgresAndMySQL starts the real server executable,
// not a recreated httptest Gateway, and verifies durable Inventory and Desired
// across a complete process termination and new OpAMP HTTP session.
func TestControlPlaneProcessRestartPostgresAndMySQL(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "control-plane")
	build := exec.Command("go", "build", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build control-plane executable: %v\n%s", err, output)
	}
	for _, tc := range []struct{ driver, env string }{
		{"postgres", "APM_TEST_POSTGRES_DSN"},
		{"mysql", "APM_TEST_MYSQL_DSN"},
	} {
		t.Run(tc.driver, func(t *testing.T) {
			dsn := os.Getenv(tc.env)
			if dsn == "" {
				t.Skip(tc.env + " not configured")
			}
			workspace := "ep001-process-" + tc.driver
			rawUID := make([]byte, 16)
			if _, err := rand.Read(rawUID); err != nil {
				t.Fatal(err)
			}
			uid := hex.EncodeToString(rawUID)
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			addr := listener.Addr().String()
			if err := listener.Close(); err != nil {
				t.Fatal(err)
			}
			baseURL := "http://" + addr
			agentToken := "ep001-agent-process-token-long"
			adminToken := "ep001-admin-process-token-long"
			otherToken := "ep001-other-process-token-long"
			env := append(os.Environ(),
				"APM_CONFIG_FILE=",
				"APM_LISTEN_ADDR="+addr,
				"APM_DB_DRIVER="+tc.driver,
				"APM_DATABASE_DSN="+dsn,
				fmt.Sprintf("APM_AGENT_CREDENTIALS_JSON=[{\"id\":\"agent-owner\",\"workspace_id\":%q,\"token\":%q}]", workspace, agentToken),
				fmt.Sprintf("APM_ADMIN_CREDENTIALS_JSON=[{\"id\":\"admin-owner\",\"workspace_id\":%q,\"token\":%q},{\"id\":\"other-admin\",\"workspace_id\":\"other-workspace\",\"token\":%q}]", workspace, adminToken, otherToken),
			)
			client := &http.Client{Timeout: 2 * time.Second}

			// 1. 运行真实服务器进程，并通过 /healthz 确认完成 DB 初始化和监听。
			start := func() func() {
				t.Helper()
				logPath := filepath.Join(t.TempDir(), "server.log")
				logFile, err := os.Create(logPath)
				if err != nil {
					t.Fatal(err)
				}
				cmd := exec.Command(binary)
				cmd.Env = env
				cmd.Stdout, cmd.Stderr = logFile, logFile
				if err := cmd.Start(); err != nil {
					_ = logFile.Close()
					t.Fatal(err)
				}
				stopped := false
				t.Cleanup(func() {
					if !stopped {
						_ = cmd.Process.Kill()
						_ = cmd.Wait()
						_ = logFile.Close()
					}
				})
				deadline := time.Now().Add(15 * time.Second)
				for time.Now().Before(deadline) {
					resp, err := client.Get(baseURL + "/healthz")
					if err == nil {
						_ = resp.Body.Close()
						if resp.StatusCode == http.StatusOK {
							break
						}
					}
					time.Sleep(30 * time.Millisecond)
				}
				resp, err := client.Get(baseURL + "/healthz")
				if err != nil {
					log, _ := os.ReadFile(logPath)
					t.Fatalf("server not ready: %v\n%s", err, log)
				}
				_ = resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("healthz: HTTP %d", resp.StatusCode)
				}
				return func() {
					t.Helper()
					if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
						t.Fatal(err)
					}
					waited := make(chan error, 1)
					go func() { waited <- cmd.Wait() }()
					select {
					case err := <-waited:
						stopped = true
						_ = logFile.Close()
						if err != nil {
							log, _ := os.ReadFile(logPath)
							t.Fatalf("control plane did not shut down cleanly: %v\n%s", err, log)
						}
					case <-time.After(10 * time.Second):
						_ = cmd.Process.Kill()
						<-waited
						stopped = true
						_ = logFile.Close()
						t.Fatal("control plane did not shut down within 10s")
					}
				}
			}

			send := func(seq uint64, status *protobufs.RemoteConfigStatus) *protobufs.ServerToAgent {
				t.Helper()
				msg := &protobufs.AgentToServer{
					InstanceUid: rawUID, SequenceNum: seq,
					Capabilities: uint64(protobufs.AgentCapabilities_AgentCapabilities_AcceptsRemoteConfig |
						protobufs.AgentCapabilities_AgentCapabilities_ReportsRemoteConfig),
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
				req, err := http.NewRequest(http.MethodPost, baseURL+"/v1/opamp", bytes.NewReader(wire))
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("Authorization", "Bearer "+agentToken)
				req.Header.Set("Content-Type", "application/x-protobuf")
				resp, err := client.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				defer resp.Body.Close()
				body, err := io.ReadAll(resp.Body)
				if err != nil || resp.StatusCode != http.StatusOK {
					t.Fatalf("OpAMP HTTP %d: %v, %s", resp.StatusCode, err, body)
				}
				var reply protobufs.ServerToAgent
				if err := proto.Unmarshal(body, &reply); err != nil {
					t.Fatal(err)
				}
				if reply.ErrorResponse != nil {
					t.Fatalf("OpAMP status rejected: %+v", reply.ErrorResponse)
				}
				return &reply
			}
			getAgent := func(token string, want int) map[string]any {
				t.Helper()
				req, err := http.NewRequest(http.MethodGet, baseURL+"/api/v1/agents/"+uid, nil)
				if err != nil {
					t.Fatal(err)
				}
				if token != "" {
					req.Header.Set("Authorization", "Bearer "+token)
				}
				resp, err := client.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				defer resp.Body.Close()
				if resp.StatusCode != want {
					body, _ := io.ReadAll(resp.Body)
					t.Fatalf("GET Agent HTTP %d want %d: %s", resp.StatusCode, want, body)
				}
				if want != http.StatusOK {
					return nil
				}
				var data map[string]any
				if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
					t.Fatal(err)
				}
				return data
			}

			stop := start()
			if got := send(1, nil); got.RemoteConfig != nil {
				t.Fatal("new Agent received unexpected Desired")
			}
			original := getAgent(adminToken, http.StatusOK)
			if original["workspace_id"] != workspace || original["service_name"] != "checkout" {
				t.Fatalf("unexpected initial persisted Agent: %+v", original)
			}
			getAgent("", http.StatusUnauthorized)
			getAgent(otherToken, http.StatusNotFound)

			// 2. 通过真实管理 API 发布配置，随后终止整个服务器进程。
			db, err := gormrepo.Open(context.Background(), appconfig.Database{
				Driver: tc.driver, DSN: dsn, MaxOpen: 4, MaxIdle: 1, MaxLifetime: time.Minute,
			})
			if err != nil {
				t.Fatal(err)
			}
			snapshot, err := config.CompileJavaSampling(0.3)
			if err != nil {
				t.Fatal(err)
			}
			revision, err := db.CreateConfigRevision(context.Background(), workspace, "admin-owner", snapshot)
			if err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			payload, err := json.Marshal(map[string]string{"config_revision_id": revision.ConfigRevisionID})
			if err != nil {
				t.Fatal(err)
			}
			req, err := http.NewRequest(http.MethodPost, baseURL+"/api/v1/agents/"+uid+"/releases", bytes.NewReader(payload))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer "+adminToken)
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusAccepted {
				t.Fatalf("release HTTP %d, want 202", resp.StatusCode)
			}
			stop()

			// 3. 新进程无运行时 Session，仍可查询原 Inventory 并下发未确认的 Desired。
			stop = start()
			restored := getAgent(adminToken, http.StatusOK)
			if restored["workspace_id"] != workspace || restored["service_name"] != "checkout" {
				t.Fatalf("Agent lost after full process restart: %+v", restored)
			}
			offered := send(1, nil)
			if offered.RemoteConfig == nil || hex.EncodeToString(offered.RemoteConfig.ConfigHash) != revision.ConfigHash {
				t.Fatalf("restarted process did not load persisted Desired: %+v", offered.RemoteConfig)
			}
			if offered.RemoteConfig.Config.GetConfigMap()["apm/sampling.json"] == nil {
				t.Fatal("restarted process returned incomplete ConfigMap")
			}
			if got := send(2, &protobufs.RemoteConfigStatus{
				Status: protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLIED,
				LastRemoteConfigHash: offered.RemoteConfig.ConfigHash,
			}); got.RemoteConfig != nil {
				t.Fatal("matching APPLIED ACK caused duplicate config offer")
			}
			stop()

			// 4. ACK 的持久结果再次从数据库读取，不依赖测试进程内缓存。
			db, err = gormrepo.Open(context.Background(), appconfig.Database{
				Driver: tc.driver, DSN: dsn, MaxOpen: 4, MaxIdle: 1, MaxLifetime: time.Minute,
			})
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			assignment, err := db.GetAgentAssignment(context.Background(), workspace, uid)
			if err != nil || assignment.State != "APPLIED" ||
				assignment.ObservedHash == nil || *assignment.ObservedHash != revision.ConfigHash {
				t.Fatalf("ACK not persisted after process restart: %+v %v", assignment, err)
			}
		})
	}
}
