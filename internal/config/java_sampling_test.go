package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"math"
	"testing"

	"github.com/open-telemetry/opamp-go/protobufs"
	"google.golang.org/protobuf/proto"
)

func TestCompileJavaSamplingDeterministicAndDecodable(t *testing.T) {
	first, err := CompileJavaSampling(0.1)
	if err != nil {
		t.Fatal(err)
	}
	second, err := CompileJavaSampling(0.1)
	if err != nil {
		t.Fatal(err)
	}
	if first.AgentKind() != JavaAgentKind || first.ConfigDialect() != JavaSamplingDialect {
		t.Fatalf("incorrect Java configuration target: %+v", first)
	}
	raw := first.ConfigMapBytes()
	if !bytes.Equal(raw, second.ConfigMapBytes()) || first.Hash() != second.Hash() {
		t.Fatal("same policy produced different OpAMP ConfigMap bytes or hash")
	}
	if first.Hash() != sha256.Sum256(raw) {
		t.Fatal("config hash does not match persisted complete ConfigMap")
	}
	var configMap protobufs.AgentConfigMap
	if err := proto.Unmarshal(raw, &configMap); err != nil {
		t.Fatal(err)
	}
	if len(configMap.ConfigMap) != 1 {
		t.Fatalf("expected one config object: %+v", configMap.ConfigMap)
	}
	object := configMap.ConfigMap["apm/sampling.json"]
	if object == nil || object.ContentType != "application/json" {
		t.Fatalf("wrong Java sampling content: %+v", object)
	}
	var policy javaSamplingPolicy
	if err := json.Unmarshal(object.Body, &policy); err != nil {
		t.Fatal(err)
	}
	if policy.SchemaVersion != 1 || policy.Sampling.Type != "parent_based_trace_id_ratio" || policy.Sampling.Rate != 0.1 {
		t.Fatalf("unexpected Java config: %+v", policy)
	}

	// Exposed bytes must not allow callers to mutate the compiled snapshot.
	raw[0] ^= 0xff
	if !bytes.Equal(first.ConfigMapBytes(), second.ConfigMapBytes()) {
		t.Fatal("caller mutation changed the immutable compiled bytes")
	}
	other, err := CompileJavaSampling(0.2)
	if err != nil || other.Hash() == first.Hash() {
		t.Fatalf("different sampling policies produced same hash: %v", err)
	}
}

func TestCompileJavaSamplingBoundaries(t *testing.T) {
	for _, rate := range []float64{0, 0.1, 1} {
		if _, err := CompileJavaSampling(rate); err != nil {
			t.Fatalf("valid sampling rate %v rejected: %v", rate, err)
		}
	}
	a, _ := CompileJavaSampling(0)
	b, _ := CompileJavaSampling(math.Copysign(0, -1))
	if a.Hash() != b.Hash() {
		t.Fatal("negative zero must use the same config revision content")
	}
	for _, rate := range []float64{-0.001, 1.001, math.NaN(), math.Inf(1), math.Inf(-1)} {
		if _, err := CompileJavaSampling(rate); err == nil {
			t.Fatalf("invalid sampling rate %v accepted", rate)
		}
	}
}
