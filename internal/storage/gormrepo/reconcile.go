package gormrepo

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/XiaoWeiKIN/apm-control-plane/internal/config"
	"github.com/open-telemetry/opamp-go/protobufs"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// DesiredConfig is the current immutable ConfigMap snapshot to offer via OpAMP.
type DesiredConfig struct {
	ConfigMapBytes []byte
	ConfigHash     string
}

const deliveryRetryInterval = 15 * time.Second

// ReconcileAgentConfig records an Agent receipt and offers the current durable
// Desired configuration. Offering a response is not proof of transport or apply.
func (s *Store) ReconcileAgentConfig(ctx context.Context, workspaceID, instanceUID string, reported *ConfigStatus) (*DesiredConfig, error) {
	// 1. 无发布意图的心跳走只读快速路径；不为每次轮询开启事务。
	var existing AgentAssignmentRecord
	err := s.db.WithContext(ctx).Where("workspace_id = ? AND instance_uid = ?", workspaceID, instanceUID).Take(&existing).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	var desired *DesiredConfig
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 2. 按发布事务的 Agent → Assignment 锁顺序串行化，防止旧回执覆盖新版本。
		var agent AgentRecord
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("workspace_id = ? AND instance_uid = ?", workspaceID, instanceUID).
			Take(&agent).Error; err != nil {
			return err
		}
		var assignment AgentAssignmentRecord
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("workspace_id = ? AND instance_uid = ?", workspaceID, instanceUID).
			Take(&assignment).Error; err != nil {
			return err
		}
		now := time.Now().UTC()

		// 3. 保留真实回执；只有已提供当前版本且 Hash 匹配才收敛。
		if reported != nil {
			oldHash, oldStatus := "", ""
			if assignment.ObservedHash != nil {
				oldHash = *assignment.ObservedHash
			}
			if assignment.ObservedStatus != nil {
				oldStatus = *assignment.ObservedStatus
			}
			state := assignment.State
			eventType := "CONFIG_ACK_STALE"
			matched := reported.Hash == assignment.DesiredHash && assignment.LastSentAt != nil
			if matched {
				switch reported.Status {
				case "APPLIED":
					state, eventType = "APPLIED", "CONFIG_APPLIED"
				case "FAILED":
					state, eventType = "FAILED", "CONFIG_FAILED"
				case "APPLYING":
					state, eventType = "APPLYING", "CONFIG_APPLYING"
				default:
					eventType = "CONFIG_STATUS_REPORTED"
				}
			} else if state == "APPLIED" && reported.Hash != assignment.DesiredHash {
				state = "PENDING"
			}
			if oldHash != reported.Hash || oldStatus != reported.Status || state != assignment.State {
				updates := map[string]any{
					"observed_hash": reported.Hash, "observed_status": reported.Status,
					"observed_at": now, "row_version": assignment.RowVersion + 1,
					"state": state, "updated_at": now,
				}
				r := tx.Model(&AgentAssignmentRecord{}).
					Where("workspace_id = ? AND instance_uid = ? AND desired_generation = ? AND row_version = ?",
						workspaceID, instanceUID, assignment.DesiredGeneration, assignment.RowVersion).Updates(updates)
				if r.Error != nil {
					return r.Error
				}
				if r.RowsAffected != 1 {
					return ErrAssignmentConflict
				}
				assignment.RowVersion++
				assignment.State = state
				if state == "APPLIED" || state == "FAILED" {
					if err := setReleaseDeliveryStatus(tx, assignment, state, now); err != nil {
						return err
					}
				}
				if err := insertDeliveryEvent(tx, assignment, eventType, &reported.Hash, &reported.Status, now); err != nil {
					return err
				}
			}
		}

		// 4. 已收敛或明确失败的版本不重发；等待 ACK 时仅按重试期限重新提供。
		switch assignment.State {
		case "APPLIED", "FAILED", "UNSUPPORTED", "SUPERSEDED":
			return nil
		}
		if assignment.NextRetryAt != nil && now.Before(*assignment.NextRetryAt) {
			return nil
		}

		// 5. 重新检查 Agent 类型、Capability 和不可变 ConfigMap，禁止下发不兼容内容。
		var revision ConfigRevisionRecord
		if err := tx.Where("workspace_id = ? AND config_revision_id = ?", workspaceID, assignment.ConfigRevisionID).
			Take(&revision).Error; err != nil {
			return err
		}
		if agent.AgentKind != config.JavaAgentKind || revision.AgentKind != config.JavaAgentKind ||
			(revision.Distribution != "" && agent.Distribution != revision.Distribution) ||
			revision.ConfigDialect != config.JavaSamplingDialect ||
			revision.PayloadFormat != config.ConfigMapFormat ||
			revision.HashAlgorithm != config.HashAlgorithm ||
			uint64(agent.Capabilities)&uint64(protobufs.AgentCapabilities_AgentCapabilities_AcceptsRemoteConfig) == 0 {
			r := tx.Model(&AgentAssignmentRecord{}).
				Where("workspace_id = ? AND instance_uid = ? AND desired_generation = ? AND row_version = ?",
					workspaceID, instanceUID, assignment.DesiredGeneration, assignment.RowVersion).
				Updates(map[string]any{"state": "UNSUPPORTED", "row_version": assignment.RowVersion + 1, "updated_at": now})
			if r.Error != nil {
				return r.Error
			}
			if r.RowsAffected != 1 {
				return ErrAssignmentConflict
			}
			if err := setReleaseDeliveryStatus(tx, assignment, "UNSUPPORTED", now); err != nil {
				return err
			}
			return insertDeliveryEvent(tx, assignment, "CONFIG_UNSUPPORTED", nil, nil, now)
		}
		sum := sha256.Sum256(revision.ConfigMapBytes)
		if revision.ConfigHash != assignment.DesiredHash || hex.EncodeToString(sum[:]) != assignment.DesiredHash {
			return fmt.Errorf("stored ConfigRevision hash does not match Desired")
		}
		if assignment.AttemptCount == math.MaxInt {
			return fmt.Errorf("config delivery attempts overflow")
		}

		// 6. 持久记录本次响应式提供的尝试，等待后续独立 ACK；事务内不进行网络发送。
		nextRetry := now.Add(deliveryRetryInterval)
		r := tx.Model(&AgentAssignmentRecord{}).
			Where("workspace_id = ? AND instance_uid = ? AND desired_generation = ? AND row_version = ?",
				workspaceID, instanceUID, assignment.DesiredGeneration, assignment.RowVersion).
			Updates(map[string]any{
				"state": "WAITING_ACK", "row_version": assignment.RowVersion + 1,
				"attempt_count": assignment.AttemptCount + 1, "last_sent_at": now,
				"next_retry_at": nextRetry, "updated_at": now,
			})
		if r.Error != nil {
			return r.Error
		}
		if r.RowsAffected != 1 {
			return ErrAssignmentConflict
		}
		if err := setReleaseDeliveryStatus(tx, assignment, "WAITING_ACK", now); err != nil {
			return err
		}
		if err := insertDeliveryEvent(tx, assignment, "CONFIG_OFFERED", &assignment.DesiredHash, nil, now); err != nil {
			return err
		}
		desired = &DesiredConfig{ConfigMapBytes: revision.ConfigMapBytes, ConfigHash: assignment.DesiredHash}
		return nil
	})
	return desired, err
}

