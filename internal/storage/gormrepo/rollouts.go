package gormrepo

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"slices"
	"time"
	"unicode/utf8"

	"github.com/XiaoWeiKIN/apm-control-plane/internal/config"
	"github.com/open-telemetry/opamp-go/protobufs"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	ErrInvalidRollout  = errors.New("invalid rollout request")
	ErrRolloutNotFound = errors.New("release not found")
	ErrRolloutNotReady = errors.New("current rollout batch is not applied")
	ErrRolloutBlocked  = errors.New("rollout is paused or blocked")
)

const maxRolloutTargets = 16

type RolloutTarget struct {
	InstanceUID       string `json:"instance_uid"`
	ConfigRevisionID  string `json:"config_revision_id"`
	BatchNo           int    `json:"batch_no"`
	Status            string `json:"status"`
	DesiredGeneration int64  `json:"desired_generation"`
}

type RolloutView struct {
	WorkspaceID         string          `json:"workspace_id"`
	ReleaseID           string          `json:"release_id"`
	Status              string          `json:"status"`
	RowVersion          int64           `json:"row_version"`
	Targets             []RolloutTarget `json:"targets"`
	Activated           []string        `json:"activated,omitempty"`
	RollbackOfReleaseID *string         `json:"rollback_of_release_id,omitempty"`
}

type rolloutPlan struct {
	BatchSize int `json:"batch_size"`
	Batches   int `json:"batches"`
}

func validRolloutUID(uid string) bool {
	if len(uid) != 32 {
		return false
	}
	raw, err := hex.DecodeString(uid)
	return err == nil && len(raw) == 16 && hex.EncodeToString(raw) == uid
}

type rollbackIntent struct {
	SourceReleaseID    string
	ExpectedRowVersion int64
}

// PublishRollout freezes an explicitly bounded target set and activates batch 0
// in one transaction. Later batches remain QUEUED without changing Desired.
func (s *Store) PublishRollout(ctx context.Context, workspace, actor, configID string, uids []string, batchSize int, key string) (RolloutView, error) {
	return s.publishRollout(ctx, workspace, actor, configID, uids, batchSize, key, nil)
}

