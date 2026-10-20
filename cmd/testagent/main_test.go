package main

import (
	"testing"

	"github.com/open-telemetry/opamp-go/protobufs"
)

func TestTestAgentAdvertisesRequiredStatusCapability(t *testing.T) {
	if testAgentCapabilities&uint64(protobufs.AgentCapabilities_AgentCapabilities_ReportsStatus) == 0 {
		t.Fatal("OpAMP test Agent must advertise ReportsStatus")
	}
}
