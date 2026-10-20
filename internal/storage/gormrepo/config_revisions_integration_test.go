//go:build integration

package gormrepo

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/XiaoWeiKIN/apm-control-plane/internal/appconfig"
	"github.com/XiaoWeiKIN/apm-control-plane/internal/config"
	"github.com/open-telemetry/opamp-go/protobufs"
	"google.golang.org/protobuf/proto"
)

func TestConfigRevisionPersistencePostgresAndMySQL(t *testing.T) {
	for _, tt := range []struct{ driver, env string }{
		{"postgres", "APM_TEST_POSTGRES_DSN"},
		{"mysql", "APM_TEST_MYSQL_DSN"},
	} {
		t.Run(tt.driver, func(t *testing.T) {
			dsn := os.Getenv(tt.env)
			if dsn == "" {
				t.Skipf("%s not configured", tt.env)
			}
			ctx := context.Background()
			database := appconfig.Database{Driver: tt.driver, DSN: dsn, MaxOpen: 5, MaxIdle: 2, MaxLifetime: time.Minute}
			store, err := Open(ctx, database)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if store != nil {
					_ = store.Close()
				}
			}()

			snapshot, err := config.CompileJavaSampling(0.1)
			if err != nil {
				t.Fatal(err)
			}
			first, err := store.CreateConfigRevision(ctx, "workspace-one", "admin-one", snapshot)
			if err != nil {
				t.Fatal(err)
			}
			if len(first.ConfigRevisionID) != 36 ||
				first.AgentKind != config.JavaAgentKind ||
				first.ConfigDialect != config.JavaSamplingDialect ||
				first.PayloadFormat != config.ConfigMapFormat ||
				first.HashAlgorithm != config.HashAlgorithm ||
				first.SourcePolicyRevisionID != nil ||
				first.PayloadSizeBytes != int64(len(snapshot.ConfigMapBytes())) ||
				first.CreatedAt.IsZero() ||
				!bytes.Equal(first.ConfigMapBytes, snapshot.ConfigMapBytes()) {
				t.Fatalf("invalid persisted ConfigRevision: %+v", first)
			}
			hash := snapshot.Hash()
			if first.ConfigHash != hex.EncodeToString(hash[:]) ||
				sha256.Sum256(first.ConfigMapBytes) != hash {
				t.Fatalf("persisted ConfigMap hash mismatch: %+v", first)
			}
			var decoded protobufs.AgentConfigMap
			if err := proto.Unmarshal(first.ConfigMapBytes, &decoded); err != nil {
				t.Fatalf("persisted ConfigMap cannot be decoded: %v", err)
			}
			if decoded.ConfigMap["apm/sampling.json"] == nil {
				t.Fatal("persisted ConfigRevision missing Java sampling object")
			}
			if _, err := store.GetConfigRevision(ctx, "workspace-two", first.ConfigRevisionID); !errors.Is(err, ErrConfigRevisionNotFound) {
				t.Fatalf("cross-workspace ConfigRevision leaked: %v", err)
			}

			// Identical bytes may be published by separate immutable business revisions.
			second, err := store.CreateConfigRevision(ctx, "workspace-one", "admin-one", snapshot)
			if err != nil {
				t.Fatal(err)
			}
			if first.ConfigRevisionID == second.ConfigRevisionID || first.ConfigHash != second.ConfigHash {
				t.Fatalf("ConfigRevision identity was deduplicated by hash: %s %s", first.ConfigRevisionID, second.ConfigRevisionID)
			}
			if _, err := store.CreateConfigRevision(ctx, "", "admin-one", snapshot); err == nil {
				t.Fatal("empty Workspace accepted")
			}
			if _, err := store.CreateConfigRevision(ctx, "workspace-one", "", snapshot); err == nil {
				t.Fatal("empty actor accepted")
			}
			if _, err := store.CreateConfigRevision(ctx, "workspace-one", "admin-one", config.Snapshot{}); err == nil {
				t.Fatal("empty ConfigMap accepted")
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			store = nil
			store, err = Open(ctx, database)
			if err != nil {
				t.Fatal(err)
			}
			restored, err := store.GetConfigRevision(ctx, "workspace-one", first.ConfigRevisionID)
			if err != nil {
				t.Fatal(err)
			}
			if restored.ConfigHash != first.ConfigHash ||
				restored.ConfigRevisionID != first.ConfigRevisionID ||
				!bytes.Equal(restored.ConfigMapBytes, first.ConfigMapBytes) ||
				restored.PayloadSizeBytes != first.PayloadSizeBytes {
				t.Fatalf("recovered revision differs from original: %+v", restored)
			}
		})
	}
}
