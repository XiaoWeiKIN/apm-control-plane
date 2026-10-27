//go:build integration

package gormrepo

import (
	"context"
	"errors"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/XiaoWeiKIN/apm-control-plane/internal/appconfig"
	"github.com/XiaoWeiKIN/apm-control-plane/internal/auth"
)

// TestSharedWorkloadCredentialPostgresAndMySQL records the existing Credential
// ownership contract. It does not implement or verify a stable Agent Principal.
func TestSharedWorkloadCredentialPostgresAndMySQL(t *testing.T) {
	for _, tc := range []struct{ driver, env string }{
		{"postgres", "APM_TEST_POSTGRES_DSN"},
		{"mysql", "APM_TEST_MYSQL_DSN"},
	} {
		t.Run(tc.driver, func(t *testing.T) {
			dsn := os.Getenv(tc.env)
			if dsn == "" {
				t.Skip(tc.env + " not configured")
			}
			ctx := context.Background()
			store, err := Open(ctx, appconfig.Database{
				Driver: tc.driver, DSN: dsn, MaxOpen: 5, MaxIdle: 2, MaxLifetime: time.Minute,
			})
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()

			workspace := "workload-" + tc.driver + "-" + newUID(t)
			owner := auth.Principal{WorkspaceID: workspace, CredentialID: "shared-credential"}
			observation := Observation{
				SeenAt: time.Now().UTC(), Transport: "http",
				Description: &Description{Kind: "java-apm", Service: "checkout"},
			}
			uids := []string{newUID(t), newUID(t), newUID(t)}

			// 1. 同一工作负载凭证允许注册多个 UID；Inventory 保持实例独立。
			for _, uid := range uids {
				record, err := store.Observe(ctx, owner, uid, observation)
				if err != nil || record.InstanceUID != uid || record.CredentialID != owner.CredentialID {
					t.Fatalf("shared credential registration %s: %+v %v", uid, record, err)
				}
			}
			agents, err := store.List(ctx, workspace)
			if err != nil || len(agents) != len(uids) {
				t.Fatalf("workload inventory: %+v %v", agents, err)
			}

			// 2. 同 Workspace 的其他凭证不能接管 UID 或修改已有元数据。
			spoofed := Observation{
				SeenAt: time.Now().UTC(), Transport: "http",
				Description: &Description{Kind: "python-apm", Service: "spoofed"},
			}
			_, err = store.Observe(ctx, auth.Principal{
				WorkspaceID: workspace, CredentialID: "other-credential",
			}, uids[0], spoofed)
			if !errors.Is(err, ErrOwnership) {
				t.Fatalf("different credential acquired workload UID: %v", err)
			}
			after, err := store.List(ctx, workspace)
			if err != nil || !reflect.DeepEqual(agents, after) {
				t.Fatalf("workload inventory changed after rejected takeover: before=%+v after=%+v err=%v", agents, after, err)
			}

			// 3. 相同 UID 在另一 Workspace 独立，不能覆盖原 Workspace 的归属。
			foreign := auth.Principal{WorkspaceID: workspace + "-other", CredentialID: "foreign-credential"}
			record, err := store.Observe(ctx, foreign, uids[0], spoofed)
			if err != nil || record.CredentialID != foreign.CredentialID || record.ServiceName != "spoofed" {
				t.Fatalf("same UID in distinct Workspace: %+v %v", record, err)
			}
			original, err := store.Get(ctx, workspace, uids[0])
			if err != nil || original.ServiceName != "checkout" || original.CredentialID != owner.CredentialID {
				t.Fatalf("cross-Workspace UID collision corrupted owner: %+v %v", original, err)
			}
		})
	}
}
