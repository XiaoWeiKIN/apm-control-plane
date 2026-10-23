//go:build integration

package gormrepo

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/XiaoWeiKIN/apm-control-plane/internal/appconfig"
	"github.com/XiaoWeiKIN/apm-control-plane/internal/auth"
	"github.com/XiaoWeiKIN/apm-control-plane/internal/config"
	"github.com/XiaoWeiKIN/apm-control-plane/internal/policy"
	"github.com/open-telemetry/opamp-go/protobufs"
)

func TestPolicySelectedRolloutAndResumePostgresAndMySQL(t *testing.T) {
	for _, tt := range []struct{ driver, env string }{
		{"postgres", "APM_TEST_POSTGRES_DSN"},
		{"mysql", "APM_TEST_MYSQL_DSN"},
	} {
		t.Run(tt.driver, func(t *testing.T) {
			dsn := os.Getenv(tt.env)
			if dsn == "" { t.Skip(tt.env + " not configured") }
			ctx := context.Background()
			store, err := Open(ctx, appconfig.Database{
				Driver: tt.driver, DSN: dsn, MaxOpen: 10, MaxIdle: 2, MaxLifetime: time.Minute,
			})
			if err != nil { t.Fatal(err) }
			defer store.Close()

			const workspace = "p32-policy-rollout-contract"
			principal := auth.Principal{WorkspaceID: workspace, CredentialID: "java-owner"}
			uids := []string{newUID(t), newUID(t), newUID(t), newUID(t), newUID(t)}
			for i, uid := range uids {
				service, environment := "checkout", "prod"
				if i == 3 { environment = "dev" }
				if i == 4 { service = "billing" }
				if _, err := store.Observe(ctx, principal, uid, Observation{
					SeenAt: time.Now().UTC(), Transport: "http",
					Capabilities: uint64(protobufs.AgentCapabilities_AgentCapabilities_AcceptsRemoteConfig),
					Description: &Description{Kind: config.JavaAgentKind, Service: service, Environment: environment},
				}); err != nil { t.Fatal(err) }
			}
			_, selected, err := store.CreatePolicy(ctx, workspace, "admin", "checkout-head", 0.2)
			if err != nil { t.Fatal(err) }
			if _, err := store.CreatePolicyBinding(ctx, workspace, "admin", BindingInput{
				PolicyRevisionID: selected.PolicyRevisionID, ScopeType: policy.ScopeService,
				Selector: policy.Selector{ServiceName: "checkout"}, Priority: 1,
			}); err != nil { t.Fatal(err) }
			_, override, err := store.CreatePolicy(ctx, workspace, "admin", "instance-override", 0.8)
			if err != nil { t.Fatal(err) }
			if _, err := store.CreatePolicyBinding(ctx, workspace, "admin", BindingInput{
				PolicyRevisionID: override.PolicyRevisionID, ScopeType: policy.ScopeInstance,
				Selector: policy.Selector{InstanceUID: uids[2]}, Priority: 1,
			}); err != nil { t.Fatal(err) }

			filter := PolicyRolloutFilter{Environment: "prod", ServiceName: "checkout"}
			if _, err := store.PublishPolicyRollout(ctx, "other-workspace", "admin",
				selected.PolicyRevisionID, filter, 1, ""); !errors.Is(err, policy.ErrNotFound) {
				t.Fatalf("cross-workspace policy release accepted: %v", err)
			}
			if _, err := store.PublishPolicyRollout(ctx, workspace, "admin",
				"00000000-0000-4000-8000-000000000000", filter, 1, ""); !errors.Is(err, policy.ErrNotFound) {
				t.Fatalf("unknown PolicyRevision accepted: %v", err)
			}
			if _, err := store.PublishPolicyRollout(ctx, workspace, "admin",
				selected.PolicyRevisionID, filter, 3, ""); !errors.Is(err, ErrInvalidRollout) {
				t.Fatalf("batch_size exceeding matched agents accepted: %v", err)
			}
			key := "p32-"+uids[0][:16]
			created, err := store.PublishPolicyRollout(ctx, workspace, "admin",
				selected.PolicyRevisionID, filter, 1, key)
			if err != nil { t.Fatal(err) }
			if len(created.Targets) != 2 || len(created.Activated) != 1 || created.Status != "RUNNING" {
				t.Fatalf("wrong frozen target set: %+v", created)
			}
			for _, target := range created.Targets {
				if target.InstanceUID != uids[0] && target.InstanceUID != uids[1] {
					t.Fatalf("selector or higher-priority override ignored: %+v", target)
				}
			}
			var release ConfigReleaseRecord
			if err := store.db.Where("workspace_id = ? AND release_id = ?", workspace, created.ReleaseID).
				Take(&release).Error; err != nil { t.Fatal(err) }
			if release.PolicyRevisionID == nil || *release.PolicyRevisionID != selected.PolicyRevisionID {
				t.Fatalf("release missing Policy provenance: %+v", release)
			}
			var selector struct {
				PolicyRevisionID string `json:"policy_revision_id"`
				InstanceUIDs []string `json:"instance_uids"`
			}
			if err := json.Unmarshal([]byte(release.TargetSelectorJSON), &selector); err != nil ||
				selector.PolicyRevisionID != selected.PolicyRevisionID || len(selector.InstanceUIDs) != 2 {
				t.Fatalf("unfrozen selection evidence: %+v %v", selector, err)
			}
			cfg, err := store.GetConfigRevision(ctx, workspace, created.Targets[0].ConfigRevisionID)
			if err != nil || cfg.SourcePolicyRevisionID == nil ||
				*cfg.SourcePolicyRevisionID != selected.PolicyRevisionID {
				t.Fatalf("missing immutable ConfigRevision provenance: %+v %v", cfg, err)
			}
			if _, err := store.GetAgentAssignment(ctx, workspace, created.Targets[1].InstanceUID); !errors.Is(err, ErrNotFound) {
				t.Fatalf("queued target received Desired: %v", err)
			}
			again, err := store.PublishPolicyRollout(ctx, workspace, "admin",
				selected.PolicyRevisionID, filter, 1, key)
			if err != nil || again.ReleaseID != created.ReleaseID || len(again.Activated) != 0 {
				t.Fatalf("idempotent policy rollout changed release: %+v %v", again, err)
			}
			if _, err := store.PublishPolicyRollout(ctx, workspace, "admin",
				selected.PolicyRevisionID, filter, 2, key); !errors.Is(err, ErrReleaseConflict) {
				t.Fatalf("same key with different batch size accepted: %v", err)
			}

			// 1. 目标失败暂停；在匹配的新 APPLIED 回执前显式恢复必须失败。
			firstUID := created.Activated[0]
			offered, _, err := store.ReconcileAgentConfig(ctx, workspace, firstUID, nil, true)
			if err != nil || offered == nil { t.Fatalf("first target offer: %+v %v", offered, err) }
			if _, _, err := store.ReconcileAgentConfig(ctx, workspace, firstUID,
				&ConfigStatus{Hash: offered.ConfigHash, Status: "FAILED"}, false); err != nil { t.Fatal(err) }
			paused, err := store.GetRollout(ctx, workspace, created.ReleaseID)
			if err != nil || paused.Status != "PAUSED" { t.Fatalf("failed target did not pause: %+v %v", paused, err) }
			if _, err := store.ResumeRollout(ctx, workspace, created.ReleaseID, "admin", paused.RowVersion);
				!errors.Is(err, ErrRolloutBlocked) {
				t.Fatalf("resumed failed target before ACK recovery: %v", err)
			}
			if _, err := store.ResumeRollout(ctx, workspace, created.ReleaseID, "admin", paused.RowVersion-1);
				!errors.Is(err, ErrAssignmentConflict) {
				t.Fatalf("stale resume version accepted: %v", err)
			}

			// 2. 晚到 APPLIED 只能更新 Target，不会自动解除 PAUSED。
			if _, _, err := store.ReconcileAgentConfig(ctx, workspace, firstUID,
				&ConfigStatus{Hash: offered.ConfigHash, Status: "APPLIED"}, false); err != nil { t.Fatal(err) }
			still, err := store.GetRollout(ctx, workspace, created.ReleaseID)
			if err != nil || still.Status != "PAUSED" || still.Targets[0].Status != "APPLIED" {
				t.Fatalf("sticky pause bypassed: %+v %v", still, err)
			}
			resumed, err := store.ResumeRollout(ctx, workspace, created.ReleaseID, "admin", still.RowVersion)
			if err != nil || resumed.Status != "RUNNING" ||
				resumed.RowVersion != still.RowVersion+1 || len(resumed.Activated) != 0 {
				t.Fatalf("failed guarded resume: %+v %v", resumed, err)
			}
			if _, err := store.ResumeRollout(ctx, workspace, created.ReleaseID, "admin", still.RowVersion);
				!errors.Is(err, ErrAssignmentConflict) {
				t.Fatalf("duplicate resume version accepted: %v", err)
			}
			var after ConfigReleaseRecord
			if err := store.db.Where("workspace_id = ? AND release_id = ?", workspace, created.ReleaseID).
				Take(&after).Error; err != nil || after.FinishedAt != nil {
				t.Fatalf("resumed release retains finished_at: %+v %v", after, err)
			}

			// 3. Resume 无网络副作用；显式 Advance 激活第二批并以 ACK 完成。
			next, err := store.AdvanceRollout(ctx, workspace, created.ReleaseID, "admin")
			if err != nil || len(next.Activated) != 1 { t.Fatalf("advance after resume: %+v %v", next, err) }
			secondUID := next.Activated[0]
			offered, _, err = store.ReconcileAgentConfig(ctx, workspace, secondUID, nil, true)
			if err != nil || offered == nil || offered.ConfigHash != cfg.ConfigHash { t.Fatalf("second offer: %+v %v", offered, err) }
			if _, _, err := store.ReconcileAgentConfig(ctx, workspace, secondUID,
				&ConfigStatus{Hash: offered.ConfigHash, Status: "APPLIED"}, false); err != nil { t.Fatal(err) }
			done, err := store.GetRollout(ctx, workspace, created.ReleaseID)
			if err != nil || done.Status != "COMPLETED" { t.Fatalf("rollout not completed: %+v %v", done, err) }
			if _, err := store.ResumeRollout(ctx, "other-workspace", created.ReleaseID, "admin", done.RowVersion);
				!errors.Is(err, ErrRolloutNotFound) { t.Fatalf("cross-workspace resume leaked: %v", err) }
			if _, err := store.ResumeRollout(ctx, workspace, created.ReleaseID, "admin", done.RowVersion);
				!errors.Is(err, ErrRolloutBlocked) { t.Fatalf("completed release resumed: %v", err) }
		})
	}
}
