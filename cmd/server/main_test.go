package main

import (
	"context"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/XiaoWeiKIN/apm-control-plane/internal/appconfig"
	"github.com/XiaoWeiKIN/apm-control-plane/internal/auth"
	"github.com/XiaoWeiKIN/apm-control-plane/internal/storage/gormrepo"
	"github.com/open-telemetry/opamp-go/protobufs"
)

type fakeStore struct {
	mu   sync.Mutex
	data map[string]gormrepo.AgentRecord
}

func newFakeStore() *fakeStore { return &fakeStore{data: make(map[string]gormrepo.AgentRecord)} }
func (s *fakeStore) Observe(_ context.Context, p auth.Principal, id string, o gormrepo.Observation) (gormrepo.AgentRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := key(p.WorkspaceID, id)
	rec, ok := s.data[k]
	if ok && rec.CredentialID != p.CredentialID {
		return rec, gormrepo.ErrOwnership
	}
	if !ok {
		rec = gormrepo.AgentRecord{WorkspaceID: p.WorkspaceID, InstanceUID: id, CredentialID: p.CredentialID, AgentKind: "unknown"}
	}
	rec.LastSeen = o.SeenAt
	rec.LastTransport = o.Transport
	rec.Capabilities = int64(o.Capabilities)
	if o.Description != nil {
		rec.AgentKind = o.Description.Kind
		rec.Distribution = o.Description.Distribution
		rec.DistributionVersion = o.Description.DistributionVersion
		rec.ServiceName = o.Description.Service
		rec.ServiceVersion = o.Description.ServiceVersion
		rec.Environment = o.Description.Environment
	}
	if o.Healthy != nil {
		rec.Healthy = o.Healthy
	}
	if o.Config != nil {
		rec.ReportedHash = o.Config.Hash
		rec.ConfigStatus = o.Config.Status
		rec.ConfigError = o.Config.Error
	}
	s.data[k] = rec
	return rec, nil
}
func (s *fakeStore) Get(_ context.Context, workspace, id string) (gormrepo.AgentRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.data[key(workspace, id)]
	if !ok {
		return r, gormrepo.ErrNotFound
	}
	return r, nil
}
func (s *fakeStore) List(_ context.Context, workspace string) ([]gormrepo.AgentRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := []gormrepo.AgentRecord{}
	for _, rec := range s.data {
		if rec.WorkspaceID == workspace {
			result = append(result, rec)
		}
	}
	return result, nil
}

func uid(first byte) ([]byte, string) {
	id := make([]byte, 16)
	id[0] = first
	return id, hex.EncodeToString(id)
}
func principal(workspace, id string) auth.Principal {
	return auth.Principal{WorkspaceID: workspace, CredentialID: id}
}

func TestPolicyDeliveryAndAck(t *testing.T) {
	store := newFakeStore()
	reg := newRegistry(store, 30*time.Second)
	p := principal("acme", "agent-a")
	id, idHex := uid(1)
	ctx := context.Background()
	msg := &protobufs.AgentToServer{InstanceUid: id, SequenceNum: 1, Capabilities: uint64(protobufs.AgentCapabilities_AgentCapabilities_AcceptsRemoteConfig)}
	cfg, _, err := reg.update(ctx, p, false, nil, msg)
	if err != nil || cfg != nil {
		t.Fatalf("unexpected first update: %v, %v", cfg, err)
	}
	if _, err := reg.assign(ctx, p, idHex, 0.1); err != nil {
		t.Fatal(err)
	}
	msg.SequenceNum = 2
	cfg, _, err = reg.update(ctx, p, false, nil, msg)
	if err != nil || cfg == nil {
		t.Fatalf("expected configuration: %v", err)
	}
	msg.SequenceNum = 3
	msg.RemoteConfigStatus = &protobufs.RemoteConfigStatus{Status: protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLIED, LastRemoteConfigHash: cfg.ConfigHash}
	cfg, _, err = reg.update(ctx, p, false, nil, msg)
	if err != nil || cfg != nil {
		t.Fatal("acked config must not be resent", err)
	}
	item, err := reg.get(ctx, p, idHex)
	if err != nil {
		t.Fatal(err)
	}
	if item["desired_config_hash"] != item["reported_config_hash"] || item["config_status"] != "APPLIED" {
		t.Fatalf("wrong state: %+v", item)
	}
}

