package auth

import (
	"testing"

	"github.com/XiaoWeiKIN/apm-control-plane/internal/appconfig"
)

func TestAuthenticationAndOrgBinding(t *testing.T) {
	a := New([]appconfig.Credential{{ID: "agent-1", OrgID: 1, Token: "random-example-token-0001"}})
	p, ok := a.Authenticate("Bearer random-example-token-0001")
	if !ok || p.OrgID != 1 || p.CredentialID != "agent-1" {
		t.Fatalf("incorrect scoped principal: %+v %v", p, ok)
	}
	for _, h := range []string{"random-example-token-0001", "Basic random-example-token-0001", "Bearer wrong-example-token-0001", "Bearer random-example-token-0001 extra"} {
		if _, ok := a.Authenticate(h); ok {
			t.Errorf("accepted %q", h)
		}
	}
}
