package main

import (
	"bytes"
	"context"
	"crypto/sha256"
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

	"github.com/XiaoWeiKIN/apm-control-plane/internal/appconfig"
	"github.com/XiaoWeiKIN/apm-control-plane/internal/auth"
	"github.com/XiaoWeiKIN/apm-control-plane/internal/storage/gormrepo"
	"github.com/gorilla/websocket"
	"github.com/open-telemetry/opamp-go/protobufs"
	opampsrv "github.com/open-telemetry/opamp-go/server"
	"github.com/open-telemetry/opamp-go/server/types"
)

// AgentStore defines the minimal domain-facing inventory operations.
// The production implementation is GORM; unit tests use an in-memory fake.
type AgentStore interface {
	Observe(context.Context, auth.Principal, string, gormrepo.Observation) (gormrepo.AgentRecord, error)
	Get(context.Context, string, string) (gormrepo.AgentRecord, error)
	List(context.Context, string) ([]gormrepo.AgentRecord, error)
}

type runtimeAgent struct {
	conn types.Connection
	sequence uint64
	remote *protobufs.AgentRemoteConfig
	desiredHash string
	configStatus string
}

type registry struct {
	mu sync.Mutex
	store AgentStore
	sessions map[string]*runtimeAgent
	pollTTL time.Duration
}

func newRegistry(store AgentStore, ttl time.Duration) *registry {
	return &registry{store:store, sessions:make(map[string]*runtimeAgent), pollTTL:ttl}
}

func key(tenant, uid string) string { return tenant+"/"+uid }

func validUID(id string) bool {
	if len(id)!=32 { return false }
	raw,err:=hex.DecodeString(id)
	return err==nil && len(raw)==16 && hex.EncodeToString(raw)==id
}

func descriptionValue(d *protobufs.AgentDescription, k string) string {
	if d==nil{return ""}
	for _,attributes:=range [][]*protobufs.KeyValue{d.IdentifyingAttributes,d.NonIdentifyingAttributes} {
		for _,a:=range attributes {
			if a!=nil && a.Key==k && a.Value!=nil { return a.Value.GetStringValue() }
		}
	}
	return ""
}

func description(d *protobufs.AgentDescription) *gormrepo.Description {
	if d==nil { return nil }
	kind:=descriptionValue(d,"agent.kind")
	if kind=="" {
		switch descriptionValue(d,"telemetry.sdk.language") {
		case "java": kind="java-apm"
		case "go": kind="go-apm"
		case "python": kind="python-apm"
		default: kind="unknown"
		}
	}
	return &gormrepo.Description{
		Kind:kind,
		Distribution:descriptionValue(d,"telemetry.distro.name"),
		DistributionVersion:descriptionValue(d,"telemetry.distro.version"),
		Service:descriptionValue(d,"service.name"),
		Namespace:descriptionValue(d,"service.namespace"),
		Environment:descriptionValue(d,"deployment.environment.name"),
		ServiceVersion:descriptionValue(d,"service.version"),
	}
}

func (r *registry) snapshot(rec gormrepo.AgentRecord, now time.Time) map[string]any {
	id:=key(rec.TenantID,rec.InstanceUID)
	r.mu.Lock()
	rt:=r.sessions[id]
	connected:=false
	desiredHash:=""
	if rt!=nil {
		connected=rt.conn!=nil && rec.LastTransport=="websocket"
		desiredHash=rt.desiredHash
	}
	r.mu.Unlock()
	if rec.LastTransport=="http" && now.Sub(rec.LastSeen)<=r.pollTTL && !rec.LastSeen.IsZero() {
		connected=true // freshness estimate, not a persistent HTTP socket
	}
	status:=rec.ConfigStatus
	if desiredHash!="" && desiredHash!=rec.ReportedHash { status="PENDING" }
	return map[string]any{
		"instance_uid":rec.InstanceUID,
		"tenant_id":rec.TenantID,
		"service_name":rec.ServiceName,
		"service_namespace":rec.ServiceNamespace,
		"environment":rec.Environment,
		"service_version":rec.ServiceVersion,
		"agent_version":rec.DistributionVersion,
		"agent_kind":rec.AgentKind,
		"distribution":rec.Distribution,
		"distribution_version":rec.DistributionVersion,
		"connected":connected,
		"transport":rec.LastTransport,
		"last_seen":rec.LastSeen,
		"healthy":rec.Healthy,
		"capabilities":uint64(rec.Capabilities),
		"desired_config_hash":desiredHash,
		"reported_config_hash":rec.ReportedHash,
		"config_status":status,
		"config_error":rec.ConfigError,
	}
}

