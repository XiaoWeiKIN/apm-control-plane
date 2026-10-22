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

type policyReleasePublisher interface {
	PublishAgentPolicy(context.Context, string, string, string, string, string) (gormrepo.ReleaseResult, error)
}

// adminPolicyReleaseHandler commits the selected policy's ConfigRevision and
// single-Agent release before waking the existing OpAMP delivery path.
func adminPolicyReleaseHandler(publisher policyReleasePublisher, credentials *auth.Authenticator, onPublished func(string, string)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		// 1. 使用可信 Admin Credential 限定 Workspace 与目标 UID。
		principal, ok := credentials.Authenticate(req.Header.Get("Authorization"))
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		uid := req.PathValue("uid")
		if !validUID(uid) {
			http.Error(w, "invalid uid", http.StatusBadRequest)
			return
		}
		var body struct {
			PolicyRevisionID string `json:"policy_revision_id"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, req.Body, 4<<10))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&body); err != nil || len(body.PolicyRevisionID) != 36 {
			http.Error(w, "invalid policy_revision_id", http.StatusBadRequest)
			return
		}
		var trailing any
		if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
			http.Error(w, "unexpected trailing JSON", http.StatusBadRequest)
			return
		}

		// 2. 原子解析、编译、写入发布意图；遇到冲突时没有网络副作用。
		result, err := publisher.PublishAgentPolicy(req.Context(), principal.WorkspaceID, uid,
			body.PolicyRevisionID, principal.CredentialID, req.Header.Get("Idempotency-Key"))
		switch {
		case errors.Is(err, gormrepo.ErrNotFound), errors.Is(err, policy.ErrNotFound):
			http.Error(w, "agent or matching policy not found", http.StatusNotFound)
		case errors.Is(err, gormrepo.ErrUnsupportedReleaseTarget):
			http.Error(w, "unsupported agent kind or capability", http.StatusUnprocessableEntity)
		case errors.Is(err, policy.ErrConflict), errors.Is(err, gormrepo.ErrReleaseConflict),
			errors.Is(err, gormrepo.ErrAssignmentConflict):
			http.Error(w, "policy or release conflict", http.StatusConflict)
		case errors.Is(err, policy.ErrInvalid), errors.Is(err, gormrepo.ErrInvalidReleaseRequest):
			http.Error(w, "invalid policy release", http.StatusBadRequest)
		case err != nil:
			http.Error(w, "storage unavailable", http.StatusServiceUnavailable)
		default:
			// 3. 只在 Desired 事务成功提交后唤醒在线 WS；HTTP 轮询仍按原有契约下发。
			if onPublished != nil {
				onPublished(principal.WorkspaceID, uid)
			}
			response(w, http.StatusAccepted, result)
		}
	})
}
