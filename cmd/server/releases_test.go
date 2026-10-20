package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/XiaoWeiKIN/apm-control-plane/internal/appconfig"
	"github.com/XiaoWeiKIN/apm-control-plane/internal/auth"
	"github.com/XiaoWeiKIN/apm-control-plane/internal/storage/gormrepo"
)

type fakeReleasePublisher struct {
	calls       int
	workspaceID string
	instanceUID string
	revisionID  string
	actor       string
	key         string
}

func (f *fakeReleasePublisher) PublishAgentConfig(
	_ context.Context, workspaceID, uid, revisionID, actor, key string,
) (gormrepo.ReleaseResult, error) {
	f.calls++
	f.workspaceID = workspaceID
	f.instanceUID = uid
	f.revisionID = revisionID
	f.actor = actor
	f.key = key
	return gormrepo.ReleaseResult{
		WorkspaceID: workspaceID, ReleaseID: "release-1", InstanceUID: uid,
		ConfigRevisionID: revisionID, DesiredGeneration: 1, Status: "PENDING",
	}, nil
}

func TestReleaseHandlerRejectsInvalidRequestsBeforePersistence(t *testing.T) {
	fake := &fakeReleasePublisher{}
	authenticator := auth.New([]appconfig.Credential{
		{ID: "admin-a", WorkspaceID: "workspace-a", Token: "test-admin-token-1234"},
	})
	mux := http.NewServeMux()
	mux.Handle("POST /api/v1/agents/{uid}/releases", adminReleaseHandler(fake, authenticator, nil))
	_, uidHex := uid(14)
	revision := "00000000-0000-4000-8000-000000000001"
	request := func(url, token, body string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(http.MethodPost, url, strings.NewReader(body))
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		r.Header.Set("Idempotency-Key", "request-one")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w
	}
	for _, tc := range []struct {
		name, url, token, body string
		status                 int
	}{
		{"unauthenticated", "/api/v1/agents/" + uidHex + "/releases", "", `{"config_revision_id":"00000000-0000-4000-8000-000000000001"}`, http.StatusUnauthorized},
		{"bad agent UID", "/api/v1/agents/not-uid/releases", "test-admin-token-1234", `{"config_revision_id":"00000000-0000-4000-8000-000000000001"}`, http.StatusBadRequest},
		{"unknown JSON field", "/api/v1/agents/" + uidHex + "/releases", "test-admin-token-1234", `{"config_revision_id":"00000000-0000-4000-8000-000000000001","workspace_id":"other"}`, http.StatusBadRequest},
		{"missing revision", "/api/v1/agents/" + uidHex + "/releases", "test-admin-token-1234", `{}`, http.StatusBadRequest},
		{"trailing JSON", "/api/v1/agents/" + uidHex + "/releases", "test-admin-token-1234", `{"config_revision_id":"00000000-0000-4000-8000-000000000001"} {}`, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := request(tc.url, tc.token, tc.body)
			if res.Code != tc.status || fake.calls != 0 {
				t.Fatalf("invalid request invoked persistence: code=%d calls=%d", res.Code, fake.calls)
			}
		})
	}
	ok := request("/api/v1/agents/"+uidHex+"/releases", "test-admin-token-1234",
		`{"config_revision_id":"00000000-0000-4000-8000-000000000001"}`)
	if ok.Code != http.StatusAccepted || fake.calls != 1 ||
		fake.workspaceID != "workspace-a" || fake.actor != "admin-a" ||
		fake.instanceUID != uidHex || fake.revisionID != revision || fake.key != "request-one" {
		t.Fatalf("release handler bypassed trusted Scope: code=%d fake=%+v", ok.Code, fake)
	}
	var result gormrepo.ReleaseResult
	if err := json.Unmarshal(ok.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Status != "PENDING" || result.DesiredGeneration != 1 {
		t.Fatalf("release response claimed unexpected state: %+v", result)
	}
}
