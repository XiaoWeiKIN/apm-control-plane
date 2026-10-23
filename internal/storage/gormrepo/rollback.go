package gormrepo

import (
	"context"
	"encoding/json"
	"errors"

	"gorm.io/gorm"
)

// PublishRollback creates a new, audited compensating rollout. The Admin
// supplies the desired historical ConfigRevision explicitly; no inference from
// an ACK or mutable current Assignment is made.
func (s *Store) PublishRollback(ctx context.Context, workspace, actor, sourceReleaseID, configRevisionID string,
	expectedRowVersion int64, batchSize int, key string) (RolloutView, error) {
	// 1. 拒绝无效来源及 CAS 版本，避免对任意单 Agent 发布冒充批量回滚。
	if !policyIdentityOK(workspace, actor) || len(sourceReleaseID) != 36 ||
		len(configRevisionID) != 36 || expectedRowVersion < 1 {
		return RolloutView{}, ErrInvalidRollout
	}
	var source ConfigReleaseRecord
	if err := s.db.WithContext(ctx).
		Where("workspace_id = ? AND release_id = ?", workspace, sourceReleaseID).
		Take(&source).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return RolloutView{}, ErrRolloutNotFound
		}
		return RolloutView{}, err
	}
	var plan rolloutPlan
	if err := json.Unmarshal([]byte(source.RolloutPlanJSON), &plan); err != nil || plan.BatchSize < 1 {
		return RolloutView{}, ErrRolloutNotFound
	}
	var frozen []ReleaseTargetRecord
	if err := s.db.WithContext(ctx).Where("workspace_id = ? AND release_id = ?", workspace, sourceReleaseID).
		Order("instance_uid").Find(&frozen).Error; err != nil {
		return RolloutView{}, err
	}
	if len(frozen) < 2 || len(frozen) > maxRolloutTargets {
		return RolloutView{}, ErrInvalidRollout
	}
	uids := make([]string, 0, len(frozen))
	for _, target := range frozen {
		uids = append(uids, target.InstanceUID)
	}

	// 2. 复用同一事务冻结新 Release；来源 CAS、目标一致性在锁内重检。
	return s.publishRollout(ctx, workspace, actor, configRevisionID, uids, batchSize, key,
		&rollbackIntent{SourceReleaseID: sourceReleaseID, ExpectedRowVersion: expectedRowVersion})
}