func (r *registry) list(ctx context.Context, p auth.Principal) ([]map[string]any,error) {
	items,err:=r.store.List(ctx,p.TenantID)
	if err!=nil{return nil,err}
	sort.Slice(items,func(i,j int)bool{return items[i].InstanceUID<items[j].InstanceUID})
	out:=make([]map[string]any,0,len(items))
	for _,item:=range items { out=append(out,r.snapshot(item,time.Now().UTC())) }
	return out,nil
}

func (r *registry) get(ctx context.Context,p auth.Principal,uid string) (map[string]any,error) {
	item,err:=r.store.Get(ctx,p.TenantID,uid)
	if err!=nil{return nil,err}
	return r.snapshot(item,time.Now().UTC()),nil
}

// update synchronously persists observations before acknowledging OpAMP status.
// The lock serializes ownership checks and observations within a P1 process.
// P2 can move persistence out of this critical section with per-Agent fencing.
func (r *registry) update(ctx context.Context,p auth.Principal,isWS bool,conn types.Connection,msg *protobufs.AgentToServer) (*protobufs.AgentRemoteConfig,bool,error) {
	if msg==nil || len(msg.InstanceUid)!=16 {return nil,false,fmt.Errorf("instance_uid must be 16 bytes")}
	id:=hex.EncodeToString(msg.InstanceUid)
	k:=key(p.TenantID,id)
	r.mu.Lock()
	defer r.mu.Unlock()
	rt:=r.sessions[k]
	if rt==nil {rt=&runtimeAgent{}}
	if isWS && rt.conn!=nil && rt.conn!=conn {return nil,false,fmt.Errorf("concurrent session for agent %s",id)}
	first:=rt.sequence==0
	full:=(first && msg.AgentDescription==nil) || (rt.sequence!=0 && msg.SequenceNum!=rt.sequence+1)
	transport:="http"
	if isWS {transport="websocket"}
	var health *bool
	if msg.Health!=nil {v:=msg.Health.Healthy;health=&v}
	var status *gormrepo.ConfigStatus
	if msg.RemoteConfigStatus!=nil {
		raw:=msg.RemoteConfigStatus
		status=&gormrepo.ConfigStatus{Hash:hex.EncodeToString(raw.LastRemoteConfigHash),Status:strings.TrimPrefix(raw.Status.String(),"RemoteConfigStatuses_"),Error:raw.ErrorMessage}
	}
	_,err:=r.store.Observe(ctx,p,id,gormrepo.Observation{
		SeenAt:time.Now().UTC(),
		Transport:transport,
		Capabilities:msg.Capabilities,
		Description:description(msg.AgentDescription),
		Healthy:health,
		Config:status,
	})
	if err!=nil{return nil,false,err}
	rt.sequence=msg.SequenceNum
	if isWS {rt.conn=conn}
	if status!=nil && rt.desiredHash!="" {
		if status.Hash==rt.desiredHash {rt.configStatus=status.Status} else {rt.configStatus="PENDING"}
	}
	r.sessions[k]=rt
	if rt.remote==nil || msg.Capabilities&uint64(protobufs.AgentCapabilities_AgentCapabilities_AcceptsRemoteConfig)==0 {
		return nil,full,nil
	}
	if msg.RemoteConfigStatus!=nil && bytes.Equal(msg.RemoteConfigStatus.LastRemoteConfigHash,rt.remote.ConfigHash) {
		return nil,full,nil
	}
	return rt.remote,full,nil
}

func (r *registry) disconnect(p auth.Principal,conn types.Connection) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for k,rt:=range r.sessions {
		if strings.HasPrefix(k,p.TenantID+"/") && rt.conn==conn {rt.conn=nil}
	}
}

