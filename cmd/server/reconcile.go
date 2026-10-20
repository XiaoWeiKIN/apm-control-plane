package main

import (
	"context"
	"encoding/hex"
	"fmt"

	"github.com/XiaoWeiKIN/apm-control-plane/internal/storage/gormrepo"
	"github.com/open-telemetry/opamp-go/protobufs"
	"google.golang.org/protobuf/proto"
)

// deliveryStore is the narrow durable Desired/Observed boundary used by OpAMP.
type deliveryStore interface {
	ReconcileAgentConfig(context.Context, string, string, *gormrepo.ConfigStatus) (*gormrepo.DesiredConfig, bool, error)
}

func remoteConfigFromDesired(desired *gormrepo.DesiredConfig) (*protobufs.AgentRemoteConfig, error) {
	if desired == nil {
		return nil, nil
	}
	hash, err := hex.DecodeString(desired.ConfigHash)
	if err != nil || len(hash) != 32 {
		return nil, fmt.Errorf("invalid stored Desired ConfigHash")
	}
	var configMap protobufs.AgentConfigMap
	if err := proto.Unmarshal(desired.ConfigMapBytes, &configMap); err != nil {
		return nil, fmt.Errorf("decode stored OpAMP ConfigMap: %w", err)
	}
	if len(configMap.ConfigMap) == 0 {
		return nil, fmt.Errorf("stored OpAMP ConfigMap is empty")
	}
	return &protobufs.AgentRemoteConfig{Config: &configMap, ConfigHash: hash}, nil
}
