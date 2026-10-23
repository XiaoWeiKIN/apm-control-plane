package gormrepo

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"time"
	"unicode/utf8"

	"github.com/XiaoWeiKIN/apm-control-plane/internal/config"
	"github.com/open-telemetry/opamp-go/protobufs"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	ErrReleaseConflict          = errors.New("release idempotency conflict")
	ErrUnsupportedReleaseTarget = errors.New("agent does not support the selected config revision")
	ErrInvalidReleaseRequest    = errors.New("invalid release request")
	ErrAssignmentConflict       = errors.New("agent assignment changed concurrently")
)

type ConfigReleaseRecord struct {
	WorkspaceID         string     `gorm:"column:workspace_id;primaryKey"`
	ReleaseID           string     `gorm:"column:release_id;primaryKey"`
	PolicyRevisionID    *string    `gorm:"column:policy_revision_id"`
	AgentKind           string     `gorm:"column:agent_kind"`
	TargetSelectorJSON  string     `gorm:"column:target_selector_json"`
	RequestSHA256       string     `gorm:"column:request_sha256"`
	IdempotencyKey      *string    `gorm:"column:idempotency_key"`
	RolloutPlanJSON     string     `gorm:"column:rollout_plan_json"`
	Status              string     `gorm:"column:status"`
	RowVersion          int64      `gorm:"column:row_version"`
	RollbackOfReleaseID *string    `gorm:"column:rollback_of_release_id"`
	RequestedBy         string     `gorm:"column:requested_by"`
	CreatedAt           time.Time  `gorm:"column:created_at"`
	StartedAt           *time.Time `gorm:"column:started_at"`
	FinishedAt          *time.Time `gorm:"column:finished_at"`
}

func (ConfigReleaseRecord) TableName() string { return "config_releases" }

type ReleaseTargetRecord struct {
	WorkspaceID       string    `gorm:"column:workspace_id;primaryKey"`
	ReleaseID         string    `gorm:"column:release_id;primaryKey"`
	InstanceUID       string    `gorm:"column:instance_uid;primaryKey"`
	ConfigRevisionID  string    `gorm:"column:config_revision_id"`
	BatchNo           int       `gorm:"column:batch_no"`
	Status            string    `gorm:"column:status"`
	DesiredGeneration int64     `gorm:"column:desired_generation"`
	LastError         string    `gorm:"column:last_error"`
	CreatedAt         time.Time `gorm:"column:created_at"`
	UpdatedAt         time.Time `gorm:"column:updated_at"`
}

func (ReleaseTargetRecord) TableName() string { return "release_targets" }

type AgentAssignmentRecord struct {
	WorkspaceID       string     `gorm:"column:workspace_id;primaryKey"`
	InstanceUID       string     `gorm:"column:instance_uid;primaryKey"`
	ReleaseID         string     `gorm:"column:release_id"`
	ConfigRevisionID  string     `gorm:"column:config_revision_id"`
	DesiredHash       string     `gorm:"column:desired_hash"`
	DesiredGeneration int64      `gorm:"column:desired_generation"`
	RowVersion        int64      `gorm:"column:row_version"`
	State             string     `gorm:"column:state"`
	ObservedHash      *string    `gorm:"column:observed_hash"`
	ObservedStatus    *string    `gorm:"column:observed_status"`
	ObservedAt        *time.Time `gorm:"column:observed_at"`
	AttemptCount      int        `gorm:"column:attempt_count"`
	LastSentAt        *time.Time `gorm:"column:last_sent_at"`
	NextRetryAt       *time.Time `gorm:"column:next_retry_at"`
	LastError         string     `gorm:"column:last_error"`
	UpdatedAt         time.Time  `gorm:"column:updated_at"`
}

func (AgentAssignmentRecord) TableName() string { return "agent_assignments" }

type ConfigEventRecord struct {
	WorkspaceID       string    `gorm:"column:workspace_id;primaryKey"`
	EventID           string    `gorm:"column:event_id;primaryKey"`
	EventType         string    `gorm:"column:event_type"`
	ReleaseID         *string   `gorm:"column:release_id"`
	InstanceUID       *string   `gorm:"column:instance_uid"`
	DesiredGeneration *int64    `gorm:"column:desired_generation"`
	ConfigHash        *string   `gorm:"column:config_hash"`
	ReportedStatus    *string   `gorm:"column:reported_status"`
	ActorType         string    `gorm:"column:actor_type"`
	ActorID           string    `gorm:"column:actor_id"`
	CorrelationID     *string   `gorm:"column:correlation_id"`
	DetailsJSON       string    `gorm:"column:details_json"`
	OccurredAt        time.Time `gorm:"column:occurred_at"`
}

func (ConfigEventRecord) TableName() string { return "config_events" }