// setReleaseDeliveryStatus applies to the current P2.2 single-target release.
func setReleaseDeliveryStatus(tx *gorm.DB, a AgentAssignmentRecord, state string, now time.Time) error {
	if err := tx.Model(&ReleaseTargetRecord{}).
		Where("workspace_id = ? AND release_id = ? AND instance_uid = ? AND desired_generation = ?",
			a.WorkspaceID, a.ReleaseID, a.InstanceUID, a.DesiredGeneration).
		Updates(map[string]any{"status": state, "updated_at": now}).Error; err != nil {
		return err
	}
	return tx.Model(&ConfigReleaseRecord{}).
		Where("workspace_id = ? AND release_id = ?", a.WorkspaceID, a.ReleaseID).
		Updates(map[string]any{"status": state}).Error
}

func insertDeliveryEvent(tx *gorm.DB, a AgentAssignmentRecord, eventType string, hash, status *string, now time.Time) error {
	id, err := newReleaseUUID()
	if err != nil {
		return err
	}
	releaseID, uid, generation := a.ReleaseID, a.InstanceUID, a.DesiredGeneration
	return tx.Create(&ConfigEventRecord{
		WorkspaceID: a.WorkspaceID, EventID: id, EventType: eventType,
		ReleaseID: &releaseID, InstanceUID: &uid, DesiredGeneration: &generation,
		ConfigHash: hash, ReportedStatus: status, ActorType: "AGENT",
		ActorID: uid, DetailsJSON: "{}", OccurredAt: now,
	}).Error
}