// publishRollout reuses the P3.1 transaction for a compensating Release.
func (s *Store) publishRollout(ctx context.Context, workspace, actor, configID string, uids []string, batchSize int, key string, rollback *rollbackIntent) (RolloutView, error) {
	// 1. 在开事务前规范化目标，拒绝重复 UID、无界批量和无效作用域。
	if workspace == "" || utf8.RuneCountInString(workspace) > 96 ||
		actor == "" || utf8.RuneCountInString(actor) > 128 ||
		len(configID) != 36 || utf8.RuneCountInString(key) > 128 ||
		len(uids) < 2 || len(uids) > maxRolloutTargets || batchSize < 1 || batchSize > len(uids) {
		return RolloutView{}, ErrInvalidRollout
	}
	targets := slices.Clone(uids)
	slices.Sort(targets)
	for i, uid := range targets {
		if !validRolloutUID(uid) || i > 0 && uid == targets[i-1] {
			return RolloutView{}, ErrInvalidRollout
		}
	}
	plan := rolloutPlan{BatchSize: batchSize, Batches: (len(targets) + batchSize - 1) / batchSize}
	kind := "batch-rollout"
	sourceID := ""
	var sourceVersion int64
	if rollback != nil {
		kind = "compensating-rollback"
		sourceID = rollback.SourceReleaseID
		sourceVersion = rollback.ExpectedRowVersion
	}
	request, err := json.Marshal(struct {
		Kind             string   `json:"kind"`
		ConfigRevisionID string   `json:"config_revision_id"`
		InstanceUIDs     []string `json:"instance_uids"`
		BatchSize        int      `json:"batch_size"`
		Actor            string   `json:"actor"`
		SourceReleaseID  string   `json:"source_release_id,omitempty"`
		SourceRowVersion int64    `json:"source_row_version,omitempty"`
	}{kind, configID, targets, batchSize, actor, sourceID, sourceVersion})
	if err != nil {
		return RolloutView{}, err
	}
	sum := sha256.Sum256(request)
	requestHash := hex.EncodeToString(sum[:])
	var result RolloutView

	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 2. 固定 Agent 锁顺序与单 Agent Release 一致，保证 Generation 快照。
		agents := make(map[string]AgentRecord, len(targets))
		assignments := make(map[string]*AgentAssignmentRecord, len(targets))
		for _, uid := range targets {
			var agent AgentRecord
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
				Where("workspace_id = ? AND instance_uid = ?", workspace, uid).Take(&agent).Error; err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					return ErrNotFound
				}
				return err
			}
			agents[uid] = agent
		}
		if key != "" {
			var previous ConfigReleaseRecord
			err := tx.Where("workspace_id = ? AND idempotency_key = ?", workspace, key).Take(&previous).Error
			if err == nil {
				if previous.RequestSHA256 != requestHash {
					return ErrReleaseConflict
				}
				result, err = rolloutViewTx(tx, previous)
				return err
			}
			if !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}
		}
		if rollback != nil {
			// 幂等回放后锁定源 Release，验证其类型、CAS 和可补偿状态。
			var source ConfigReleaseRecord
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
				Where("workspace_id = ? AND release_id = ?", workspace, rollback.SourceReleaseID).
				Take(&source).Error; err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					return ErrRolloutNotFound
				}
				return err
			}
			var sourcePlan rolloutPlan
			if err := json.Unmarshal([]byte(source.RolloutPlanJSON), &sourcePlan); err != nil || sourcePlan.BatchSize < 1 {
				return ErrRolloutNotFound
			}
			if source.RowVersion != rollback.ExpectedRowVersion {
				return ErrAssignmentConflict
			}
			switch source.Status {
			case "RUNNING", "PAUSED", "COMPLETED":
			default:
				return ErrRolloutBlocked
			}
			var sourceTargets []ReleaseTargetRecord
			if err := tx.Where("workspace_id = ? AND release_id = ?", workspace, source.ReleaseID).
				Order("instance_uid").Find(&sourceTargets).Error; err != nil {
				return err
			}
			if len(sourceTargets) != len(targets) {
				return ErrInvalidRollout
			}
			for i, target := range sourceTargets {
				if target.InstanceUID != targets[i] || target.ConfigRevisionID == configID {
					return ErrInvalidRollout
				}
			}
		}
		var revision ConfigRevisionRecord
		if err := tx.Where("workspace_id = ? AND config_revision_id = ?", workspace, configID).
			Take(&revision).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrConfigRevisionNotFound
			}
			return err
		}
		if revision.AgentKind != config.JavaAgentKind || revision.ConfigDialect != config.JavaSamplingDialect ||
			revision.HashAlgorithm != config.HashAlgorithm || revision.PayloadFormat != config.ConfigMapFormat ||
			len(revision.ConfigHash) != 64 || revision.PayloadSizeBytes < 1 {
			return ErrUnsupportedReleaseTarget
		}
		for _, uid := range targets {
			agent := agents[uid]
			if agent.AgentKind != config.JavaAgentKind ||
				revision.Distribution != "" && revision.Distribution != agent.Distribution ||
				uint64(agent.Capabilities)&uint64(protobufs.AgentCapabilities_AgentCapabilities_AcceptsRemoteConfig) == 0 {
				return ErrUnsupportedReleaseTarget
			}
			var current AgentAssignmentRecord
			err := tx.Where("workspace_id = ? AND instance_uid = ?", workspace, uid).Take(&current).Error
			if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}
			if err == nil {
				if current.DesiredGeneration == math.MaxInt64 || current.RowVersion == math.MaxInt64 {
					return ErrAssignmentConflict
				}
				assignments[uid] = &current
			}
		}

		// 3. 同事务冻结全部目标的 ConfigRevision、BatchNo 和预期 Generation。
		id, err := newReleaseUUID()
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		selector, err := json.Marshal(struct {
			InstanceUIDs []string `json:"instance_uids"`
		}{targets})
		if err != nil {
			return err
		}
		rolloutJSON, err := json.Marshal(plan)
		if err != nil {
			return err
		}
		var idem *string
		if key != "" {
			idem = &key
		}
		release := ConfigReleaseRecord{
			WorkspaceID: workspace, ReleaseID: id, AgentKind: revision.AgentKind,
			TargetSelectorJSON: string(selector), RequestSHA256: requestHash,
			IdempotencyKey: idem, RolloutPlanJSON: string(rolloutJSON),
			Status: "RUNNING", RowVersion: 1, RequestedBy: actor,
			RollbackOfReleaseID: func() *string {
				if rollback != nil {
					return &rollback.SourceReleaseID
				}
				return nil
			}(),
			CreatedAt: now, StartedAt: &now,
		}
		if err := tx.Create(&release).Error; err != nil {
			if key != "" && errors.Is(err, gorm.ErrDuplicatedKey) {
				return ErrReleaseConflict
			}
			return err
		}
		if rollback != nil {
			id, err := newReleaseUUID()
			if err != nil {
				return err
			}
			details, err := json.Marshal(struct {
				SourceReleaseID  string `json:"source_release_id"`
				ConfigRevisionID string `json:"config_revision_id"`
			}{rollback.SourceReleaseID, configID})
			if err != nil {
				return err
			}
			if err := tx.Create(&ConfigEventRecord{
				WorkspaceID: workspace, EventID: id, EventType: "ROLLBACK_REQUESTED",
				ReleaseID: &release.ReleaseID, ActorType: "ADMIN", ActorID: actor,
				DetailsJSON: string(details), OccurredAt: now,
			}).Error; err != nil {
				return err
			}
		}
		for i, uid := range targets {
			generation := int64(1)
			if current := assignments[uid]; current != nil {
				generation = current.DesiredGeneration + 1
			}
			status := "QUEUED"
			if i < batchSize {
				status = "PENDING"
			}
			target := ReleaseTargetRecord{
				WorkspaceID: workspace, ReleaseID: id, InstanceUID: uid,
				ConfigRevisionID: configID, BatchNo: i / batchSize, Status: status,
				DesiredGeneration: generation, LastError: "", CreatedAt: now, UpdatedAt: now,
			}
			if err := tx.Create(&target).Error; err != nil {
				return err
			}
			if err := rolloutEventTx(tx, workspace, id, uid, generation, "TARGET_FROZEN", actor, now); err != nil {
				return err
			}
		}

		// 4. 只对首批提交 Desired；未激活目标保留原 Assignment，等待显式 Advance。
		for _, uid := range targets[:batchSize] {
			current := assignments[uid]
			generation := int64(1)
			if current != nil {
				generation = current.DesiredGeneration + 1
			}
			if err := rolloutAssignTx(tx, workspace, uid, id, configID, revision.ConfigHash, generation, current, actor, now); err != nil {
				return err
			}
			if err := rolloutEventTx(tx, workspace, id, uid, generation, "TARGET_ASSIGNED", actor, now); err != nil {
				return err
			}
		}
		result, err = rolloutViewTx(tx, release)
		if err == nil {
			result.Activated = slices.Clone(targets[:batchSize])
		}
		return err
	})
	return result, err
}

