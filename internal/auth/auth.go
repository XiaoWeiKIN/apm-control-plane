package auth

import (
	"crypto/sha256"
	"crypto/subtle"
	"strings"

	"github.com/XiaoWeiKIN/apm-control-plane/internal/appconfig"
)

type Principal struct {
	WorkspaceID  string
	CredentialID string
}

type Authenticator struct {
	creds []appconfig.Credential
}

func New(credentials []appconfig.Credential) *Authenticator {
	return &Authenticator{creds: append([]appconfig.Credential(nil), credentials...)}
}

// Authenticate never trusts a workspace or identity provided in AgentDescription.
func (a *Authenticator) Authenticate(header string) (Principal, bool) {
	if !strings.HasPrefix(header, "Bearer ") {
		return Principal{}, false
	}
	token := strings.TrimPrefix(header, "Bearer ")
	if token == "" || strings.ContainsAny(token, " \t\r\n") {
		return Principal{}, false
	}
	actual := sha256.Sum256([]byte(token))
	for _, cred := range a.creds {
		expected := sha256.Sum256([]byte(cred.Token))
		if subtle.ConstantTimeCompare(actual[:], expected[:]) == 1 {
			return Principal{WorkspaceID: cred.WorkspaceID, CredentialID: cred.ID}, true
		}
	}
	return Principal{}, false
}
