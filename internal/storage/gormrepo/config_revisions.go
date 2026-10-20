package gormrepo

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/XiaoWeiKIN/apm-control-plane/internal/config"
	"gorm.io/gorm"
)

var ErrConfigRevisionNotFound = errors.New("config revision not found")

// ConfigRevisionRecord stores an immutable, complete OpAMP ConfigMap snapshot.
type ConfigRevisionRecord struct {
	WorkspaceID            string    `gorm:"column:workspace_id;primaryKey"`
	ConfigRevisionID        string    `gorm:"column:config_revision_id;primaryKey"`
	SourcePolicyRevisionID  *string   `gorm:"column:source_policy_revision_id"`
	AgentKind               string    `gorm:"column:agent_kind"`
	Distribution            string    `gorm:"column:distribution"`
	ConfigDialect           string    `gorm:"column:config_dialect"`
	PayloadFormat           string    `gorm:"column:payload_format"`
	ConfigMapBytes          []byte    `gorm:"column:config_map_bytes"`
	ConfigHash              string    `gorm:"column:config_hash"`
	HashAlgorithm           string    `gorm:"column:hash_algorithm"`
	PayloadSizeBytes        int64     `gorm:"column:payload_size_bytes"`
	CreatedAt               time.Time `gorm:"column:created_at"`
	CreatedBy               string    `gorm:"column:created_by"`
}

func (ConfigRevisionRecord) TableName() string { return "config_revisions" }

// CreateConfigRevision creates a new immutable revision without changing
// any Agent's Desired configuration or sending any OpAMP message.
func (s *Store) CreateConfigRevision(ctx context.Context, workspaceID, createdBy string, snapshot config.Snapshot) (ConfigRevisionRecord, error) {
	// 1. 验证创建者、Workspace 与已编译的完整配置内容。
	if workspaceID == "" || utf8.RuneCountInString(workspaceID) > 96 {
		return ConfigRevisionRecord{}, fmt.Errorf("workspace_id is missing or too long")
	}
	if createdBy == "" || utf8.RuneCountInString(createdBy) > 128 {
		return ConfigRevisionRecord{}, fmt.Errorf("created_by is missing or too long")
	}
	data := snapshot.ConfigMapBytes()
	if len(data) == 0 {
		return ConfigRevisionRecord{}, fmt.Errorf("compiled ConfigMap is empty")
	}

	// 2. 分配独立的 Revision UUID；相同 Hash 不意味着同一次创建。
	idBytes := make([]byte, 16)
	if _, err := rand.Read(idBytes); err != nil {
		return ConfigRevisionRecord{}, fmt.Errorf("generate config revision id: %w", err)
	}
	idBytes[6] = idBytes[6]&0x0f | 0x40
	idBytes[8] = idBytes[8]&0x3f | 0x80
	id := fmt.Sprintf("%x-%x-%x-%x-%x", idBytes[:4], idBytes[4:6], idBytes[6:8], idBytes[8:10], idBytes[10:])
	hash := snapshot.Hash()
	record := ConfigRevisionRecord{
		WorkspaceID:       workspaceID,
		ConfigRevisionID:   id,
		AgentKind:          snapshot.AgentKind(),
		ConfigDialect:      snapshot.ConfigDialect(),
		PayloadFormat:      config.ConfigMapFormat,
		ConfigMapBytes:     data,
		ConfigHash:         hex.EncodeToString(hash[:]),
		HashAlgorithm:      config.HashAlgorithm,
		PayloadSizeBytes:   int64(len(data)),
		CreatedAt:          time.Now().UTC(),
		CreatedBy:          createdBy,
	}
	if err := s.db.WithContext(ctx).Create(&record).Error; err != nil {
		return ConfigRevisionRecord{}, fmt.Errorf("create config revision: %w", err)
	}
	return record, nil
}

func (s *Store) GetConfigRevision(ctx context.Context, workspaceID, id string) (ConfigRevisionRecord, error) {
	var record ConfigRevisionRecord
	err := s.db.WithContext(ctx).
		Where("workspace_id = ? AND config_revision_id = ?", workspaceID, id).
		Take(&record).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ConfigRevisionRecord{}, ErrConfigRevisionNotFound
	}
	return record, err
}
