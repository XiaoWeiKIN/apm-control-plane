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

type fakeStore struct{
	mu sync.Mutex
	data map[string]gormrepo.AgentRecord
}
func newFakeStore() *fakeStore {return &fakeStore{data:make(map[string]gormrepo.AgentRecord)}}
func (s *fakeStore) Observe(_ context.Context,p auth.Principal,id string,o gormrepo.Observation)(gormrepo.AgentRecord,error){
	s.mu.Lock();defer s.mu.Unlock()
	k:=key(p.TenantID,id)
	rec,ok:=s.data[k]
	if ok && rec.CredentialID!=p.CredentialID {return rec,gormrepo.ErrOwnership}
	if !ok {rec=gormrepo.AgentRecord{TenantID:p.TenantID,InstanceUID:id,CredentialID:p.CredentialID,AgentKind:"unknown"}}
	rec.LastSeen=o.SeenAt
	rec.LastTransport=o.Transport
	rec.Capabilities=int64(o.Capabilities)
	if o.Description!=nil {
		rec.AgentKind=o.Description.Kind
		rec.Distribution=o.Description.Distribution
		rec.DistributionVersion=o.Description.DistributionVersion
		rec.ServiceName=o.Description.Service
		rec.ServiceVersion=o.Description.ServiceVersion
		rec.Environment=o.Description.Environment
	}
	if o.Healthy!=nil {rec.Healthy=o.Healthy}
	if o.Config!=nil {rec.ReportedHash=o.Config.Hash;rec.ConfigStatus=o.Config.Status;rec.ConfigError=o.Config.Error}
	s.data[k]=rec
	return rec,nil
}
func (s *fakeStore) Get(_ context.Context,tenant,id string)(gormrepo.AgentRecord,error){
	s.mu.Lock();defer s.mu.Unlock()
	r,ok:=s.data[key(tenant,id)]
	if !ok{return r,gormrepo.ErrNotFound}
	return r,nil
}
func (s *fakeStore) List(_ context.Context,tenant string)([]gormrepo.AgentRecord,error){
	s.mu.Lock();defer s.mu.Unlock()
	result:=[]gormrepo.AgentRecord{}
	for _,rec:=range s.data {if rec.TenantID==tenant {result=append(result,rec)}}
	return result,nil
}

func uid(first byte)([]byte,string){
	id:=make([]byte,16);id[0]=first
	return id,hex.EncodeToString(id)
}
func principal(tenant,id string)auth.Principal{return auth.Principal{TenantID:tenant,CredentialID:id}}

func TestPolicyDeliveryAndAck(t *testing.T){
	store:=newFakeStore()
	reg:=newRegistry(store,30*time.Second)
	p:=principal("acme","agent-a")
	id,idHex:=uid(1)
	ctx:=context.Background()
	msg:=&protobufs.AgentToServer{InstanceUid:id,SequenceNum:1,Capabilities:uint64(protobufs.AgentCapabilities_AgentCapabilities_AcceptsRemoteConfig)}
	cfg,_,err:=reg.update(ctx,p,false,nil,msg)
	if err!=nil || cfg!=nil {t.Fatalf("unexpected first update: %v, %v",cfg,err)}
	if _,err:=reg.assign(ctx,p,idHex,0.1);err!=nil{t.Fatal(err)}
	msg.SequenceNum=2
	cfg,_,err=reg.update(ctx,p,false,nil,msg)
	if err!=nil||cfg==nil{t.Fatalf("expected configuration: %v",err)}
	msg.SequenceNum=3
	msg.RemoteConfigStatus=&protobufs.RemoteConfigStatus{Status:protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLIED,LastRemoteConfigHash:cfg.ConfigHash}
	cfg,_,err=reg.update(ctx,p,false,nil,msg)
	if err!=nil||cfg!=nil{t.Fatal("acked config must not be resent",err)}
	item,err:=reg.get(ctx,p,idHex)
	if err!=nil {t.Fatal(err)}
	if item["desired_config_hash"]!=item["reported_config_hash"] || item["config_status"]!="APPLIED" {t.Fatalf("wrong state: %+v",item)}
}