func TestWorkspaceScopeAndOwnerBinding(t *testing.T) {
	reg := newRegistry(newFakeStore(), 30*time.Second)
	id, uidHex := uid(2)
	ctx := context.Background()
	msg := &protobufs.AgentToServer{InstanceUid: id, SequenceNum: 1}
	owner := principal("workspace1", "token1")
	if _, _, err := reg.update(ctx, owner, false, nil, msg); err != nil {
		t.Fatal(err)
	}
	_, _, err := reg.update(ctx, principal("workspace1", "other-token"), false, nil, msg)
	if !errors.Is(err, gormrepo.ErrOwnership) {
		t.Fatalf("expected ownership rejection, got %v", err)
	}
	if _, err := reg.get(ctx, principal("workspace2", "admin"), uidHex); !errors.Is(err, gormrepo.ErrNotFound) {
		t.Fatalf("workspace leakage: %v", err)
	}

	adminCreds := auth.New([]appconfig.Credential{{ID: "admin1", WorkspaceID: "workspace1", Token: "admin-one-secret-long"}, {ID: "admin2", WorkspaceID: "workspace2", Token: "admin-two-secret-long"}})
	api := adminHandler(reg, adminCreds)
	for _, tt := range []struct {
		token  string
		status int
	}{
		{"admin-one-secret-long", 200}, {"admin-two-secret-long", 404}, {"bad-secret-very-long", 401},
	} {
		req := httptest.NewRequest("GET", "/api/v1/agents/"+uidHex, nil)
		req.Header.Set("Authorization", "Bearer "+tt.token)
		w := httptest.NewRecorder()
		api.ServeHTTP(w, req)
		if w.Code != tt.status {
			t.Fatalf("%s: got %d expected %d", tt.token, w.Code, tt.status)
		}
	}
}

func TestTransportFreshnessAndIdentityFields(t *testing.T) {
	ctx := context.Background()
	st := newFakeStore()
	reg := newRegistry(st, 15*time.Second)
	id, uidHex := uid(3)
	p := principal("workspace1", "agent1")
	msg := &protobufs.AgentToServer{InstanceUid: id, SequenceNum: 1, AgentDescription: &protobufs.AgentDescription{
		IdentifyingAttributes: []*protobufs.KeyValue{
			{Key: "service.name", Value: &protobufs.AnyValue{Value: &protobufs.AnyValue_StringValue{StringValue: "checkout"}}},
			{Key: "service.version", Value: &protobufs.AnyValue{Value: &protobufs.AnyValue_StringValue{StringValue: "app-9"}}},
			{Key: "telemetry.distro.version", Value: &protobufs.AnyValue{Value: &protobufs.AnyValue_StringValue{StringValue: "agent-2"}}},
			{Key: "telemetry.sdk.language", Value: &protobufs.AnyValue{Value: &protobufs.AnyValue_StringValue{StringValue: "java"}}},
		},
	}}
	if _, _, err := reg.update(ctx, p, false, nil, msg); err != nil {
		t.Fatal(err)
	}
	item, err := reg.get(ctx, p, uidHex)
	if err != nil {
		t.Fatal(err)
	}
	if item["workspace_id"] != "workspace1" || item["connected"] != true || item["agent_kind"] != "java-apm" || item["agent_version"] != "agent-2" || item["service_version"] != "app-9" {
		t.Fatalf("invalid snapshot: %+v", item)
	}
	st.mu.Lock()
	rec := st.data[key("workspace1", uidHex)]
	rec.LastSeen = time.Now().Add(-time.Minute)
	st.data[key("workspace1", uidHex)] = rec
	st.mu.Unlock()
	item, err = reg.get(ctx, p, uidHex)
	if err != nil || item["connected"] != false {
		t.Fatalf("polling freshness lost: %+v %v", item, err)
	}
}

type mockConnection struct {
	id           int
	disconnected chan struct{}
}

func (*mockConnection) Connection() net.Conn                                 { return nil }
func (*mockConnection) Send(context.Context, *protobufs.ServerToAgent) error { return nil }
func (c *mockConnection) Disconnect() error {
	if c.disconnected != nil {
		close(c.disconnected)
	}
	return nil
}