// ReleaseResult represents persisted release intent, not delivery or Agent application.
type ReleaseResult struct {
	WorkspaceID       string `json:"workspace_id"`
	ReleaseID         string `json:"release_id"`
	InstanceUID       string `json:"instance_uid"`
	ConfigRevisionID  string `json:"config_revision_id"`
	DesiredGeneration int64  `json:"desired_generation"`
	Status            string `json:"status"`
}

func newReleaseUUID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	raw[6] = raw[6]&0x0f | 0x40
	raw[8] = raw[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", raw[:4], raw[4:6], raw[6:8], raw[8:10], raw[10:]), nil
}

// PublishAgentConfig creates one frozen target and replaces one Agent's Desired.
// The committed Desired is delivered asynchronously by the existing Reconciler.
func (s *Store) PublishAgentConfig(ctx context.Context, workspaceID, instanceUID, configRevisionID, requestedBy, idempotencyKey string) (ReleaseResult, error) {
	// 1. 拒绝非法 Workspace、Agent、Revision 和幂等键，不启动无效事务。
	if workspaceID == "" || utf8.RuneCountInString(workspaceID) > 96 ||
		requestedBy == "" || utf8.RuneCountInString(requestedBy) > 128 ||
		len(instanceUID) != 32 || len(configRevisionID) != 36 ||
		utf8.RuneCountInString(idempotencyKey) > 128 {
		return ReleaseResult{}, ErrInvalidReleaseRequest
	}
	rawUID, err := hex.DecodeString(instanceUID)
	if err != nil || len(rawUID) != 16 || hex.EncodeToString(rawUID) != instanceUID {
		return ReleaseResult{}, ErrInvalidReleaseRequest
	}
	hash := sha256.Sum256([]byte(instanceUID + "\x00" + configRevisionID + "\x00" + requestedBy))
	requestHash := hex.EncodeToString(hash[:])
	var result ReleaseResult

	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		agent, err := lockAgentForReleaseTx(tx, workspaceID, instanceUID)
		if err != nil {
			return err
		}
		return publishAgentConfigTx(tx, agent, workspaceID, instanceUID, configRevisionID, requestedBy,
			idempotencyKey, requestHash, nil, &result)
	})
	if err != nil {
		return ReleaseResult{}, err
	}
	return result, nil
}

// lockAgentForReleaseTx serializes publications targeting the same Agent.
func lockAgentForReleaseTx(tx *gorm.DB, workspaceID, instanceUID string) (AgentRecord, error) {
	var agent AgentRecord
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("workspace_id = ? AND instance_uid = ?", workspaceID, instanceUID).
		Take(&agent).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return AgentRecord{}, ErrNotFound
		}
		return AgentRecord{}, err
	}
	return agent, nil
}