// AdvanceRollout starts exactly the next frozen batch after all earlier targets
// are APPLIED. It never activates a partial batch after a stale Generation.
func (s *Store) AdvanceRollout(ctx context.Context, workspace, releaseID, actor string) (RolloutView, error) {
	if !policyIdentityOK(workspace, actor) || len(releaseID) != 36 {
		return RolloutView{}, ErrInvalidRollout
	}
	// 1. 不可变目标列表在事务外读取，避免 MySQL RR 创建过早的读快照。
	var frozen []ReleaseTargetRecord
	if err := s.db.WithContext(ctx).Where("workspace_id = ? AND release_id = ?", workspace, releaseID).
		Order("instance_uid").Find(&frozen).Error; err != nil {
		return RolloutView{}, err
	}
	if len(frozen) == 0 {
		return RolloutView{}, ErrRolloutNotFound
	}
	var result RolloutView
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var err error
		// 2. 按 UID 顺序锁住所有冻结 Agent，再读取 Release 和最新 Target 状态。
		agents := make(map[string]AgentRecord, len(frozen))
		for _, target := range frozen {
			var agent AgentRecord
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
				Where("workspace_id = ? AND instance_uid = ?", workspace, target.InstanceUID).
				Take(&agent).Error; err != nil {
				return err
			}
			agents[target.InstanceUID] = agent
		}
		var release ConfigReleaseRecord
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("workspace_id = ? AND release_id = ?", workspace, releaseID).
			Take(&release).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrRolloutNotFound
			}
			return err
		}
		var plan rolloutPlan
		if err := json.Unmarshal([]byte(release.RolloutPlanJSON), &plan); err != nil || plan.BatchSize < 1 {
			return ErrInvalidRollout
		}
		if release.Status == "PAUSED" {
			return ErrRolloutBlocked
		}
		if release.Status == "COMPLETED" {
			var err error
			result, err = rolloutViewTx(tx, release)
			return err
		}
		if release.Status != "RUNNING" {
			return ErrRolloutBlocked
		}
		if err := tx.Where("workspace_id = ? AND release_id = ?", workspace, releaseID).
			Order("batch_no, instance_uid").Find(&frozen).Error; err != nil {
			return err
		}

		// 3. 已启动批次必须全部 APPLIED；失败或被替代时禁止继续扩散。
		nextBatch := -1
		blocked := ""
		for _, target := range frozen {
			if target.Status == "QUEUED" {
				if nextBatch < 0 {
					nextBatch = target.BatchNo
				}
				continue
			}
			if target.Status == "FAILED" || target.Status == "UNSUPPORTED" ||
				target.Status == "TIMED_OUT" || target.Status == "SUPERSEDED" {
				blocked = target.InstanceUID
				break
			}
			if target.Status != "APPLIED" {
				return ErrRolloutNotReady
			}
		}
		if blocked != "" {
			now := time.Now().UTC()
			if err := rolloutStateTx(tx, release, "PAUSED", now); err != nil {
				return err
			}
			if err := rolloutEventTx(tx, workspace, releaseID, blocked, 0, "ROLLOUT_PAUSED", actor, now); err != nil {
				return err
			}
			result, err = rolloutViewTx(tx, release)
			if err == nil {
				result.Status = "PAUSED"
				result.RowVersion++
			}
			return err
		}
		if nextBatch < 0 {
			return ErrRolloutNotReady
		}
		var next []ReleaseTargetRecord
		for _, target := range frozen {
			if target.BatchNo == nextBatch {
				next = append(next, target)
			}
		}
		var revision ConfigRevisionRecord
		if err := tx.Where("workspace_id = ? AND config_revision_id = ?", workspace, next[0].ConfigRevisionID).
			Take(&revision).Error; err != nil {
			return err
		}
		current := make(map[string]*AgentAssignmentRecord, len(next))
		blocked = ""
		blockedStatus := ""
		for _, target := range next {
			agent := agents[target.InstanceUID]
			if agent.AgentKind != config.JavaAgentKind ||
				revision.Distribution != "" && revision.Distribution != agent.Distribution ||
				uint64(agent.Capabilities)&uint64(protobufs.AgentCapabilities_AgentCapabilities_AcceptsRemoteConfig) == 0 {
				blocked, blockedStatus = target.InstanceUID, "UNSUPPORTED"
				break
			}
			var assignment AgentAssignmentRecord
			err := tx.Where("workspace_id = ? AND instance_uid = ?", workspace, target.InstanceUID).
				Take(&assignment).Error
			if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}
			generation := int64(1)
			if err == nil {
				if assignment.DesiredGeneration == math.MaxInt64 || assignment.RowVersion == math.MaxInt64 {
					return ErrAssignmentConflict
				}
				generation = assignment.DesiredGeneration + 1
				current[target.InstanceUID] = &assignment
			}
			if generation != target.DesiredGeneration {
				blocked, blockedStatus = target.InstanceUID, "SUPERSEDED"
				break
			}
		}
		now := time.Now().UTC()
		if blocked != "" {
			// 4. 后续目标发生能力变化或 Generation 冲突时记录暂停，不激活任何目标。
			if err := tx.Model(&ReleaseTargetRecord{}).
				Where("workspace_id = ? AND release_id = ? AND instance_uid = ? AND status = 'QUEUED'",
					workspace, releaseID, blocked).
				Updates(map[string]any{"status": blockedStatus, "updated_at": now}).Error; err != nil {
				return err
			}
			if err := rolloutStateTx(tx, release, "PAUSED", now); err != nil {
				return err
			}
			if err := rolloutEventTx(tx, workspace, releaseID, blocked, 0, "ROLLOUT_PAUSED", actor, now); err != nil {
				return err
			}
			result, err = rolloutViewTx(tx, release)
			if err == nil {
				result.Status = "PAUSED"
				result.RowVersion++
			}
			return err
		}

		// 5. 下一批所有 Target/Assignment/审计在一个事务中推进。
		for _, target := range next {
			r := tx.Model(&ReleaseTargetRecord{}).
				Where("workspace_id = ? AND release_id = ? AND instance_uid = ? AND status = 'QUEUED'",
					workspace, releaseID, target.InstanceUID).
				Updates(map[string]any{"status": "PENDING", "updated_at": now})
			if r.Error != nil {
				return r.Error
			}
			if r.RowsAffected != 1 {
				return ErrAssignmentConflict
			}
			if err := rolloutAssignTx(tx, workspace, target.InstanceUID, releaseID, target.ConfigRevisionID,
				revision.ConfigHash, target.DesiredGeneration, current[target.InstanceUID], actor, now); err != nil {
				return err
			}
			if err := rolloutEventTx(tx, workspace, releaseID, target.InstanceUID,
				target.DesiredGeneration, "TARGET_ASSIGNED", actor, now); err != nil {
				return err
			}
		}
		if err := rolloutStateTx(tx, release, "RUNNING", now); err != nil {
			return err
		}
		result, err = rolloutViewTx(tx, release)
		if err != nil {
			return err
		}
		for _, target := range next {
			result.Activated = append(result.Activated, target.InstanceUID)
		}
		result.RowVersion++
		return nil
	})
	return result, err
}

