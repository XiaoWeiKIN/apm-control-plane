package policy

import (
	"errors"
	"testing"
)

func TestSelectorScopesAndCanonicalHash(t *testing.T) {
	a := Agent{InstanceUID: "00112233445566778899aabbccddeeff", Environment: "prod", ServiceName: "checkout"}
	for _, tt := range []struct {
		scope    string
		selector Selector
		matches  bool
	}{
		{ScopeWorkspace, Selector{}, true},
		{ScopeEnvironment, Selector{Environment: "prod"}, true},
		{ScopeEnvironment, Selector{Environment: "dev"}, false},
		{ScopeService, Selector{Environment: "prod", ServiceName: "checkout"}, true},
		{ScopeService, Selector{ServiceName: "checkout"}, true},
		{ScopeService, Selector{ServiceName: "billing"}, false},
		{ScopeInstance, Selector{InstanceUID: a.InstanceUID}, true},
		{ScopeInstance, Selector{InstanceUID: "11112222333344445555666677778888"}, false},
	} {
		t.Run(tt.scope+"/"+tt.selector.ServiceName+tt.selector.Environment, func(t *testing.T) {
			raw, hash, err := tt.selector.Canonical(tt.scope)
			if err != nil || raw == "" || len(hash) != 64 {
				t.Fatalf("canonical selector %q: %q %s %v", tt.scope, raw, hash, err)
			}
			if tt.selector.Matches(tt.scope, a) != tt.matches {
				t.Fatalf("selector match: %s %+v", tt.scope, tt.selector)
			}
			if _, again, err := tt.selector.Canonical(tt.scope); err != nil || again != hash {
				t.Fatalf("unstable selector hash: %s, %s, %v", hash, again, err)
			}
		})
	}
	for _, tt := range []struct {
		scope    string
		selector Selector
	}{
		{ScopeWorkspace, Selector{Environment: "prod"}},
		{ScopeEnvironment, Selector{}},
		{ScopeEnvironment, Selector{Environment: "prod", ServiceName: "checkout"}},
		{ScopeService, Selector{}},
		{ScopeInstance, Selector{InstanceUID: "not-a-uid"}},
		{"TENANT", Selector{}},
	} {
		if _, _, err := tt.selector.Canonical(tt.scope); !errors.Is(err, ErrInvalid) {
			t.Fatalf("accepted unsupported selector %s %+v: %v", tt.scope, tt.selector, err)
		}
	}
}

func TestJavaSamplingPolicyCanonicalBody(t *testing.T) {
	body, hash, err := JavaSamplingBody(0.1)
	if err != nil || body == "" || len(hash) != 64 {
		t.Fatalf("compile Java policy: %q %s %v", body, hash, err)
	}
	if b, h, err := JavaSamplingBody(0.1); err != nil || b != body || h != hash {
		t.Fatalf("non-deterministic policy: %s %s %v", b, h, err)
	}
	for _, rate := range []float64{-1, 1.01} {
		if _, _, err := JavaSamplingBody(rate); !errors.Is(err, ErrInvalid) {
			t.Fatalf("accepted rate %v: %v", rate, err)
		}
	}
}
