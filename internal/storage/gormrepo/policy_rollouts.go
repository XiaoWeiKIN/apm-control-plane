package gormrepo

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"
	"unicode/utf8"

	"github.com/XiaoWeiKIN/apm-control-plane/internal/config"
	"github.com/XiaoWeiKIN/apm-control-plane/internal/policy"
	"github.com/open-telemetry/opamp-go/protobufs"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// PolicyRolloutFilter limits the inventory snapshot; omitted fields match the
// whole Workspace. Inventory attributes select candidates, never authorize them.
type PolicyRolloutFilter struct {
	Environment string `json:"environment,omitempty"`
	ServiceName string `json:"service_name,omitempty"`
}

// PublishPolicyRollout selects a bounded inventory snapshot and freezes matching
// Java Agents in the existing P3.1 atomic rollout transaction.
func (s *Store) PublishPolicyRollout(ctx context.Context, workspace, actor, policyRevisionID string,
	filter PolicyRolloutFilter, batchSize int, key string) (RolloutView, error) {
	// 1. 拒绝非法 Workspace、Revision 和候选筛选范围，不接受隐式截断。
	if !policyIdentityOK(workspace, actor) || len(policyRevisionID) != 36 ||
		utf8.RuneCountInString(key) > 128 ||
		utf8.RuneCountInString(filter.Environment) > 128 ||
		utf8.RuneCountInString(filter.ServiceName) > 128 ||
		batchSize < 1 || batchSize > maxRolloutTargets {
		return RolloutView{}, ErrInvalidRollout
	}
	request, err := json.Marshal(struct {
		Kind             string              `json:"kind"`
		PolicyRevisionID string              `json:"policy_revision_id"`
		Filter           PolicyRolloutFilter `json:"filter"`
		BatchSize        int                 `json:"batch_size"`
		Actor            string              `json:"actor"`
	}{"policy-rollout", policyRevisionID, filter, batchSize, actor})
	if err != nil {
		return RolloutView{}, err
	}
	sum := sha256.Sum256(request)
	requestHash := hex.EncodeToString(sum[:])

	var result RolloutView
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 2. 幂等回放优先读取原发布，不受后续 Agent 上下线或 Binding 变更影响。
		if key != "" {
			var previous ConfigReleaseRecord
			err := tx.Clauses(clause.Locking{Strength: "SHARE"}).
				Where("workspace_id = ? AND idempotency_key = ?", workspace, key).Take(&previous).Error
			if err == nil {
				if previous.RequestSHA256 != requestHash {
					return ErrReleaseConflict
				}
				var plan rolloutPlan
				if err := json.Unmarshal([]byte(previous.RolloutPlanJSON), &plan); err != nil || plan.BatchSize < 1 {
					return ErrReleaseConflict
				}
				result, err = rolloutViewTx(tx, previous)
				return err
			}
			if !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}
		}

		var revision PolicyRevisionRecord
		if err := tx.Clauses(clause.Locking{Strength: "SHARE"}).
			Where("workspace_id = ? AND policy_revision_id = ?", workspace, policyRevisionID).
			Take(&revision).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return policy.ErrNotFound
			}
			return err
		}
		var parent PolicyRecord
		if err := tx.Clauses(clause.Locking{Strength: "SHARE"}).
			Where("workspace_id = ? AND policy_id = ?", workspace, revision.PolicyID).
			Take(&parent).Error; err != nil {
			return err
		}
		if parent.Status != "ACTIVE" || parent.PolicyType != "head_sampling" ||
			parent.AgentKind != config.JavaAgentKind {
			return policy.ErrInvalid
		}

		// 3. 在事务内锁住同 Workspace 有界候选 Agent，按当前 Binding 解析实际选中 Revision。
		query := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("workspace_id = ? AND agent_kind = ?", workspace, config.JavaAgentKind)
		if filter.Environment != "" {
			query = query.Where("environment = ?", filter.Environment)
		}
		if filter.ServiceName != "" {
			query = query.Where("service_name = ?", filter.ServiceName)
		}
		var candidates []AgentRecord
		if err := query.Order("instance_uid").Limit(maxRolloutTargets + 1).
			Find(&candidates).Error; err != nil {
			return err
		}
		if len(candidates) > maxRolloutTargets || len(candidates) < 2 {
			return ErrInvalidRollout
		}
		uids := make([]string, 0, len(candidates))
		for _, agent := range candidates {
			resolved, err := (&Store{db: tx}).resolveAgentPolicy(ctx, workspace, agent, true)
			if errors.Is(err, policy.ErrNotFound) {
				continue
			}
			if err != nil {
				return err
			}
			if resolved.PolicyRevisionID != policyRevisionID {
				continue
			}
			if uint64(agent.Capabilities)&uint64(protobufs.AgentCapabilities_AgentCapabilities_AcceptsRemoteConfig) == 0 {
				return ErrUnsupportedReleaseTarget
			}
			uids = append(uids, agent.InstanceUID)
		}
		if len(uids) < 2 || batchSize > len(uids) {
			return ErrInvalidRollout
		}

		// 4. 校验策略正文与 SHA-256 后，确定性编译一份完整 Java ConfigMap。
		var document struct {
			SchemaVersion int `json:"schema_version"`
			Sampling      struct {
				Type string  `json:"type"`
				Rate float64 `json:"rate"`
			} `json:"sampling"`
		}
		if err := json.Unmarshal([]byte(revision.BodyJSON), &document); err != nil {
			return policy.ErrInvalid
		}
		canonical, hash, err := policy.JavaSamplingBody(document.Sampling.Rate)
		if err != nil || revision.SchemaVersion != 1 || document.SchemaVersion != 1 ||
			document.Sampling.Type != "parent_based_trace_id_ratio" ||
			canonical != revision.BodyJSON || hash != revision.BodySHA256 {
			return policy.ErrInvalid
		}
		snapshot, err := config.CompileJavaSampling(document.Sampling.Rate)
		if err != nil {
			return policy.ErrInvalid
		}
		configID, err := newReleaseUUID()
		if err != nil {
			return err
		}
		data := snapshot.ConfigMapBytes()
		sum := snapshot.Hash()
		record := ConfigRevisionRecord{
			WorkspaceID: workspace, ConfigRevisionID: configID,
			SourcePolicyRevisionID: &policyRevisionID,
			AgentKind:              snapshot.AgentKind(), ConfigDialect: snapshot.ConfigDialect(),
			PayloadFormat: config.ConfigMapFormat, ConfigMapBytes: data,
			ConfigHash: hex.EncodeToString(sum[:]), HashAlgorithm: config.HashAlgorithm,
			PayloadSizeBytes: int64(len(data)), CreatedAt: time.Now().UTC(), CreatedBy: actor,
		}
		if err := tx.Create(&record).Error; err != nil {
			return err
		}

		// 5. 复用 P3.1 的完整冻结事务，外层事务保证新 ConfigRevision 同步回滚。
		result, err = (&Store{db: tx}).PublishRollout(ctx, workspace, actor, configID, uids, batchSize, "")
		if err != nil {
			return err
		}
		selector, err := json.Marshal(struct {
			PolicyRevisionID string   `json:"policy_revision_id"`
			Environment      string   `json:"environment,omitempty"`
			ServiceName      string   `json:"service_name,omitempty"`
			InstanceUIDs     []string `json:"instance_uids"`
		}{policyRevisionID, filter.Environment, filter.ServiceName, uids})
		if err != nil {
			return err
		}
		var idem *string
		if key != "" {
			idem = &key
		}
		r := tx.Model(&ConfigReleaseRecord{}).
			Where("workspace_id = ? AND release_id = ?", workspace, result.ReleaseID).
			Updates(map[string]any{
				"policy_revision_id":   policyRevisionID,
				"target_selector_json": string(selector),
				"request_sha256":       requestHash,
				"idempotency_key":      idem,
			})
		if r.Error != nil {
			if key != "" && errors.Is(r.Error, gorm.ErrDuplicatedKey) {
				return ErrReleaseConflict
			}
			return r.Error
		}
		if r.RowsAffected != 1 {
			return ErrAssignmentConflict
		}
		return nil
	})
	return result, err
}