func rolloutAssignTx(tx *gorm.DB, workspace, uid, releaseID, configID, hash string,
	generation int64, current *AgentAssignmentRecord, actor string, now time.Time) error {
	if current == nil {
		return tx.Create(&AgentAssignmentRecord{
			WorkspaceID: workspace, InstanceUID: uid, ReleaseID: releaseID, ConfigRevisionID: configID,
			DesiredHash: hash, DesiredGeneration: generation, RowVersion: 1,
			State: "PENDING", LastError: "", UpdatedAt: now,
		}).Error
	}
	r := tx.Model(&AgentAssignmentRecord{}).
		Where("workspace_id = ? AND instance_uid = ? AND desired_generation = ? AND row_version = ?",
			workspace, uid, current.DesiredGeneration, current.RowVersion).
		Updates(map[string]any{
			"release_id": releaseID, "config_revision_id": configID, "desired_hash": hash,
			"desired_generation": generation, "row_version": current.RowVersion + 1,
			"state": "PENDING", "attempt_count": 0, "last_sent_at": nil,
			"next_retry_at": nil, "last_error": "", "updated_at": now,
		})
	if r.Error != nil {
		return r.Error
	}
	if r.RowsAffected != 1 {
		return ErrAssignmentConflict
	}
	return supersedePreviousTargetTx(tx, *current, actor, now)
}

