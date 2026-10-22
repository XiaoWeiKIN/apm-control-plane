package gormrepo

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/XiaoWeiKIN/apm-control-plane/internal/config"
	"github.com/XiaoWeiKIN/apm-control-plane/internal/policy"
	"github.com/open-telemetry/opamp-go/protobufs"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// PublishAgentPolicy atomically resolves a selected Java policy, compiles a
// ConfigRevision and publishes a single-Agent Desired assignment.
func (s *Store) PublishAgentPolicy(ctx context.Context, workspaceID, instanceUID, expectedPolicyRevisionID, requestedBy, idempotencyKey string) (ReleaseResult, error) {
	// 1. 校验可信 Workspace、目标身份与期望 PolicyRevision，非法输入提前失败。
	if workspaceID == "" || utf8.RuneCountInString(workspaceID) > 96 ||
		requestedBy == "" || utf8.RuneCountInString(requestedBy) > 128 ||
		len(instanceUID) != 32 || len(expectedPolicyRevisionID) != 36 ||
		utf8.RuneCountInString(idempotencyKey) > 128 {
		return ReleaseResult{}, ErrInvalidReleaseRequest
	}
	uid, err := hex.DecodeString(instanceUID)
	if err != nil || len(uid) != 16 || hex.EncodeToString(uid) != instanceUID {
		return ReleaseResult{}, ErrInvalidReleaseRequest
	}
	hash := sha256.Sum256([]byte("policy\x00" + instanceUID + "\x00" + expectedPolicyRevisionID + "\x00" + requestedBy))
	requestHash := hex.EncodeToString(hash[:])
	var result ReleaseResult

	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 2. 与普通发布使用相同的 Agent 锁，先处理幂等重试，不重复编译或生成 Revision。
		var agent AgentRecord
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("workspace_id = ? AND instance_uid = ?", workspaceID, instanceUID).
			Take(&agent).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}
		if idempotencyKey != "" {
			var previous ConfigReleaseRecord
			err := tx.Where("workspace_id = ? AND idempotency_key = ?", workspaceID, idempotencyKey).Take(&previous).Error
			if err == nil {
				if previous.RequestSHA256 != requestHash {
					return ErrReleaseConflict
				}
				var target ReleaseTargetRecord
				if err := tx.Where("workspace_id = ? AND release_id = ? AND instance_uid = ?",
					workspaceID, previous.ReleaseID, instanceUID).Take(&target).Error; err != nil {
					return err
				}
				result = ReleaseResult{workspaceID, previous.ReleaseID, instanceUID,
					target.ConfigRevisionID, target.DesiredGeneration, target.Status}
				return nil
			}
			if !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}
		}

		// 3. 提前拒绝不具备 Java RemoteConfig 能力的 Agent，再解析策略与编译内容。
		if agent.AgentKind != config.JavaAgentKind ||
			uint64(agent.Capabilities)&uint64(protobufs.AgentCapabilities_AgentCapabilities_AcceptsRemoteConfig) == 0 {
			return ErrUnsupportedReleaseTarget
		}

		// 4. 按当前 Binding 重新解析目标并校验期望 Revision，拒绝静默发布其他版本。
		resolved, err := (&Store{db: tx}).ResolveAgentPolicy(ctx, workspaceID, instanceUID)
		if err != nil {
			return err
		}
		if resolved.PolicyRevisionID != expectedPolicyRevisionID {
			return policy.ErrConflict
		}
		var document struct {
			SchemaVersion int `json:"schema_version"`
			Sampling      struct {
				Type string  `json:"type"`
				Rate float64 `json:"rate"`
			} `json:"sampling"`
		}
		if err := json.Unmarshal(resolved.Body, &document); err != nil {
			return fmt.Errorf("decode policy revision: %w", err)
		}
		canonical, bodyHash, err := policy.JavaSamplingBody(document.Sampling.Rate)
		if err != nil || document.SchemaVersion != 1 ||
			document.Sampling.Type != "parent_based_trace_id_ratio" ||
			canonical != string(resolved.Body) || bodyHash != resolved.BodySHA256 {
			return policy.ErrInvalid
		}
		snapshot, err := config.CompileJavaSampling(document.Sampling.Rate)
		if err != nil {
			return policy.ErrInvalid
		}

		// 5. 同一事务保存完整不可变 ConfigMap 和 Policy 来源，不留下失败发布的孤立 Revision。
		configID, err := newReleaseUUID()
		if err != nil {
			return err
		}
		data := snapshot.ConfigMapBytes()
		sum := snapshot.Hash()
		configRevision := ConfigRevisionRecord{
			WorkspaceID: workspaceID, ConfigRevisionID: configID,
			SourcePolicyRevisionID: &expectedPolicyRevisionID,
			AgentKind:              snapshot.AgentKind(), ConfigDialect: snapshot.ConfigDialect(),
			PayloadFormat: config.ConfigMapFormat, ConfigMapBytes: data,
			ConfigHash: hex.EncodeToString(sum[:]), HashAlgorithm: config.HashAlgorithm,
			PayloadSizeBytes: int64(len(data)), CreatedAt: time.Now().UTC(), CreatedBy: requestedBy,
		}
		if err := tx.Create(&configRevision).Error; err != nil {
			return err
		}

		// 6. 复用 P2.2 的 Release/Target/Assignment/Event 原子事务，提交后由 Gateway 唤醒。
		return publishAgentConfigTx(tx, workspaceID, instanceUID, configID, requestedBy,
			idempotencyKey, requestHash, &expectedPolicyRevisionID, &result)
	})
	if err != nil {
		return ReleaseResult{}, err
	}
	return result, nil
}
