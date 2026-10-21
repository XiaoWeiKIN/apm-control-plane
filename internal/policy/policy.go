package policy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/XiaoWeiKIN/apm-control-plane/internal/config"
)

var (
	ErrInvalid  = errors.New("invalid policy or selector")
	ErrConflict = errors.New("policy selector conflict")
	ErrNotFound = errors.New("policy not found")
)

const (
	ScopeWorkspace   = "WORKSPACE"
	ScopeEnvironment = "ENVIRONMENT"
	ScopeService     = "SERVICE"
	ScopeInstance    = "INSTANCE"
)

// Selector uses only Agent inventory attributes; it never carries an authorization scope.
type Selector struct {
	Environment string `json:"environment,omitempty"`
	ServiceName string `json:"service_name,omitempty"`
	InstanceUID string `json:"instance_uid,omitempty"`
}

func (s Selector) Canonical(scope string) (string, string, error) {
	switch scope {
	case ScopeWorkspace:
		if s.Environment != "" || s.ServiceName != "" || s.InstanceUID != "" {
			return "", "", ErrInvalid
		}
	case ScopeEnvironment:
		if s.Environment == "" || s.ServiceName != "" || s.InstanceUID != "" {
			return "", "", ErrInvalid
		}
	case ScopeService:
		if s.ServiceName == "" || s.InstanceUID != "" {
			return "", "", ErrInvalid
		}
	case ScopeInstance:
		if len(s.InstanceUID) != 32 || s.Environment != "" || s.ServiceName != "" {
			return "", "", ErrInvalid
		}
		raw, err := hex.DecodeString(s.InstanceUID)
		if err != nil || hex.EncodeToString(raw) != s.InstanceUID {
			return "", "", ErrInvalid
		}
	default:
		return "", "", ErrInvalid
	}
	body, err := json.Marshal(s)
	if err != nil {
		return "", "", fmt.Errorf("marshal selector: %w", err)
	}
	sum := sha256.Sum256(body)
	return string(body), hex.EncodeToString(sum[:]), nil
}

type Agent struct {
	InstanceUID string
	Environment string
	ServiceName string
}

func (s Selector) Matches(scope string, a Agent) bool {
	switch scope {
	case ScopeWorkspace:
		return true
	case ScopeEnvironment:
		return s.Environment == a.Environment
	case ScopeService:
		return s.ServiceName == a.ServiceName && (s.Environment == "" || s.Environment == a.Environment)
	case ScopeInstance:
		return s.InstanceUID == a.InstanceUID
	default:
		return false
	}
}

func ScopeRank(scope string) int {
	switch scope {
	case ScopeInstance:
		return 4
	case ScopeService:
		return 3
	case ScopeEnvironment:
		return 2
	case ScopeWorkspace:
		return 1
	}
	return 0
}

// JavaSamplingBody returns the canonical immutable policy JSON and content hash.
func JavaSamplingBody(rate float64) (string, string, error) {
	if _, err := config.CompileJavaSampling(rate); err != nil {
		return "", "", fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if rate == 0 {
		rate = 0
	}
	body := struct {
		SchemaVersion int `json:"schema_version"`
		Sampling      struct {
			Type string  `json:"type"`
			Rate float64 `json:"rate"`
		} `json:"sampling"`
	}{SchemaVersion: 1}
	body.Sampling.Type = "parent_based_trace_id_ratio"
	body.Sampling.Rate = rate
	data, err := json.Marshal(body)
	if err != nil {
		return "", "", err
	}
	sum := sha256.Sum256(data)
	return string(data), hex.EncodeToString(sum[:]), nil
}
