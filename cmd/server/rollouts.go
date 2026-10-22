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

type rolloutStore interface {
	PublishRollout(context.Context, string, string, string, []string, int, string) (gormrepo.RolloutView, error)
	AdvanceRollout(context.Context, string, string, string) (gormrepo.RolloutView, error)
	GetRollout(context.Context, string, string) (gormrepo.RolloutView, error)
}

// adminRolloutHandler manages explicit, bounded multi-Agent batches without
// performing any network sends inside the database transaction.
func adminRolloutHandler(store rolloutStore, credentials *auth.Authenticator, onPublished func(string, string)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		// 1. 先授权 Workspace；不允许 Agent 自报身份或跨作用域查询。
		principal, ok := credentials.Authenticate(req.Header.Get("Authorization"))
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var result gormrepo.RolloutView
		var err error
		code := http.StatusOK
		switch {
		case req.Method == http.MethodPost && req.URL.Path == "/api/v1/rollouts":
			var body struct {
				ConfigRevisionID string   `json:"config_revision_id"`
				InstanceUIDs     []string `json:"instance_uids"`
				BatchSize        int      `json:"batch_size"`
			}
			decoder := json.NewDecoder(http.MaxBytesReader(w, req.Body, 4<<10))
			decoder.DisallowUnknownFields()
			if decodeErr := decoder.Decode(&body); decodeErr != nil {
				http.Error(w, "invalid rollout JSON", http.StatusBadRequest)
				return
			}
			var trailing any
			if decodeErr := decoder.Decode(&trailing); !errors.Is(decodeErr, io.EOF) {
				http.Error(w, "unexpected trailing JSON", http.StatusBadRequest)
				return
			}
			result, err = store.PublishRollout(req.Context(), principal.WorkspaceID, principal.CredentialID,
				body.ConfigRevisionID, body.InstanceUIDs, body.BatchSize, req.Header.Get("Idempotency-Key"))
			code = http.StatusAccepted
		case req.Method == http.MethodPost && req.PathValue("release_id") != "":
			result, err = store.AdvanceRollout(req.Context(), principal.WorkspaceID,
				req.PathValue("release_id"), principal.CredentialID)
		case req.Method == http.MethodGet && req.PathValue("release_id") != "":
			result, err = store.GetRollout(req.Context(), principal.WorkspaceID, req.PathValue("release_id"))
		default:
			http.NotFound(w, req)
			return
		}

		// 2. 只在成功提交后通知被激活的在线 Agent。
		switch {
		case errors.Is(err, gormrepo.ErrInvalidRollout):
			http.Error(w, "invalid rollout request", http.StatusBadRequest)
		case errors.Is(err, gormrepo.ErrNotFound), errors.Is(err, gormrepo.ErrConfigRevisionNotFound),
			errors.Is(err, gormrepo.ErrRolloutNotFound):
			http.Error(w, "release, agent or config revision not found", http.StatusNotFound)
		case errors.Is(err, gormrepo.ErrUnsupportedReleaseTarget):
			http.Error(w, "one or more targets do not support the config", http.StatusUnprocessableEntity)
		case errors.Is(err, gormrepo.ErrRolloutBlocked), errors.Is(err, gormrepo.ErrRolloutNotReady),
			errors.Is(err, gormrepo.ErrReleaseConflict), errors.Is(err, gormrepo.ErrAssignmentConflict):
			http.Error(w, "rollout conflict or batch not ready", http.StatusConflict)
		case err != nil:
			http.Error(w, "storage unavailable", http.StatusServiceUnavailable)
		default:
			if onPublished != nil {
				for _, uid := range result.Activated {
					onPublished(principal.WorkspaceID, uid)
				}
			}
			if result.Status == "PAUSED" {
				code = http.StatusConflict
			}
			response(w, code, result)
		}
	})
}
