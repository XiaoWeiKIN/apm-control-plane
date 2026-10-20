package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/XiaoWeiKIN/apm-control-plane/internal/auth"
	"github.com/XiaoWeiKIN/apm-control-plane/internal/storage/gormrepo"
)

type releasePublisher interface {
	PublishAgentConfig(context.Context, string, string, string, string, string) (gormrepo.ReleaseResult, error)
}

// adminReleaseHandler persists release intent; actual OpAMP delivery belongs to P2.3.
func adminReleaseHandler(publisher releasePublisher, credentials *auth.Authenticator) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		// 1. 认证 Workspace 与校验 UID/请求结构，非法输入不触发数据库写入。
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
		var command struct {
			ConfigRevisionID string `json:"config_revision_id"`
		}
		decoder := json.NewDecoder(io.LimitReader(req.Body, 4<<10))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&command); err != nil || len(command.ConfigRevisionID) != 36 {
			http.Error(w, "invalid config_revision_id", http.StatusBadRequest)
			return
		}
		var trailing any
		if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
			http.Error(w, "unexpected trailing JSON", http.StatusBadRequest)
			return
		}

		// 2. 原子创建 Release/Target/Assignment/Event；成功仅代表 Desired 已提交。
		result, err := publisher.PublishAgentConfig(req.Context(), principal.WorkspaceID, uid,
			command.ConfigRevisionID, principal.CredentialID, req.Header.Get("Idempotency-Key"))
		switch {
		case errors.Is(err, gormrepo.ErrNotFound), errors.Is(err, gormrepo.ErrConfigRevisionNotFound):
			http.Error(w, "agent or config revision not found", http.StatusNotFound)
		case errors.Is(err, gormrepo.ErrUnsupportedReleaseTarget):
			http.Error(w, "agent does not support the selected config revision", http.StatusUnprocessableEntity)
		case errors.Is(err, gormrepo.ErrInvalidReleaseRequest):
			http.Error(w, "invalid release request", http.StatusBadRequest)
		case errors.Is(err, gormrepo.ErrReleaseConflict), errors.Is(err, gormrepo.ErrAssignmentConflict):
			http.Error(w, "release conflict", http.StatusConflict)
		case err != nil:
			http.Error(w, "storage unavailable", http.StatusServiceUnavailable)
		default:
			response(w, http.StatusAccepted, result)
		}
	})
}