// supersedePreviousTargetTx stops the old multi-Agent rollout as soon as an
// active Target loses its Desired, rather than waiting for another ACK/Advance.
func supersedePreviousTargetTx(tx *gorm.DB, current AgentAssignmentRecord, actor string, now time.Time) error {
	// 1. 锁定旧 Target，避免与同一 Agent 的状态回执交错。
	var target ReleaseTargetRecord
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("workspace_id = ? AND release_id = ? AND instance_uid = ?",
			current.WorkspaceID, current.ReleaseID, current.InstanceUID).
		Take(&target).Error; err != nil {
		return err
	}
	switch target.Status {
	case "PENDING", "WAITING_ACK", "APPLYING", "APPLIED":
	default:
		return nil
	}

	// 2. 已完成的历史发布保留 APPLIED；未完成的多 Agent 发布立即暂停。
	var previous ConfigReleaseRecord
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("workspace_id = ? AND release_id = ?", current.WorkspaceID, current.ReleaseID).
		Take(&previous).Error; err != nil {
		return err
	}
	var plan rolloutPlan
	if err := json.Unmarshal([]byte(previous.RolloutPlanJSON), &plan); err != nil {
		return err
	}
	if target.Status == "APPLIED" && (plan.BatchSize == 0 || previous.Status != "RUNNING") {
		return nil
	}
	r := tx.Model(&ReleaseTargetRecord{}).
		Where("workspace_id = ? AND release_id = ? AND instance_uid = ? AND status = ?",
			current.WorkspaceID, current.ReleaseID, current.InstanceUID, target.Status).
		Updates(map[string]any{"status": "SUPERSEDED", "updated_at": now})
	if r.Error != nil {
		return r.Error
	}
	if r.RowsAffected != 1 {
		return ErrAssignmentConflict
	}
	if plan.BatchSize == 0 || previous.Status != "RUNNING" {
		return nil
	}
	if err := rolloutStateTx(tx, previous, "PAUSED", now); err != nil {
		return err
	}
	return rolloutEventTx(tx, current.WorkspaceID, current.ReleaseID, current.InstanceUID,
		current.DesiredGeneration, "ROLLOUT_PAUSED", actor, now)
}

