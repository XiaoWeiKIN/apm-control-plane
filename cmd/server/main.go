package main

import (
 "bytes"
 "context"
 "crypto/sha256"
 "crypto/subtle"
 "encoding/hex"
 "encoding/json"
 "errors"
 "fmt"
 "io"
 "log/slog"
 "net/http"
 "os"
 "os/signal"
 "sort"
 "strings"
 "sync"
 "syscall"
 "time"

 "github.com/open-telemetry/opamp-go/protobufs"
 opampsrv "github.com/open-telemetry/opamp-go/server"
 "github.com/open-telemetry/opamp-go/server/types"
)

type agent struct {
 ID string
 Service string
 Environment string
 Version string
 LastSeen time.Time
 Connected bool
 Healthy *bool
 Capabilities uint64
 DesiredHash string
 ReportedHash string
 ConfigStatus string
 ConfigError string
 connection types.Connection
 remote *protobufs.AgentRemoteConfig
 sequence uint64
}
type registry struct {
 mu sync.RWMutex
 agents map[string]*agent
}
func newRegistry()*registry{return &registry{agents:make(map[string]*agent)}}
func (r *registry) snapshot(a *agent)map[string]any {
 return map[string]any{
  "instance_uid":a.ID,"service_name":a.Service,"environment":a.Environment,
  "agent_version":a.Version,"connected":a.Connected,"last_seen":a.LastSeen,
  "healthy":a.Healthy,"capabilities":a.Capabilities,"desired_config_hash":a.DesiredHash,
  "reported_config_hash":a.ReportedHash,"config_status":a.ConfigStatus,"config_error":a.ConfigError,
 }
}
func (r *registry) list()[]map[string]any {
 r.mu.RLock();defer r.mu.RUnlock()
 ids:=make([]string,0,len(r.agents));for id:=range r.agents{ids=append(ids,id)}
 sort.Strings(ids)
 result:=make([]map[string]any,0,len(ids));for _,id:=range ids{result=append(result,r.snapshot(r.agents[id]))}
 return result
}
func (r *registry) get(id string)(map[string]any,bool){
 r.mu.RLock();defer r.mu.RUnlock()
 a,ok:=r.agents[id];if !ok{return nil,false};return r.snapshot(a),true
}
func descriptionValue(d *protobufs.AgentDescription,key string)string{
 if d==nil{return ""}
 for _,xs:=range [][]*protobufs.KeyValue{d.IdentifyingAttributes,d.NonIdentifyingAttributes}{
  for _,x:=range xs{if x.Key==key&&x.Value!=nil{return x.Value.GetStringValue()}}
 }
 return ""
}
func validUID(id string)bool {
 if len(id)!=32{return false}
 raw,err:=hex.DecodeString(id)
 return err==nil&&len(raw)==16&&hex.EncodeToString(raw)==id
}
func (r *registry) update(conn types.Connection,msg *protobufs.AgentToServer)(*protobufs.AgentRemoteConfig,bool,error){
 if len(msg.InstanceUid)!=16{return nil,false,fmt.Errorf("instance_uid must be 16 bytes")}
 id:=hex.EncodeToString(msg.InstanceUid)
 r.mu.Lock();defer r.mu.Unlock()
 a,ok:=r.agents[id]
 if !ok{a=&agent{ID:id};r.agents[id]=a}
 requestFull:=(!ok&&msg.AgentDescription==nil)||(a.sequence!=0&&msg.SequenceNum!=a.sequence+1)
 a.sequence=msg.SequenceNum
 a.connection=conn;a.Connected=true;a.LastSeen=time.Now().UTC()
 a.Capabilities=msg.Capabilities
 if msg.AgentDescription!=nil{
  a.Service=descriptionValue(msg.AgentDescription,"service.name")
  a.Environment=descriptionValue(msg.AgentDescription,"deployment.environment.name")
  a.Version=descriptionValue(msg.AgentDescription,"service.version")
 }
 if msg.Health!=nil{b:=msg.Health.Healthy;a.Healthy=&b}
 if status:=msg.RemoteConfigStatus;status!=nil{
  a.ReportedHash=hex.EncodeToString(status.LastRemoteConfigHash)
  a.ConfigError=status.ErrorMessage
  if a.ReportedHash==a.DesiredHash&&a.DesiredHash!=""{a.ConfigStatus=status.Status.String()} else if a.DesiredHash!=""{a.ConfigStatus="PENDING"}
 }
 if a.remote==nil||msg.Capabilities&uint64(protobufs.AgentCapabilities_AgentCapabilities_AcceptsRemoteConfig)==0{return nil,requestFull,nil}
 if msg.RemoteConfigStatus!=nil&&bytes.Equal(msg.RemoteConfigStatus.LastRemoteConfigHash,a.remote.ConfigHash){return nil,requestFull,nil}
 return a.remote,requestFull,nil
}
func (r *registry) disconnect(conn types.Connection){
 r.mu.Lock();defer r.mu.Unlock()
 for _,a:=range r.agents{if a.connection==conn{a.connection=nil;a.Connected=false}}
}
func (r *registry) assign(id string,rate float64)(map[string]any,error){
 if rate!=rate||rate<0||rate>1{return nil,fmt.Errorf("sampling_rate must be in [0, 1]")}
 payload:=map[string]any{"schema_version":1,"sampling":map[string]any{"type":"parent_based_trace_id_ratio","rate":rate}}
 body,err:=json.Marshal(payload);if err!=nil{return nil,err}
 hash:=sha256.Sum256(body)
 remote:=&protobufs.AgentRemoteConfig{
  Config:&protobufs.AgentConfigMap{ConfigMap:map[string]*protobufs.AgentConfigObject{
   "apm/sampling.json":{Body:body,ContentType:"application/json"},
  }},
  ConfigHash:hash[:],
 }
 r.mu.Lock();defer r.mu.Unlock()
 a,ok:=r.agents[id];if !ok{return nil,errors.New("agent not found")}
 if a.remote!=nil&&bytes.Equal(a.remote.ConfigHash,hash[:]){return r.snapshot(a),nil}
 a.remote=remote;a.DesiredHash=hex.EncodeToString(hash[:]);a.ConfigStatus="PENDING";a.ConfigError=""
 return r.snapshot(a),nil
}
func response(w http.ResponseWriter,status int,data any){
 w.Header().Set("Content-Type","application/json");w.WriteHeader(status);_ =json.NewEncoder(w).Encode(data)
}
func protected(token string,next http.Handler)http.Handler{
 return http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){
  provided:=strings.TrimPrefix(r.Header.Get("Authorization"),"Bearer ")
  if token==""||subtle.ConstantTimeCompare([]byte(token),[]byte(provided))!=1{http.Error(w,"unauthorized",401);return}
  next.ServeHTTP(w,r)
 })
}
func api(r *registry)http.Handler{
 mux:=http.NewServeMux()
 mux.HandleFunc("GET /api/v1/agents",func(w http.ResponseWriter,_ *http.Request){response(w,200,r.list())})
 mux.HandleFunc("GET /api/v1/agents/{uid}",func(w http.ResponseWriter,req *http.Request){
  id:=req.PathValue("uid");if !validUID(id){http.Error(w,"invalid uid",400);return}
  v,ok:=r.get(id);if !ok{http.Error(w,"agent not found",404);return};response(w,200,v)
 })
 mux.HandleFunc("PUT /api/v1/agents/{uid}/sampling",func(w http.ResponseWriter,req *http.Request){
  id:=req.PathValue("uid");if !validUID(id){http.Error(w,"invalid uid",400);return}
  var value struct{SamplingRate *float64 `json:"sampling_rate"`}
  decoder:=json.NewDecoder(io.LimitReader(req.Body,64<<10));decoder.DisallowUnknownFields()
  if err:=decoder.Decode(&value);err!=nil||value.SamplingRate==nil{http.Error(w,"invalid sampling_rate JSON",400);return}
  var trailing any;if err:=decoder.Decode(&trailing);!errors.Is(err,io.EOF){http.Error(w,"unexpected trailing JSON",400);return}
  v,err:=r.assign(id,*value.SamplingRate)
  if err!=nil{code:=400;if err.Error()=="agent not found"{code=404};http.Error(w,err.Error(),code);return}
  response(w,200,v)
 })
 return mux
}
func main(){
 logger:=slog.New(slog.NewJSONHandler(os.Stdout,nil))
 agentToken,adminToken:=os.Getenv("APM_OPAMP_TOKEN"),os.Getenv("APM_ADMIN_TOKEN")
 if agentToken==""||adminToken==""||agentToken==adminToken{logger.Error("distinct APM_OPAMP_TOKEN and APM_ADMIN_TOKEN are required");os.Exit(1)}
 addr:=os.Getenv("APM_LISTEN_ADDR");if addr==""{addr="127.0.0.1:4320"}
 reg:=newRegistry()
 gateway:=opampsrv.New(nil)
 handler,connContext,err:=gateway.Attach(opampsrv.Settings{Callbacks:types.Callbacks{
  OnConnecting:func(req *http.Request)types.ConnectionResponse{
   provided:=strings.TrimPrefix(req.Header.Get("Authorization"),"Bearer ")
   if subtle.ConstantTimeCompare([]byte(agentToken),[]byte(provided))!=1{
    return types.ConnectionResponse{Accept:false,HTTPStatusCode:401}
   }
   return types.ConnectionResponse{Accept:true,ConnectionCallbacks:types.ConnectionCallbacks{
    OnConnectionClose:reg.disconnect,
    OnMessage:func(_ context.Context,conn types.Connection,msg *protobufs.AgentToServer)*protobufs.ServerToAgent{
     out:=&protobufs.ServerToAgent{InstanceUid:msg.InstanceUid,Capabilities:uint64(
      protobufs.ServerCapabilities_ServerCapabilities_AcceptsStatus|
      protobufs.ServerCapabilities_ServerCapabilities_OffersRemoteConfig)}
     config,full,err:=reg.update(conn,msg)
     if err!=nil{logger.Warn("rejected status","error",err);return out}
     if full{out.Flags=uint64(protobufs.ServerToAgentFlags_ServerToAgentFlags_ReportFullState)}
     out.RemoteConfig=config;return out
    },
    OnReadMessageError:func(_ types.Connection,_ int,_ []byte,e error){logger.Warn("opamp read error","error",e)},
    OnMessageResponseError:func(_ types.Connection,_ *protobufs.ServerToAgent,e error){logger.Warn("opamp send error","error",e)},
   }}
  },
 }})
 if err!=nil{logger.Error("opamp attach failed","error",err);os.Exit(1)}
 mux:=http.NewServeMux()
 mux.HandleFunc("GET /healthz",func(w http.ResponseWriter,_ *http.Request){response(w,200,map[string]string{"status":"ok"})})
 mux.HandleFunc("/v1/opamp",handler)
 mux.Handle("/api/",protected(adminToken,api(reg)))
 srv:=&http.Server{Addr:addr,Handler:mux,ConnContext:connContext,ReadHeaderTimeout:5*time.Second}
 ctx,stop:=signal.NotifyContext(context.Background(),os.Interrupt,syscall.SIGTERM);defer stop()
 go func(){<-ctx.Done();shutdownCtx,cancel:=context.WithTimeout(context.Background(),5*time.Second);defer cancel();_ =srv.Shutdown(shutdownCtx)}()
 logger.Info("starting opamp control plane","address",addr)
 if err:=srv.ListenAndServe();err!=nil&&!errors.Is(err,http.ErrServerClosed){logger.Error("server failed","error",err);os.Exit(1)}
}
