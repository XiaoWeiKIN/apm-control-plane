package main

import (
 "encoding/hex"
 "net/http"
 "net/http/httptest"
 "strings"
 "testing"

 "github.com/open-telemetry/opamp-go/protobufs"
)

func TestPolicyDeliveryAndAck(t *testing.T){
 r:=newRegistry()
 id:=make([]byte,16);id[0]=1
 msg:=&protobufs.AgentToServer{
  InstanceUid:id,SequenceNum:1,
  Capabilities:uint64(protobufs.AgentCapabilities_AgentCapabilities_AcceptsRemoteConfig),
 }
 cfg,_,err:=r.update(nil,msg)
 if err!=nil||cfg!=nil{t.Fatalf("unexpected first status: %v %v",cfg,err)}
 uid:=hex.EncodeToString(id)
 if _,err:=r.assign(uid,0.1);err!=nil{t.Fatal(err)}
 msg.SequenceNum=2
 cfg,_,err=r.update(nil,msg)
 if err!=nil||cfg==nil{t.Fatal("expected remote config after assignment")}
 msg.SequenceNum=3
 msg.RemoteConfigStatus=&protobufs.RemoteConfigStatus{
  Status:protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLIED,
  LastRemoteConfigHash:cfg.ConfigHash,
 }
 cfg,_,err=r.update(nil,msg)
 if err!=nil||cfg!=nil{t.Fatal("acknowledged policy must not be resent")}
 state,ok:=r.get(uid)
 if !ok||state["desired_config_hash"]!=state["reported_config_hash"]{t.Fatalf("unexpected status: %+v",state)}
}

func TestAuthAndValidation(t *testing.T){
 r:=newRegistry()
 handler:=protected("admin-token",api(r))
 req:=httptest.NewRequest("GET","/api/v1/agents",nil)
 w:=httptest.NewRecorder();handler.ServeHTTP(w,req)
 if w.Code!=http.StatusUnauthorized{t.Fatalf("unauthorized returned %d",w.Code)}
 id:=make([]byte,16);id[0]=1
 _,_,_ =r.update(nil,&protobufs.AgentToServer{InstanceUid:id})
 uid:=hex.EncodeToString(id)
 req=httptest.NewRequest("PUT","/api/v1/agents/"+uid+"/sampling",strings.NewReader(`{"sampling_rate":0.25}`))
 req.Header.Set("Authorization","Bearer admin-token")
 w=httptest.NewRecorder();handler.ServeHTTP(w,req)
 if w.Code!=http.StatusOK{t.Fatalf("publish failed %d: %s",w.Code,w.Body.String())}
 req=httptest.NewRequest("PUT","/api/v1/agents/"+uid+"/sampling",strings.NewReader(`{"sampling_rate":1.5}`))
 req.Header.Set("Authorization","Bearer admin-token")
 w=httptest.NewRecorder();handler.ServeHTTP(w,req)
 if w.Code!=http.StatusBadRequest{t.Fatalf("invalid rate accepted: %d",w.Code)}
}

func TestInvalidUID(t *testing.T){
 r:=newRegistry()
 if _,_,err:=r.update(nil,&protobufs.AgentToServer{InstanceUid:[]byte{1}});err==nil{t.Fatal("invalid uid accepted")}
}
