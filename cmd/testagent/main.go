// Test agent: OpAMP HTTP polling and remote-config ACK. It does not run a JVM.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/open-telemetry/opamp-go/protobufs"
	"google.golang.org/protobuf/proto"
)

func main() {
	endpoint := flag.String("endpoint", "http://127.0.0.1:4320/v1/opamp", "OpAMP HTTP endpoint")
	service := flag.String("service", "checkout", "service.name for test agent")
	interval := flag.Duration("interval", 2*time.Second, "HTTP polling interval")
	flag.Parse()
	token := os.Getenv("APM_OPAMP_TOKEN")
	if token == "" {
		log.Fatal("APM_OPAMP_TOKEN required")
	}
	uid := make([]byte, 16)
	if _, err := rand.Read(uid); err != nil {
		log.Fatal(err)
	}
	log.Printf("instance_uid=%s", hex.EncodeToString(uid))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	client := &http.Client{Timeout: 10 * time.Second}
	ticker := time.NewTicker(*interval)
	defer ticker.Stop()
	var configStatus *protobufs.RemoteConfigStatus
	var sequence uint64
	for {
		sequence++
		msg := &protobufs.AgentToServer{
			InstanceUid: uid, SequenceNum: sequence,
			Capabilities: uint64(protobufs.AgentCapabilities_AgentCapabilities_AcceptsRemoteConfig | protobufs.AgentCapabilities_AgentCapabilities_ReportsRemoteConfig),
			AgentDescription: &protobufs.AgentDescription{IdentifyingAttributes: []*protobufs.KeyValue{
				{Key: "service.name", Value: &protobufs.AnyValue{Value: &protobufs.AnyValue_StringValue{StringValue: *service}}},
				{Key: "deployment.environment.name", Value: &protobufs.AnyValue{Value: &protobufs.AnyValue_StringValue{StringValue: "dev"}}},
				{Key: "agent.kind", Value: &protobufs.AnyValue{Value: &protobufs.AnyValue_StringValue{StringValue: "opamp-test-agent"}}},
				{Key: "telemetry.distro.name", Value: &protobufs.AnyValue{Value: &protobufs.AnyValue_StringValue{StringValue: "opamp-test-agent"}}},
				{Key: "telemetry.distro.version", Value: &protobufs.AnyValue{Value: &protobufs.AnyValue_StringValue{StringValue: "p1-demo"}}},
				{Key: "service.version", Value: &protobufs.AnyValue{Value: &protobufs.AnyValue_StringValue{StringValue: "app-demo"}}},
			}}, RemoteConfigStatus: configStatus,
		}
		body, err := proto.Marshal(msg)
		if err != nil {
			log.Fatal(err)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, *endpoint, bytes.NewReader(body))
		if err != nil {
			log.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/x-protobuf")
		res, err := client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("poll failed: %v", err)
		} else {
			raw, readErr := io.ReadAll(io.LimitReader(res.Body, 8<<20))
			_ = res.Body.Close()
			if readErr != nil {
				log.Printf("read response: %v", readErr)
			} else if res.StatusCode != 200 {
				log.Printf("HTTP %d", res.StatusCode)
			} else {
				var reply protobufs.ServerToAgent
				if err := proto.Unmarshal(raw, &reply); err != nil {
					log.Printf("decode: %v", err)
				} else if reply.RemoteConfig != nil {
					config := reply.RemoteConfig
					file := config.GetConfig().GetConfigMap()["apm/sampling.json"]
					var policy struct {
						SchemaVersion int `json:"schema_version"`
						Sampling      struct {
							Type string  `json:"type"`
							Rate float64 `json:"rate"`
						} `json:"sampling"`
					}
					valid := file != nil && json.Unmarshal(file.Body, &policy) == nil && policy.SchemaVersion == 1 &&
						policy.Sampling.Type == "parent_based_trace_id_ratio" && policy.Sampling.Rate >= 0 && policy.Sampling.Rate <= 1
					if valid {
						configStatus = &protobufs.RemoteConfigStatus{
							Status:               protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLIED,
							LastRemoteConfigHash: config.ConfigHash,
						}
						fmt.Printf("Applied policy rate=%.3f hash=%x (test agent, no JVM)\n", policy.Sampling.Rate, config.ConfigHash)
					} else {
						log.Print("remote policy validation failed")
					}
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
