package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math"

	"github.com/open-telemetry/opamp-go/protobufs"
	"google.golang.org/protobuf/proto"
)

const (
	JavaAgentKind       = "java-apm"
	JavaSamplingDialect = "java-apm-sampling/v1"
	ConfigMapFormat     = "opamp-config-map-proto"
	HashAlgorithm       = "sha256-proto-deterministic-v1"
)

// Snapshot is an immutable compiled OpAMP ConfigMap.
// The Hash is computed over the deterministic protobuf encoding of the full map.
type Snapshot struct {
	agentKind string
	dialect   string
	data      []byte
	hash      [sha256.Size]byte
}

func (s Snapshot) AgentKind() string       { return s.agentKind }
func (s Snapshot) ConfigDialect() string   { return s.dialect }
func (s Snapshot) ConfigMapBytes() []byte  { return bytes.Clone(s.data) }
func (s Snapshot) Hash() [sha256.Size]byte { return s.hash }

type javaSamplingPolicy struct {
	SchemaVersion int `json:"schema_version"`
	Sampling      struct {
		Type string  `json:"type"`
		Rate float64 `json:"rate"`
	} `json:"sampling"`
}

// CompileJavaSampling builds the P2.1 Java Head Sampling ConfigMap.
// This only compiles a candidate config; it does not publish or apply it.
func CompileJavaSampling(rate float64) (Snapshot, error) {
	if math.IsNaN(rate) || math.IsInf(rate, 0) || rate < 0 || rate > 1 {
		return Snapshot{}, fmt.Errorf("sampling_rate must be in [0, 1]")
	}
	if rate == 0 {
		rate = 0 // Canonicalize negative zero to the same configuration.
	}
	policy := javaSamplingPolicy{SchemaVersion: 1}
	policy.Sampling.Type = "parent_based_trace_id_ratio"
	policy.Sampling.Rate = rate
	body, err := json.Marshal(policy)
	if err != nil {
		return Snapshot{}, fmt.Errorf("marshal Java sampling policy: %w", err)
	}
	configMap := &protobufs.AgentConfigMap{ConfigMap: map[string]*protobufs.AgentConfigObject{
		"apm/sampling.json": {Body: body, ContentType: "application/json"},
	}}
	data, err := (proto.MarshalOptions{Deterministic: true}).Marshal(configMap)
	if err != nil {
		return Snapshot{}, fmt.Errorf("marshal OpAMP ConfigMap: %w", err)
	}
	return Snapshot{
		agentKind: JavaAgentKind,
		dialect:   JavaSamplingDialect,
		data:      data,
		hash:      sha256.Sum256(data),
	}, nil
}
