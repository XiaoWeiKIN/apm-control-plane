package main

import (
	"context"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
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

type pushTestDelivery struct {
	desired *gormrepo.DesiredConfig
	called  chan struct{}
}

func (s *pushTestDelivery) ReconcileAgentConfig(_ context.Context, _, _ string, _ *gormrepo.ConfigStatus, offer bool) (*gormrepo.DesiredConfig, bool, error) {
	if s.called != nil {
		select {
		case s.called <- struct{}{}:
		default:
		}
	}
	if offer && s.desired != nil {
		return s.desired, true, nil
	}
	return nil, s.desired != nil, nil
}

type failingWSConnection struct {
	mu           sync.Mutex
	sendCount    int
	disconnected chan struct{}
	once         sync.Once
}

func (*failingWSConnection) Connection() net.Conn { return nil }

func (c *failingWSConnection) Send(context.Context, *protobufs.ServerToAgent) error {
	c.mu.Lock()
	c.sendCount++
	c.mu.Unlock()
	return errors.New("injected WS write failure")
}

func (c *failingWSConnection) Disconnect() error {
	c.once.Do(func() { close(c.disconnected) })
	return nil
}

func TestWSFailedSendDisconnectsAndStopsWriter(t *testing.T) {
	snapshot, err := config.CompileJavaSampling(0.25)
	if err != nil {
		t.Fatal(err)
	}
	hash := snapshot.Hash()
	delivery := &pushTestDelivery{desired: &gormrepo.DesiredConfig{
		ConfigMapBytes: snapshot.ConfigMapBytes(), ConfigHash: hex.EncodeToString(hash[:]),
	}}
	reg := newRegistry(newFakeStore(), time.Second)
	reg.delivery = delivery
	p := principal("workspace-a", "agent-a")
	uidBytes, uidHex := uid(55)
	conn := &failingWSConnection{disconnected: make(chan struct{})}
	reg.sessions[key(p.WorkspaceID, uidHex)] = &runtimeAgent{conn: conn}
	reg.trackWS(conn)
	reg.startWriter(p, conn, uidBytes, slog.New(slog.NewTextHandler(io.Discard, nil)))
	select {
	case <-conn.disconnected:
	case <-time.After(3 * time.Second):
		t.Fatal("failed WS send did not disconnect session")
	}
	reg.disconnect(p, conn)
	if err := reg.stop(); err != nil {
		t.Fatal(err)
	}
	conn.mu.Lock()
	sends := conn.sendCount
	conn.mu.Unlock()
	if sends != 1 {
		t.Fatalf("failed WS send retried on terminated session: %d", sends)
	}
}

func TestWSReportFullStateWithoutDesired(t *testing.T) {
	reg := newRegistry(newFakeStore(), time.Second)
	reg.delivery = &pushTestDelivery{}
	handler, cc, err := makeGateway(reg, auth.New([]appconfig.Credential{
		{ID: "agent-a", WorkspaceID: "workspace-a", Token: "ws-status-token-long"},
	}), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/opamp", handler)
	server := httptest.NewUnstartedServer(mux)
	server.Config.ConnContext = cc
	server.Start()
	defer server.Close()
	defer reg.stop()

	conn, response, err := websocket.DefaultDialer.Dial("ws"+server.URL[len("http"):]+"/v1/opamp",
		http.Header{"Authorization": {"Bearer ws-status-token-long"}})
	if err != nil {
		t.Fatalf("dial: %v, response: %v", err, response)
	}
	defer conn.Close()
	id, _ := uid(56)
	wire, err := proto.Marshal(&protobufs.AgentToServer{InstanceUid: id, SequenceNum: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.WriteMessage(websocket.BinaryMessage, append([]byte{0}, wire...)); err != nil {
		t.Fatal(err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	_, replyWire, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("missing ReportFullState: %v", err)
	}
	var reply protobufs.ServerToAgent
	if len(replyWire) == 0 || replyWire[0] != 0 {
		t.Fatalf("unexpected WS header: %x", replyWire)
	}
	if err := proto.Unmarshal(replyWire[1:], &reply); err != nil {
		t.Fatal(err)
	}
	flag := uint64(protobufs.ServerToAgentFlags_ServerToAgentFlags_ReportFullState)
	if reply.Flags&flag == 0 || reply.RemoteConfig != nil {
		t.Fatalf("invalid full-state request: %+v", reply)
	}
}