func TestTenantScopeAndOwnerBinding(t *testing.T){
	reg:=newRegistry(newFakeStore(),30*time.Second)
	id,uidHex:=uid(2)
	ctx:=context.Background()
	msg:=&protobufs.AgentToServer{InstanceUid:id,SequenceNum:1}
	owner:=principal("tenant1","token1")
	if _,_,err:=reg.update(ctx,owner,false,nil,msg);err!=nil {t.Fatal(err)}
	_,_,err:=reg.update(ctx,principal("tenant1","other-token"),false,nil,msg)
	if !errors.Is(err,gormrepo.ErrOwnership){t.Fatalf("expected ownership rejection, got %v",err)}
	if _,err:=reg.get(ctx,principal("tenant2","admin"),uidHex);!errors.Is(err,gormrepo.ErrNotFound){t.Fatalf("tenant leakage: %v",err)}

	adminCreds:=auth.New([]appconfig.Credential{{ID:"admin1",TenantID:"tenant1",Token:"admin-one-secret-long"},{ID:"admin2",TenantID:"tenant2",Token:"admin-two-secret-long"}})
	api:=adminHandler(reg,adminCreds)
	for _,tt:=range []struct{token string;status int}{
		{"admin-one-secret-long",200},{"admin-two-secret-long",404},{"bad-secret-very-long",401},
	}{
		req:=httptest.NewRequest("GET","/api/v1/agents/"+uidHex,nil)
		req.Header.Set("Authorization","Bearer "+tt.token)
		w:=httptest.NewRecorder();api.ServeHTTP(w,req)
		if w.Code!=tt.status{t.Fatalf("%s: got %d expected %d",tt.token,w.Code,tt.status)}
	}
}

func TestTransportFreshnessAndIdentityFields(t *testing.T){
	ctx:=context.Background()
	st:=newFakeStore()
	reg:=newRegistry(st,15*time.Second)
	id,uidHex:=uid(3)
	p:=principal("tenant1","agent1")
	msg:=&protobufs.AgentToServer{InstanceUid:id,SequenceNum:1,AgentDescription:&protobufs.AgentDescription{
		IdentifyingAttributes:[]*protobufs.KeyValue{
			{Key:"service.name",Value:&protobufs.AnyValue{Value:&protobufs.AnyValue_StringValue{StringValue:"checkout"}}},
			{Key:"service.version",Value:&protobufs.AnyValue{Value:&protobufs.AnyValue_StringValue{StringValue:"app-9"}}},
			{Key:"telemetry.distro.version",Value:&protobufs.AnyValue{Value:&protobufs.AnyValue_StringValue{StringValue:"agent-2"}}},
			{Key:"telemetry.sdk.language",Value:&protobufs.AnyValue{Value:&protobufs.AnyValue_StringValue{StringValue:"java"}}},
		},
	}}
	if _,_,err:=reg.update(ctx,p,false,nil,msg);err!=nil{t.Fatal(err)}
	item,err:=reg.get(ctx,p,uidHex)
	if err!=nil {t.Fatal(err)}
	if item["connected"]!=true || item["agent_kind"]!="java-apm" || item["agent_version"]!="agent-2" || item["service_version"]!="app-9" {
		t.Fatalf("invalid snapshot: %+v",item)
	}
	st.mu.Lock()
	rec:=st.data[key("tenant1",uidHex)]
	rec.LastSeen=time.Now().Add(-time.Minute)
	st.data[key("tenant1",uidHex)]=rec
	st.mu.Unlock()
	item,err=reg.get(ctx,p,uidHex)
	if err!=nil||item["connected"]!=false{t.Fatalf("polling freshness lost: %+v %v",item,err)}
}

type mockConnection struct{}
func (*mockConnection) Connection() net.Conn{return nil}
func (*mockConnection) Send(context.Context,*protobufs.ServerToAgent) error{return nil}
func (*mockConnection) Disconnect()error{return nil}

func TestWebsocketSessionExclusion(t *testing.T){
	reg:=newRegistry(newFakeStore(),time.Second)
	id,_:=uid(4)
	p:=principal("t1","c1")
	connA,connB:=&mockConnection{},&mockConnection{}
	msg:=&protobufs.AgentToServer{InstanceUid:id,SequenceNum:1}
	if _,_,err:=reg.update(context.Background(),p,true,connA,msg);err!=nil {t.Fatal(err)}
	if _,_,err:=reg.update(context.Background(),p,true,connB,msg);err==nil {t.Fatal("concurrent WS session must be rejected")}
	reg.disconnect(p,connA)
	if _,_,err:=reg.update(context.Background(),p,true,connB,msg);err!=nil {t.Fatalf("reconnect rejected: %v",err)}
}

func TestBadSamplingAndUID(t *testing.T){
	for _,rate:=range []float64{-1,2} {
		if _,err:=samplingConfig(rate);err==nil {t.Fatalf("accepted invalid rate %f",rate)}
	}
	if validUID("invalid") || validUID(strings.Repeat("X",32)){t.Fatal("invalid UID accepted")}
}

func TestAdminUnauthorized(t *testing.T){
	handler:=adminHandler(newRegistry(newFakeStore(),time.Second),auth.New([]appconfig.Credential{{ID:"x",TenantID:"t",Token:"very-long-admin-token"}}))
	w:=httptest.NewRecorder()
	handler.ServeHTTP(w,httptest.NewRequest(http.MethodGet,"/api/v1/agents",nil))
	if w.Code!=http.StatusUnauthorized{t.Fatalf("unexpected status: %d",w.Code)}
}
