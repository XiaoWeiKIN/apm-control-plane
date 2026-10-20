//go:build integration

package gormrepo

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/XiaoWeiKIN/apm-control-plane/internal/appconfig"
	"github.com/XiaoWeiKIN/apm-control-plane/internal/auth"
)

func newUID(t *testing.T) string {
	t.Helper()
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(raw)
}

func TestRepositoryContractPostgresAndMySQL(t *testing.T) {
	cases := []struct{ driver, env string }{
		{"postgres", "APM_TEST_POSTGRES_DSN"},
		{"mysql", "APM_TEST_MYSQL_DSN"},
	}
	for _, tt := range cases {
		t.Run(tt.driver, func(t *testing.T) {
			dsn := os.Getenv(tt.env)
			if dsn == "" {
				t.Skipf("%s not configured", tt.env)
			}
			ctx := context.Background()
			cfg := appconfig.Database{Driver: tt.driver, DSN: dsn, MaxOpen: 5, MaxIdle: 2, MaxLifetime: time.Minute}
			store, err := Open(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			uid := newUID(t)
			p := auth.Principal{WorkspaceID: "team-alpha", CredentialID: "cred-a"}
			obs := Observation{
				SeenAt: time.Now().UTC(), Transport: "http", Capabilities: 3,
				Description: &Description{Kind: "java-apm", Distribution: "example-java", DistributionVersion: "1.2.3", Service: "checkout", ServiceVersion: "9.4.1"},
			}
			record, err := store.Observe(ctx, p, uid, obs)
			if err != nil {
				t.Fatal(err)
			}
			if record.AgentKind != "java-apm" || record.ServiceVersion != "9.4.1" || record.DistributionVersion != "1.2.3" {
				t.Fatalf("bad first observation: %+v", record)
			}
			// OpAMP compression means a message without AgentDescription cannot wipe identity fields.
			healthy := true
			updated, err := store.Observe(ctx, p, uid, Observation{SeenAt: time.Now().UTC(), Transport: "http", Capabilities: 3, Healthy: &healthy})
			if err != nil {
				t.Fatal(err)
			}
			if updated.ServiceName != "checkout" || updated.Healthy == nil || !*updated.Healthy {
				t.Fatalf("lost compressed state: %+v", updated)
			}
			// UID ownership stays bound to the credential that registered it.
			_, err = store.Observe(ctx, auth.Principal{WorkspaceID: "team-alpha", CredentialID: "cred-b"}, uid, obs)
			if !errors.Is(err, ErrOwnership) {
				t.Fatalf("wrong identity collision: %v", err)
			}
			// A different workspace must never see or overwrite this agent.
			if _, err := store.Get(ctx, "team-beta", uid); !errors.Is(err, ErrNotFound) {
				t.Fatalf("cross-workspace read: %v", err)
			}
			list, err := store.List(ctx, "team-beta")
			if err != nil || len(list) != 0 {
				t.Fatalf("cross-workspace listing: %+v %v", list, err)
			}
			status := &ConfigStatus{Hash: "aabbcc", Status: "APPLIED"}
			_, err = store.Observe(ctx, p, uid, Observation{SeenAt: time.Now().UTC(), Transport: "http", Capabilities: 3, Config: status})
			if err != nil {
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			store, err = Open(ctx, cfg) // simulate control-plane restarting
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			restored, err := store.Get(ctx, p.WorkspaceID, uid)
			if err != nil {
				t.Fatal(err)
			}
			if restored.ReportedHash != "aabbcc" || restored.ConfigStatus != "APPLIED" || restored.ServiceName != "checkout" || restored.LastSeen.IsZero() {
				t.Fatalf("persistence lost: %+v", restored)
			}
		})
	}
}

func TestInvalidObservationNeverPersists(t *testing.T) {
	for _, tt := range []struct{ driver, env string }{
		{"postgres", "APM_TEST_POSTGRES_DSN"},
		{"mysql", "APM_TEST_MYSQL_DSN"},
	} {
		t.Run(tt.driver, func(t *testing.T) {
			dsn := os.Getenv(tt.env)
			if dsn == "" {
				t.Skip(tt.env + " not configured")
			}
			ctx := context.Background()
			store, err := Open(ctx, appconfig.Database{Driver: tt.driver, DSN: dsn, MaxOpen: 3, MaxIdle: 1, MaxLifetime: time.Minute})
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			p := auth.Principal{WorkspaceID: "test-workspace", CredentialID: "test-credential"}
			for _, tc := range []struct {
				name string
				obs  Observation
			}{
				{"oversized service", Observation{SeenAt: time.Now().UTC(), Transport: "http", Description: &Description{Kind: "java-apm", Service: strings.Repeat("x", 256)}}},
				{"oversized config error", Observation{SeenAt: time.Now().UTC(), Transport: "http", Config: &ConfigStatus{Status: "FAILED", Error: strings.Repeat("x", 8193)}}},
			} {
				t.Run(tc.name, func(t *testing.T) {
					id := newUID(t)
					if _, err := store.Observe(ctx, p, id, tc.obs); !errors.Is(err, ErrInvalidObservation) {
						t.Fatalf("invalid observation accepted: %v", err)
					}
					if _, err := store.Get(ctx, p.WorkspaceID, id); !errors.Is(err, ErrNotFound) {
						t.Fatalf("invalid observation changed Inventory: %v", err)
					}
				})
			}
		})
	}
}