func TestWebsocketSessionExclusion(t *testing.T) {
	reg := newRegistry(newFakeStore(), time.Second)
	id, _ := uid(4)
	p := principal("t1", "c1")
	connA, connB := &mockConnection{id: 1}, &mockConnection{id: 2}
	msg := &protobufs.AgentToServer{InstanceUid: id, SequenceNum: 1}
	if _, _, err := reg.update(context.Background(), p, true, connA, msg); err != nil {
		t.Fatal(err)
	}
	if _, _, err := reg.update(context.Background(), p, true, connB, msg); err == nil {
		t.Fatal("concurrent WS session must be rejected")
	}
	reg.disconnect(p, connA)
	if _, _, err := reg.update(context.Background(), p, true, connB, msg); err != nil {
		t.Fatalf("reconnect rejected: %v", err)
	}
}

func TestInactiveSessionsAreEvictedWithoutDroppingActiveOrDemoState(t *testing.T) {
	reg := newRegistry(newFakeStore(), 30*time.Second)
	p := principal("workspace", "owner")
	ctx := context.Background()
	inactiveUID, inactiveID := uid(60)
	activeUID, activeID := uid(61)
	demoUID, demoID := uid(62)
	for _, id := range [][]byte{inactiveUID, activeUID, demoUID} {
		if _, _, err := reg.update(ctx, p, false, nil, &protobufs.AgentToServer{
			InstanceUid: id, SequenceNum: 1,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := reg.assign(ctx, p, demoID, 0.25); err != nil {
		t.Fatal(err)
	}
	conn := &mockConnection{id: 62}
	if _, _, err := reg.update(ctx, p, true, conn, &protobufs.AgentToServer{
		InstanceUid: activeUID, SequenceNum: 2,
	}); err != nil {
		t.Fatal(err)
	}
	reg.mu.Lock()
	for _, id := range []string{inactiveID, activeID, demoID} {
		reg.sessions[key(p.WorkspaceID, id)].lastActive = time.Now().Add(-6 * time.Minute)
	}
	reg.lastSweep = time.Now().Add(-2 * time.Minute)
	reg.mu.Unlock()

	freshUID, _ := uid(63)
	if _, _, err := reg.update(ctx, p, false, nil, &protobufs.AgentToServer{
		InstanceUid: freshUID, SequenceNum: 1,
	}); err != nil {
		t.Fatal(err)
	}
	reg.mu.Lock()
	_, idleExists := reg.sessions[key(p.WorkspaceID, inactiveID)]
	_, activeExists := reg.sessions[key(p.WorkspaceID, activeID)]
	_, demoExists := reg.sessions[key(p.WorkspaceID, demoID)]
	reg.mu.Unlock()
	if idleExists || !activeExists || !demoExists {
		t.Fatalf("incorrect session eviction: idle=%t active=%t demo=%t", idleExists, activeExists, demoExists)
	}
}

func TestBadSamplingAndUID(t *testing.T) {
	for _, rate := range []float64{-1, 2} {
		if _, err := samplingConfig(rate); err == nil {
			t.Fatalf("accepted invalid rate %f", rate)
		}
	}
	if validUID("invalid") || validUID(strings.Repeat("X", 32)) {
		t.Fatal("invalid UID accepted")
	}
}

func TestAdminUnauthorized(t *testing.T) {
	handler := adminHandler(newRegistry(newFakeStore(), time.Second), auth.New([]appconfig.Credential{{ID: "x", WorkspaceID: "t", Token: "very-long-admin-token"}}))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/agents", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unexpected status: %d", w.Code)
	}
}

type blockingStore struct {
	*fakeStore
	blockUID string
	entered  chan struct{}
	release  chan struct{}
	once     sync.Once
}

func (s *blockingStore) Observe(ctx context.Context, p auth.Principal, id string, o gormrepo.Observation) (gormrepo.AgentRecord, error) {
	if id == s.blockUID {
		s.once.Do(func() { close(s.entered) })
		<-s.release
	}
	return s.fakeStore.Observe(ctx, p, id, o)
}

func TestUnrelatedAgentWriteIsNotBlockedByDatabaseIO(t *testing.T) {
	idA, hexA := uid(10)
	idB, _ := uid(11)
	store := &blockingStore{fakeStore: newFakeStore(), blockUID: hexA, entered: make(chan struct{}), release: make(chan struct{})}
	unblock := sync.OnceFunc(func() { close(store.release) })
	defer unblock()
	reg := newRegistry(store, time.Second)
	p := principal("workspace-a", "credential-a")
	firstDone := make(chan error, 1)
	go func() {
		_, _, err := reg.update(context.Background(), p, false, nil, &protobufs.AgentToServer{InstanceUid: idA, SequenceNum: 1})
		firstDone <- err
	}()
	select {
	case <-store.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("first Agent never reached storage")
	}

	secondDone := make(chan error, 1)
	go func() {
		_, _, err := reg.update(context.Background(), p, false, nil, &protobufs.AgentToServer{InstanceUid: idB, SequenceNum: 1})
		secondDone <- err
	}()
	select {
	case err := <-secondDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("unrelated Agent was blocked by a database write")
	}
	unblock()
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
}

func TestStopWaitsForPendingWriteAndRejectsNewObservations(t *testing.T) {
	idA, hexA := uid(12)
	idB, _ := uid(13)
	store := &blockingStore{fakeStore: newFakeStore(), blockUID: hexA, entered: make(chan struct{}), release: make(chan struct{})}
	unblock := sync.OnceFunc(func() { close(store.release) })
	defer unblock()
	reg := newRegistry(store, time.Second)
	p := principal("workspace-a", "credential-a")
	conn := &mockConnection{id: 4, disconnected: make(chan struct{})}
	reg.trackWS(conn)
	updateDone := make(chan error, 1)
	go func() {
		_, _, err := reg.update(context.Background(), p, true, conn, &protobufs.AgentToServer{InstanceUid: idA, SequenceNum: 1})
		updateDone <- err
	}()
	select {
	case <-store.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("observation never reached storage")
	}
	stopped := make(chan error, 1)
	go func() { stopped <- reg.stop() }()
	select {
	case <-conn.disconnected:
	case <-time.After(2 * time.Second):
		t.Fatal("stop did not disconnect the WebSocket")
	}
	if _, _, err := reg.update(context.Background(), p, false, nil, &protobufs.AgentToServer{InstanceUid: idB, SequenceNum: 1}); !errors.Is(err, errRegistryClosing) {
		t.Fatalf("new status accepted during shutdown: %v", err)
	}
	select {
	case err := <-stopped:
		t.Fatalf("stop returned before the write finished: %v", err)
	default:
	}
	unblock()
	if err := <-updateDone; err != nil {
		t.Fatal(err)
	}
	if err := <-stopped; err != nil {
		t.Fatal(err)
	}
}

func TestServeWaitsForHTTPRequestsToDrain(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered := make(chan struct{})
	release := make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(entered)
		<-release
		w.WriteHeader(http.StatusOK)
	})}
	done := make(chan error, 1)
	go func() { done <- serve(ctx, srv, listener, newRegistry(newFakeStore(), time.Second)) }()
	clientDone := make(chan error, 1)
	go func() {
		res, err := (&http.Client{Timeout: 3 * time.Second}).Get("http://" + listener.Addr().String())
		if err == nil {
			_ = res.Body.Close()
		}
		clientDone <- err
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("HTTP request did not reach handler")
	}
	cancel()
	select {
	case err := <-done:
		t.Fatalf("serve exited before request completion: %v", err)
	case <-time.After(75 * time.Millisecond):
	}
	unblock()
	if err := <-clientDone; err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestInvalidObservationIsOpAMPBadRequest(t *testing.T) {
	reply := processingErrorResponse(make([]byte, 16), gormrepo.ErrInvalidObservation)
	if reply.ErrorResponse == nil ||
		reply.ErrorResponse.Type != protobufs.ServerErrorResponseType_ServerErrorResponseType_BadRequest ||
		reply.RemoteConfig != nil || reply.Capabilities != 0 {
		t.Fatalf("invalid Agent metadata should yield only error_response: %+v", reply)
	}
}