// publishAgentConfigTx owns the Release/Target/Assignment/Event transaction body.
// Callers hold the Agent row lock in the same transaction; no OpAMP sends occur here.
func publishAgentConfigTx(tx *gorm.DB, agent AgentRecord, workspaceID, instanceUID, configRevisionID, requestedBy,
	idempotencyKey, requestHash string, sourcePolicyRevisionID *string, result *ReleaseResult) error {
	// 1. 已处理的同一幂等请求直接返回原 Release，不能再次更新 Desired。
	if idempotencyKey != "" {
		var previous ConfigReleaseRecord
		err := tx.Where("workspace_id = ? AND idempotency_key = ?", workspaceID, idempotencyKey).Take(&previous).Error
		if err == nil {
			if previous.RequestSHA256 != requestHash {
				return ErrReleaseConflict
			}
			var target ReleaseTargetRecord
			if err := tx.Where("workspace_id = ? AND release_id = ? AND instance_uid = ?", workspaceID, previous.ReleaseID, instanceUID).Take(&target).Error; err != nil {
				return err
			}
			*result = ReleaseResult{workspaceID, previous.ReleaseID, instanceUID, target.ConfigRevisionID, target.DesiredGeneration, target.Status}
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
	}

	// 2. 校验目标配置与 Agent 类型/远程配置能力匹配。
	var revision ConfigRevisionRecord
	if err := tx.Where("workspace_id = ? AND config_revision_id = ?", workspaceID, configRevisionID).Take(&revision).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrConfigRevisionNotFound
		}
		return err
	}
	if agent.AgentKind != config.JavaAgentKind || revision.AgentKind != config.JavaAgentKind ||
		revision.ConfigDialect != config.JavaSamplingDialect || revision.PayloadFormat != config.ConfigMapFormat ||
		revision.HashAlgorithm != config.HashAlgorithm ||
		(revision.Distribution != "" && revision.Distribution != agent.Distribution) ||
		uint64(agent.Capabilities)&uint64(protobufs.AgentCapabilities_AgentCapabilities_AcceptsRemoteConfig) == 0 {
		return ErrUnsupportedReleaseTarget
	}
	if len(revision.ConfigHash) != 64 || revision.PayloadSizeBytes <= 0 {
		return ErrUnsupportedReleaseTarget
	}

	// 3. 读取上次 Assignment，写入同事务的新 Release 与冻结目标。
	var current AgentAssignmentRecord
	err := tx.Where("workspace_id = ? AND instance_uid = ?", workspaceID, instanceUID).Take(&current).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	existing := err == nil
	generation := int64(1)
	version := int64(1)
	if existing {
		if current.DesiredGeneration == math.MaxInt64 || current.RowVersion == math.MaxInt64 {
			return ErrAssignmentConflict
		}
		generation = current.DesiredGeneration + 1
		version = current.RowVersion + 1
	}
	releaseID, err := newReleaseUUID()
	if err != nil {
		return fmt.Errorf("generate release id: %w", err)
	}
	eventID, err := newReleaseUUID()
	if err != nil {
		return fmt.Errorf("generate audit event id: %w", err)
	}
	now := time.Now().UTC()
	selector := fmt.Sprintf(`{"instance_uid":"%s"}`, instanceUID)
	idem := (*string)(nil)
	if idempotencyKey != "" {
		idem = &idempotencyKey
	}
	release := ConfigReleaseRecord{
		WorkspaceID: workspaceID, ReleaseID: releaseID, AgentKind: revision.AgentKind,
		TargetSelectorJSON: selector, RequestSHA256: requestHash, IdempotencyKey: idem,
		PolicyRevisionID: sourcePolicyRevisionID,
		RolloutPlanJSON:  `{"batches":1}`, Status: "PENDING", RowVersion: 1,
		RequestedBy: requestedBy, CreatedAt: now,
	}
	if err := tx.Create(&release).Error; err != nil {
		if idempotencyKey != "" && errors.Is(err, gorm.ErrDuplicatedKey) {
			return ErrReleaseConflict
		}
		return err
	}
	target := ReleaseTargetRecord{
		WorkspaceID: workspaceID, ReleaseID: releaseID, InstanceUID: instanceUID,
		ConfigRevisionID: configRevisionID, BatchNo: 0, Status: "PENDING",
		DesiredGeneration: generation, LastError: "", CreatedAt: now, UpdatedAt: now,
	}
	if err := tx.Create(&target).Error; err != nil {
		return err
	}

	// 4. 更新唯一当前 Desired，并保留先前 Observed 历史，防止旧状态冒认成功。
	if existing {
		updates := map[string]any{
			"release_id": releaseID, "config_revision_id": configRevisionID,
			"desired_hash": revision.ConfigHash, "desired_generation": generation,
			"row_version": version, "state": "PENDING", "attempt_count": 0,
			"last_sent_at": nil, "next_retry_at": nil, "last_error": "", "updated_at": now,
		}
		r := tx.Model(&AgentAssignmentRecord{}).
			Where("workspace_id = ? AND instance_uid = ? AND row_version = ? AND desired_generation = ?",
				workspaceID, instanceUID, current.RowVersion, current.DesiredGeneration).Updates(updates)
		if r.Error != nil {
			return r.Error
		}
		if r.RowsAffected != 1 {
			return ErrAssignmentConflict
		}
		if err := supersedePreviousTargetTx(tx, current, requestedBy, now); err != nil {
			return err
		}
	} else {
		assignment := AgentAssignmentRecord{
			WorkspaceID: workspaceID, InstanceUID: instanceUID, ReleaseID: releaseID,
			ConfigRevisionID: configRevisionID, DesiredHash: revision.ConfigHash,
			DesiredGeneration: 1, RowVersion: 1, State: "PENDING", LastError: "", UpdatedAt: now,
		}
		if err := tx.Create(&assignment).Error; err != nil {
			return err
		}
	}

	// 5. 审计事件和发布状态同事务提交，失败时不留下半完成发布。
	event := ConfigEventRecord{
		WorkspaceID: workspaceID, EventID: eventID, EventType: "TARGET_ASSIGNED",
		ReleaseID: &releaseID, InstanceUID: &instanceUID, DesiredGeneration: &generation,
		ConfigHash: &revision.ConfigHash, ActorType: "ADMIN", ActorID: requestedBy,
		DetailsJSON: "{}", OccurredAt: now,
	}
	if err := tx.Create(&event).Error; err != nil {
		return err
	}
	*result = ReleaseResult{workspaceID, releaseID, instanceUID, configRevisionID, generation, "PENDING"}
	return nil
}

func (s *Store) GetAgentAssignment(ctx context.Context, workspaceID, instanceUID string) (AgentAssignmentRecord, error) {
	var a AgentAssignmentRecord
	err := s.db.WithContext(ctx).Where("workspace_id = ? AND instance_uid = ?", workspaceID, instanceUID).Take(&a).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return AgentAssignmentRecord{}, ErrNotFound
	}
	return a, err
}