func rolloutEventTx(tx *gorm.DB, workspace, releaseID, uid string, generation int64,
	eventType, actor string, now time.Time) error {
	id, err := newReleaseUUID()
	if err != nil {
		return err
	}
	var gen *int64
	if generation > 0 {
		gen = &generation
	}
	return tx.Create(&ConfigEventRecord{
		WorkspaceID: workspace, EventID: id, ReleaseID: &releaseID, InstanceUID: &uid,
		DesiredGeneration: gen, EventType: eventType, ActorType: "ADMIN",
		ActorID: actor, DetailsJSON: "{}", OccurredAt: now,
	}).Error
}

func rolloutStateTx(tx *gorm.DB, release ConfigReleaseRecord, status string, now time.Time) error {
	if release.RowVersion == math.MaxInt64 {
		return ErrAssignmentConflict
	}
	updates := map[string]any{"status": status, "row_version": release.RowVersion + 1}
	if status == "PAUSED" || status == "COMPLETED" {
		updates["finished_at"] = now
	} else if status == "RUNNING" {
		updates["finished_at"] = nil
	}
	r := tx.Model(&ConfigReleaseRecord{}).
		Where("workspace_id = ? AND release_id = ? AND row_version = ?",
			release.WorkspaceID, release.ReleaseID, release.RowVersion).Updates(updates)
	if r.Error != nil {
		return r.Error
	}
	if r.RowsAffected != 1 {
		return ErrAssignmentConflict
	}
	return nil
}

func rolloutViewTx(tx *gorm.DB, release ConfigReleaseRecord) (RolloutView, error) {
	var frozen []ReleaseTargetRecord
	err := tx.Where("workspace_id = ? AND release_id = ?", release.WorkspaceID, release.ReleaseID).
		Order("batch_no, instance_uid").Find(&frozen).Error
	if err != nil {
		return RolloutView{}, err
	}
	result := RolloutView{WorkspaceID: release.WorkspaceID, ReleaseID: release.ReleaseID,
		Status: release.Status, RowVersion: release.RowVersion, RollbackOfReleaseID: release.RollbackOfReleaseID, Targets: make([]RolloutTarget, 0, len(frozen))}
	for _, target := range frozen {
		result.Targets = append(result.Targets, RolloutTarget{
			InstanceUID: target.InstanceUID, ConfigRevisionID: target.ConfigRevisionID,
			BatchNo: target.BatchNo, Status: target.Status, DesiredGeneration: target.DesiredGeneration,
		})
	}
	return result, nil
}

