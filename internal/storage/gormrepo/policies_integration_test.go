//go:build integration

package gormrepo

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/XiaoWeiKIN/apm-control-plane/internal/appconfig"
	"github.com/XiaoWeiKIN/apm-control-plane/internal/auth"
	"github.com/XiaoWeiKIN/apm-control-plane/internal/config"
	"github.com/XiaoWeiKIN/apm-control-plane/internal/policy"
	"github.com/open-telemetry/opamp-go/protobufs"
)

func TestPolicyResolutionPostgresAndMySQL(t *testing.T) {
	for _, tt := range []struct{ driver, env string }{
		{"postgres", "APM_TEST_POSTGRES_DSN"},
		{"mysql", "APM_TEST_MYSQL_DSN"},
	} {
		t.Run(tt.driver, func(t *testing.T) {
			dsn := os.Getenv(tt.env)
			if dsn == "" {
				t.Skip(tt.env + " is not set")
			}
			ctx := context.Background()
			store, err := Open(ctx, appconfig.Database{Driver: tt.driver, DSN: dsn, MaxOpen: 10, MaxIdle: 2, MaxLifetime: time.Minute})
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()

			p := auth.Principal{OrgID: fixtureOrgID("policy-resolution-contract"), CredentialID: "java-owner"}
			id := newUID(t)
			capability := uint64(protobufs.AgentCapabilities_AgentCapabilities_AcceptsRemoteConfig)
			if _, err := store.Observe(ctx, p, id, Observation{SeenAt: time.Now().UTC(), Transport: "http",
				Capabilities: capability, Description: &Description{Kind: config.JavaAgentKind, Service: "checkout", Environment: "prod"}}); err != nil {
				t.Fatal(err)
			}

			// Org must be scoped by trusted principal, never by the selector.
			if _, err := store.ResolveAgentPolicy(ctx, "other-org", id); !errors.Is(err, ErrNotFound) {
				t.Fatalf("cross Org inventory access: %v", err)
			}
			if _, err := store.ResolveAgentPolicy(ctx, p.OrgID, id); !errors.Is(err, policy.ErrNotFound) {
				t.Fatalf("unexpected policy without Binding: %v", err)
			}

			_, base, err := store.CreatePolicy(ctx, p.OrgID, "admin", "default", 0.1)
			if err != nil {
				t.Fatal(err)
			}
			_, env, err := store.CreatePolicy(ctx, p.OrgID, "admin", "production", 0.2)
			if err != nil {
				t.Fatal(err)
			}
			_, service, err := store.CreatePolicy(ctx, p.OrgID, "admin", "checkout", 0.3)
			if err != nil {
				t.Fatal(err)
			}
			_, contender, err := store.CreatePolicy(ctx, p.OrgID, "admin", "collision", 0.4)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.CreatePolicyRevision(ctx, "other-org", "admin", base.PolicyID, 0.5); !errors.Is(err, policy.ErrNotFound) {
				t.Fatalf("cross-org revision creation: %v", err)
			}
			createBinding := func(revisionID, scope string, selector policy.Selector, priority int) PolicyBindingRecord {
				t.Helper()
				b, err := store.CreatePolicyBinding(ctx, p.OrgID, "admin", BindingInput{
					PolicyRevisionID: revisionID, ScopeType: scope, Selector: selector, Priority: priority,
				})
				if err != nil {
					t.Fatal(err)
				}
				return b
			}
			org := createBinding(base.PolicyRevisionID, policy.ScopeOrg, policy.Selector{}, 100)
			envBinding := createBinding(env.PolicyRevisionID, policy.ScopeEnvironment, policy.Selector{Environment: "prod"}, 0)
			serviceBinding := createBinding(service.PolicyRevisionID, policy.ScopeService, policy.Selector{ServiceName: "checkout"}, 1)
			resolve := func(want string) {
				t.Helper()
				got, err := store.ResolveAgentPolicy(ctx, p.OrgID, id)
				if err != nil || got.PolicyRevisionID != want {
					t.Fatalf("resolved revision %+v, want %s: %v", got, want, err)
				}
				if len(got.Body) == 0 || got.BodySHA256 == "" {
					t.Fatalf("missing immutable body: %+v", got)
				}
			}
			resolve(service.PolicyRevisionID)
			other := createBinding(contender.PolicyRevisionID, policy.ScopeService, policy.Selector{ServiceName: "checkout", Environment: "prod"}, 1)
			if _, err := store.ResolveAgentPolicy(ctx, p.OrgID, id); !errors.Is(err, policy.ErrConflict) {
				t.Fatalf("ambiguous top priority did not fail: %v", err)
			}
			// A strictly higher priority resolves an otherwise ambiguous lower level.
			higher := createBinding(service.PolicyRevisionID, policy.ScopeService, policy.Selector{ServiceName: "checkout"}, 2)
			resolve(service.PolicyRevisionID)
			// Missing or cross-Org bindings must not be reported as stale CAS conflicts.
			update := BindingInput{
				PolicyRevisionID: service.PolicyRevisionID, ScopeType: policy.ScopeService,
				Selector: policy.Selector{ServiceName: "checkout"}, Priority: 1, Enabled: true, RowVersion: 1,
			}
			if _, err := store.UpdatePolicyBinding(ctx, p.OrgID, "admin", "11111111-1111-4111-8111-111111111111", update); !errors.Is(err, policy.ErrNotFound) {
				t.Fatalf("missing binding should be 404: %v", err)
			}
			_, foreignRevision, err := store.CreatePolicy(ctx, "other-org", "admin", "foreign", 0.2)
			if err != nil {
				t.Fatal(err)
			}
			update.PolicyRevisionID = foreignRevision.PolicyRevisionID
			if _, err := store.UpdatePolicyBinding(ctx, "other-org", "admin", serviceBinding.BindingID, update); !errors.Is(err, policy.ErrNotFound) {
				t.Fatalf("cross-Org binding should be 404: %v", err)
			}
			// Concurrent callers cannot overwrite a newer Binding row_version.
			if _, err := store.UpdatePolicyBinding(ctx, p.OrgID, "admin", higher.BindingID, BindingInput{
				PolicyRevisionID: higher.PolicyRevisionID, ScopeType: higher.ScopeType, Selector: policy.Selector{ServiceName: "checkout"},
				Priority: higher.Priority, Enabled: false, RowVersion: higher.RowVersion,
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := store.UpdatePolicyBinding(ctx, p.OrgID, "admin", higher.BindingID, BindingInput{
				PolicyRevisionID: higher.PolicyRevisionID, ScopeType: higher.ScopeType, Selector: policy.Selector{ServiceName: "checkout"},
				Priority: higher.Priority, Enabled: true, RowVersion: higher.RowVersion,
			}); !errors.Is(err, policy.ErrConflict) {
				t.Fatalf("stale Binding CAS not rejected: %v", err)
			}
			if _, err := store.ResolveAgentPolicy(ctx, p.OrgID, id); !errors.Is(err, policy.ErrConflict) {
				t.Fatalf("conflicting fallback not restored: %v", err)
			}
			if _, err := store.UpdatePolicyBinding(ctx, p.OrgID, "admin", other.BindingID, BindingInput{
				PolicyRevisionID: other.PolicyRevisionID, ScopeType: other.ScopeType,
				Selector: policy.Selector{ServiceName: "checkout", Environment: "prod"},
				Priority: 1, Enabled: false, RowVersion: 1,
			}); err != nil {
				t.Fatal(err)
			}
			resolve(service.PolicyRevisionID)
			for _, b := range []PolicyBindingRecord{serviceBinding, envBinding, org} {
				selectors := map[string]policy.Selector{
					policy.ScopeService:     {ServiceName: "checkout"},
					policy.ScopeEnvironment: {Environment: "prod"},
					policy.ScopeOrg:         {},
				}
				if _, err := store.UpdatePolicyBinding(ctx, p.OrgID, "admin", b.BindingID, BindingInput{
					PolicyRevisionID: b.PolicyRevisionID, ScopeType: b.ScopeType, Selector: selectors[b.ScopeType],
					Priority: b.Priority, Enabled: false, RowVersion: b.RowVersion,
				}); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := store.ResolveAgentPolicy(ctx, p.OrgID, id); !errors.Is(err, policy.ErrNotFound) {
				t.Fatalf("disabled rules still matched: %v", err)
			}

			// Multiple writers serialize through the parent Policy row lock.
			var wg sync.WaitGroup
			numbers := make(chan int64, 5)
			errs := make(chan error, 5)
			for i := 0; i < 5; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					rev, err := store.CreatePolicyRevision(ctx, p.OrgID, "admin", base.PolicyID, 0.6)
					if err != nil {
						errs <- err
						return
					}
					numbers <- rev.RevisionNumber
				}()
			}
			wg.Wait()
			close(errs)
			close(numbers)
			for err := range errs {
				t.Fatalf("concurrent revision failed: %v", err)
			}
			seen := map[int64]bool{}
			for number := range numbers {
				seen[number] = true
			}
			for i := int64(2); i <= 6; i++ {
				if !seen[i] {
					t.Fatalf("missing concurrent PolicyRevision %d: %+v", i, seen)
				}
			}
		})
	}
}