func samplingConfig(rate float64) (*protobufs.AgentRemoteConfig,error) {
	if rate!=rate || rate<0 || rate>1 {return nil,fmt.Errorf("sampling_rate must be in [0, 1]")}
	payload:=map[string]any{"schema_version":1,"sampling":map[string]any{
		"type":"parent_based_trace_id_ratio","rate":rate,
	}}
	body,err:=json.Marshal(payload)
	if err!=nil{return nil,err}
	hash:=sha256.Sum256(body)
	return &protobufs.AgentRemoteConfig{
		Config:&protobufs.AgentConfigMap{ConfigMap:map[string]*protobufs.AgentConfigObject{
			"apm/sampling.json":{Body:body,ContentType:"application/json"},
		}},
		ConfigHash:hash[:],
	},nil
}

func (r *registry) assign(ctx context.Context,p auth.Principal,uid string,rate float64) (map[string]any,error) {
	cfg,err:=samplingConfig(rate)
	if err!=nil{return nil,err}
	record,err:=r.store.Get(ctx,p.TenantID,uid)
	if err!=nil{return nil,err}
	r.mu.Lock()
	k:=key(p.TenantID,uid)
	rt:=r.sessions[k]
	if rt==nil {rt=&runtimeAgent{};r.sessions[k]=rt}
	rt.remote=cfg
	rt.desiredHash=hex.EncodeToString(cfg.ConfigHash)
	rt.configStatus="PENDING"
	r.mu.Unlock()
	// P1 preserves the existing demo's ephemeral per-instance sampling API;
	// immutable revisions, durable Desired and WS proactive push belong to P2.
	return r.snapshot(record,time.Now().UTC()),nil
}

func response(w http.ResponseWriter,code int,body any){
	w.Header().Set("Content-Type","application/json")
	w.WriteHeader(code)
	_ =json.NewEncoder(w).Encode(body)
}

func adminHandler(reg *registry,creds *auth.Authenticator) http.Handler {
	mux:=http.NewServeMux()
	mux.HandleFunc("GET /api/v1/agents",func(w http.ResponseWriter,req *http.Request){
		p,ok:=creds.Authenticate(req.Header.Get("Authorization"))
		if !ok {http.Error(w,"unauthorized",http.StatusUnauthorized);return}
		data,err:=reg.list(req.Context(),p)
		if err!=nil {http.Error(w,"storage unavailable",http.StatusServiceUnavailable);return}
		response(w,http.StatusOK,data)
	})
	mux.HandleFunc("GET /api/v1/agents/{uid}",func(w http.ResponseWriter,req *http.Request){
		p,ok:=creds.Authenticate(req.Header.Get("Authorization"))
		if !ok {http.Error(w,"unauthorized",http.StatusUnauthorized);return}
		uid:=req.PathValue("uid")
		if !validUID(uid) {http.Error(w,"invalid uid",http.StatusBadRequest);return}
		item,err:=reg.get(req.Context(),p,uid)
		if errors.Is(err,gormrepo.ErrNotFound) {http.Error(w,"agent not found",http.StatusNotFound);return}
		if err!=nil {http.Error(w,"storage unavailable",http.StatusServiceUnavailable);return}
		response(w,http.StatusOK,item)
	})
	mux.HandleFunc("PUT /api/v1/agents/{uid}/sampling",func(w http.ResponseWriter,req *http.Request){
		p,ok:=creds.Authenticate(req.Header.Get("Authorization"))
		if !ok {http.Error(w,"unauthorized",http.StatusUnauthorized);return}
		uid:=req.PathValue("uid")
		if !validUID(uid) {http.Error(w,"invalid uid",http.StatusBadRequest);return}
		var payload struct{ SamplingRate *float64 `json:"sampling_rate"` }
		decoder:=json.NewDecoder(io.LimitReader(req.Body,64<<10))
		decoder.DisallowUnknownFields()
		if err:=decoder.Decode(&payload);err!=nil||payload.SamplingRate==nil {http.Error(w,"invalid sampling_rate JSON",http.StatusBadRequest);return}
		var trailing any
		if err:=decoder.Decode(&trailing);!errors.Is(err,io.EOF) {http.Error(w,"unexpected trailing JSON",http.StatusBadRequest);return}
		item,err:=reg.assign(req.Context(),p,uid,*payload.SamplingRate)
		if errors.Is(err,gormrepo.ErrNotFound){http.Error(w,"agent not found",http.StatusNotFound);return}
		if err!=nil {
			if strings.Contains(err.Error(),"sampling_rate") {http.Error(w,err.Error(),http.StatusBadRequest);return}
			http.Error(w,"storage unavailable",http.StatusServiceUnavailable);return
		}
		response(w,http.StatusOK,item)
	})
	return mux
}

