package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/XiaoWeiKIN/apm-control-plane/internal/auth"
	"github.com/XiaoWeiKIN/apm-control-plane/internal/policy"
	"github.com/XiaoWeiKIN/apm-control-plane/internal/storage/gormrepo"
)

type policyStore interface {
	CreatePolicy(context.Context, string, string, string, float64) (gormrepo.PolicyRecord, gormrepo.PolicyRevisionRecord, error)
	CreatePolicyRevision(context.Context, string, string, string, float64) (gormrepo.PolicyRevisionRecord, error)
	CreatePolicyBinding(context.Context, string, string, gormrepo.BindingInput) (gormrepo.PolicyBindingRecord, error)
	UpdatePolicyBinding(context.Context, string, string, string, gormrepo.BindingInput) (gormrepo.PolicyBindingRecord, error)
	ResolveAgentPolicy(context.Context, string, string) (gormrepo.PolicyResolution, error)
}

// policyAPI exposes workspace-scoped policy authoring and read-only Agent resolution.
func policyAPI(store policyStore, credentials *auth.Authenticator) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		// 1. 从可信 Admin Credential 取得 Workspace；无认证不得读取 Agent/Policy。
		principal, ok := credentials.Authenticate(req.Header.Get("Authorization"))
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		// 2. 将具体管理命令映射到已有 GORM 事务接口。
		var result any
		statusCode := http.StatusCreated
		var err error
		switch {
		case req.Method == http.MethodPost && req.URL.Path == "/api/v1/policies":
			var body struct {
				Name         string   `json:"name"`
				SamplingRate *float64 `json:"sampling_rate"`
			}
			if !decodePolicyRequest(w, req, &body) {
				return
			}
			if body.SamplingRate == nil {
				http.Error(w, "sampling_rate is required", http.StatusBadRequest)
				return
			}
			var p gormrepo.PolicyRecord
			var rev gormrepo.PolicyRevisionRecord
			p, rev, err = store.CreatePolicy(req.Context(), principal.WorkspaceID, principal.CredentialID, body.Name, *body.SamplingRate)
			result = map[string]any{"policy": p, "revision": rev}
		case req.Method == http.MethodPost && req.PathValue("policy_id") != "":
			var body struct {
				SamplingRate *float64 `json:"sampling_rate"`
			}
			if !decodePolicyRequest(w, req, &body) {
				return
			}
			if body.SamplingRate == nil {
				http.Error(w, "sampling_rate is required", http.StatusBadRequest)
				return
			}
			result, err = store.CreatePolicyRevision(req.Context(), principal.WorkspaceID,
				principal.CredentialID, req.PathValue("policy_id"), *body.SamplingRate)
		case req.Method == http.MethodPost && req.URL.Path == "/api/v1/policy-bindings":
			var body gormrepo.BindingInput
			if !decodePolicyRequest(w, req, &body) {
				return
			}
			result, err = store.CreatePolicyBinding(req.Context(), principal.WorkspaceID, principal.CredentialID, body)
		case req.Method == http.MethodPut && req.PathValue("binding_id") != "":
			var body gormrepo.BindingInput
			if !decodePolicyRequest(w, req, &body) {
				return
			}
			result, err = store.UpdatePolicyBinding(req.Context(), principal.WorkspaceID,
				principal.CredentialID, req.PathValue("binding_id"), body)
			statusCode = http.StatusOK
		case req.Method == http.MethodGet && req.PathValue("uid") != "":
			uid := req.PathValue("uid")
			if !validUID(uid) {
				http.Error(w, "invalid uid", http.StatusBadRequest)
				return
			}
			result, err = store.ResolveAgentPolicy(req.Context(), principal.WorkspaceID, uid)
			statusCode = http.StatusOK
		default:
			http.NotFound(w, req)
			return
		}

		// 3. 统一映射业务拒绝，Resolver 不执行 Release/OpAMP 网络写入。
		switch {
		case errors.Is(err, policy.ErrInvalid):
			http.Error(w, "invalid policy or selector", http.StatusBadRequest)
		case errors.Is(err, policy.ErrNotFound), errors.Is(err, gormrepo.ErrNotFound):
			http.Error(w, "policy or agent not found", http.StatusNotFound)
		case errors.Is(err, policy.ErrConflict):
			http.Error(w, "policy binding conflict", http.StatusConflict)
		case errors.Is(err, gormrepo.ErrUnsupportedReleaseTarget):
			http.Error(w, "unsupported agent kind", http.StatusUnprocessableEntity)
		case err != nil:
			http.Error(w, "storage unavailable", http.StatusServiceUnavailable)
		default:
			response(w, statusCode, result)
		}
	})
}

func decodePolicyRequest(w http.ResponseWriter, req *http.Request, value any) bool {
	d := json.NewDecoder(io.LimitReader(req.Body, 64<<10))
	d.DisallowUnknownFields()
	if err := d.Decode(value); err != nil {
		http.Error(w, "invalid policy JSON", http.StatusBadRequest)
		return false
	}
	var trailing any
	if err := d.Decode(&trailing); !errors.Is(err, io.EOF) {
		http.Error(w, "unexpected trailing JSON", http.StatusBadRequest)
		return false
	}
	return true
}