// ResumeRollout only clears PAUSED after a matching explicit Admin CAS.
// No failed or superseded Target is silently retried or replaced.
func (s *Store) ResumeRollout(ctx context.Context, workspace, releaseID, actor string, expectedVersion int64) (RolloutView, error) {
	if !policyIdentityOK(workspace, actor) || len(releaseID) != 36 || expectedVersion < 1 {
		return RolloutView{}, ErrInvalidRollout
	}

	// 1. 在事务外读取不可变目标 UID，保证后续锁顺序与发布/Advance 一致。
	var frozen []ReleaseTargetRecord
	if err := s.db.WithContext(ctx).Where("workspace_id = ? AND release_id = ?", workspace, releaseID).
		Order("instance_uid").Find(&frozen).Error; err != nil {
		return RolloutView{}, err
	}
	if len(frozen) == 0 {
		return RolloutView{}, ErrRolloutNotFound
	}
	var result RolloutView
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 2. 依 UID 锁 Agent，再锁 Release 和读取其冻结目标最新状态。
		for _, target := range frozen {
			var agent AgentRecord
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
				Where("workspace_id = ? AND instance_uid = ?", workspace, target.InstanceUID).
				Take(&agent).Error; err != nil {
				return err
			}
		}
		var release ConfigReleaseRecord
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("workspace_id = ? AND release_id = ?", workspace, releaseID).
			Take(&release).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrRolloutNotFound
			}
			return err
		}
		var plan rolloutPlan
		if err := json.Unmarshal([]byte(release.RolloutPlanJSON), &plan); err != nil || plan.BatchSize < 1 {
			return ErrRolloutNotFound
		}
		if release.RowVersion != expectedVersion {
			return ErrAssignmentConflict
		}
		if release.Status != "PAUSED" {
			return ErrRolloutBlocked
		}
		if err := tx.Where("workspace_id = ? AND release_id = ?", workspace, releaseID).
			Order("batch_no, instance_uid").Find(&frozen).Error; err != nil {
			return err
		}

		// 3. 只有当前 Assignment 确认已应用的目标可计入恢复；队列不得有损坏。
		queued := 0
		for _, target := range frozen {
			switch target.Status {
			case "QUEUED":
				queued++
				var assignment AgentAssignmentRecord
				err := tx.Where("workspace_id = ? AND instance_uid = ?", workspace, target.InstanceUID).
					Take(&assignment).Error
				if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
					return err
				}
				generation := int64(1)
				if err == nil {
					if assignment.DesiredGeneration == math.MaxInt64 {
						return ErrRolloutBlocked
					}
					generation = assignment.DesiredGeneration + 1
				}
				if generation != target.DesiredGeneration {
					return ErrRolloutBlocked
				}
			case "APPLIED":
				var assignment AgentAssignmentRecord
				if err := tx.Where("workspace_id = ? AND instance_uid = ?", workspace, target.InstanceUID).
					Take(&assignment).Error; err != nil {
					return err
				}
				if assignment.ReleaseID != releaseID || assignment.DesiredGeneration != target.DesiredGeneration ||
					assignment.State != "APPLIED" {
					return ErrRolloutBlocked
				}
			default:
				return ErrRolloutBlocked
			}
		}

		// 4. 恢复仅更新 Release 与审计；后续批次仍需显式 Advance。
		now := time.Now().UTC()
		status := "RUNNING"
		if queued == 0 {
			status = "COMPLETED"
		}
		if err := rolloutStateTx(tx, release, status, now); err != nil {
			return err
		}
		if err := rolloutEventTx(tx, workspace, releaseID, frozen[0].InstanceUID, 0,
			"ROLLOUT_RESUMED", actor, now); err != nil {
			return err
		}
		var err error
		result, err = rolloutViewTx(tx, release)
		if err != nil {
			return err
		}
		result.Status = status
		result.RowVersion++
		return nil
	})
	return result, err
}

func (s *Store) GetRollout(ctx context.Context, workspace, releaseID string) (RolloutView, error) {
	var release ConfigReleaseRecord
	err := s.db.WithContext(ctx).Where("workspace_id = ? AND release_id = ?", workspace, releaseID).Take(&release).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return RolloutView{}, ErrRolloutNotFound
	}
	if err != nil {
		return RolloutView{}, err
	}
	var plan rolloutPlan
	if err := json.Unmarshal([]byte(release.RolloutPlanJSON), &plan); err != nil || plan.BatchSize < 1 {
		return RolloutView{}, ErrRolloutNotFound
	}
	return rolloutViewTx(s.db.WithContext(ctx), release)
}