func makeGateway(reg *registry,agentAuth *auth.Authenticator,log *slog.Logger) (opampsrv.HTTPHandlerFunc,opampsrv.ConnContext,error) {
	gateway:=opampsrv.New(nil)
	return gateway.Attach(opampsrv.Settings{MaxMessageSize:4<<20,Callbacks:types.Callbacks{
		OnConnecting:func(req *http.Request) types.ConnectionResponse {
			principal,ok:=agentAuth.Authenticate(req.Header.Get("Authorization"))
			if !ok {return types.ConnectionResponse{Accept:false,HTTPStatusCode:http.StatusUnauthorized}}
			isWS:=websocket.IsWebSocketUpgrade(req)
			return types.ConnectionResponse{Accept:true,ConnectionCallbacks:types.ConnectionCallbacks{
				OnConnectionClose:func(conn types.Connection){if isWS {reg.disconnect(principal,conn)}},
				OnMessage:func(ctx context.Context,conn types.Connection,msg *protobufs.AgentToServer)*protobufs.ServerToAgent{
					out:=&protobufs.ServerToAgent{InstanceUid:msg.InstanceUid,Capabilities:uint64(
						protobufs.ServerCapabilities_ServerCapabilities_AcceptsStatus |
						protobufs.ServerCapabilities_ServerCapabilities_OffersRemoteConfig)}
					config,full,err:=reg.update(ctx,principal,isWS,conn,msg)
					if err!=nil {log.Warn("opamp status rejected","error",err);return out}
					if full {out.Flags=uint64(protobufs.ServerToAgentFlags_ServerToAgentFlags_ReportFullState)}
					out.RemoteConfig=config
					return out
				},
				OnReadMessageError:func(_ types.Connection,_ int,_ []byte,e error){log.Warn("opamp read error","error",e)},
				OnMessageResponseError:func(_ types.Connection,_ *protobufs.ServerToAgent,e error){log.Warn("opamp send error","error",e)},
			}}
		},
	}})
}

func main() {
	log:=slog.New(slog.NewJSONHandler(os.Stdout,nil))
	cfg,err:=appconfig.Load()
	if err!=nil {log.Error("invalid startup configuration","error",err);os.Exit(1)}
	connectCtx,cancel:=context.WithTimeout(context.Background(),10*time.Second)
	store,err:=gormrepo.Open(connectCtx,cfg.Database)
	cancel()
	if err!=nil {log.Error("database not ready","error",err);os.Exit(1)}
	defer func(){if err:=store.Close();err!=nil {log.Error("database close failed","error",err)}}()
	reg:=newRegistry(store,cfg.HTTPPollingTTL)
	handler,connCtx,err:=makeGateway(reg,auth.New(cfg.AgentCredentials),log)
	if err!=nil {log.Error("opamp attach failed","error",err);os.Exit(1)}
	mux:=http.NewServeMux()
	mux.HandleFunc("GET /healthz",func(w http.ResponseWriter,_ *http.Request){response(w,200,map[string]string{"status":"ok"})})
	mux.HandleFunc("/v1/opamp",handler)
	mux.Handle("/api/",adminHandler(reg,auth.New(cfg.AdminCredentials)))
	srv:=&http.Server{Addr:cfg.ListenAddress,Handler:mux,ConnContext:connCtx,ReadHeaderTimeout:cfg.ReadHeaderTimeout}
	ctx,stop:=signal.NotifyContext(context.Background(),os.Interrupt,syscall.SIGTERM)
	defer stop()
	go func(){
		<-ctx.Done()
		timeout,cancel:=context.WithTimeout(context.Background(),5*time.Second)
		defer cancel()
		_ =srv.Shutdown(timeout)
	}()
	log.Info("starting opamp control plane","address",cfg.ListenAddress,"database_driver",cfg.Database.Driver)
	if err:=srv.ListenAndServe();err!=nil && !errors.Is(err,http.ErrServerClosed) {
		log.Error("server failed","error",err)
	}
}
