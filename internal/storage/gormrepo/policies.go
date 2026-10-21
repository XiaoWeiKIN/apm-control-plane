package gormrepo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"
	"unicode/utf8"

	"github.com/XiaoWeiKIN/apm-control-plane/internal/config"
	"github.com/XiaoWeiKIN/apm-control-plane/internal/policy"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type PolicyRecord struct {
	WorkspaceID string    `gorm:"column:workspace_id;primaryKey" json:"workspace_id"`
	PolicyID    string    `gorm:"column:policy_id;primaryKey" json:"policy_id"`
	PolicyType  string    `gorm:"column:policy_type" json:"policy_type"`
	AgentKind   string    `gorm:"column:agent_kind" json:"agent_kind"`
	Name        string    `gorm:"column:name" json:"name"`
	Status      string    `gorm:"column:status" json:"status"`
	CreatedBy   string    `gorm:"column:created_by" json:"created_by"`
	CreatedAt   time.Time `gorm:"column:created_at" json:"created_at"`
	UpdatedAt   time.Time `gorm:"column:updated_at" json:"updated_at"`
}

func (PolicyRecord) TableName() string { return "policies" }

type PolicyRevisionRecord struct {
	WorkspaceID      string    `gorm:"column:workspace_id;primaryKey" json:"workspace_id"`
	PolicyRevisionID string    `gorm:"column:policy_revision_id;primaryKey" json:"policy_revision_id"`
	PolicyID         string    `gorm:"column:policy_id" json:"policy_id"`
	RevisionNumber   int64     `gorm:"column:revision_number" json:"revision_number"`
	SchemaVersion    int       `gorm:"column:schema_version" json:"schema_version"`
	BodyJSON         string    `gorm:"column:body_json" json:"-"`
	BodySHA256       string    `gorm:"column:body_sha256" json:"body_sha256"`
	CreatedBy        string    `gorm:"column:created_by" json:"created_by"`
	CreatedAt        time.Time `gorm:"column:created_at" json:"created_at"`
}

func (PolicyRevisionRecord) TableName() string { return "policy_revisions" }

type PolicyBindingRecord struct {
	WorkspaceID      string    `gorm:"column:workspace_id;primaryKey" json:"workspace_id"`
	BindingID        string    `gorm:"column:binding_id;primaryKey" json:"binding_id"`
	PolicyRevisionID string    `gorm:"column:policy_revision_id" json:"policy_revision_id"`
	AgentKind        string    `gorm:"column:agent_kind" json:"agent_kind"`
	ScopeType        string    `gorm:"column:scope_type" json:"scope_type"`
	SelectorJSON     string    `gorm:"column:selector_json" json:"-"`
	SelectorHash     string    `gorm:"column:selector_hash" json:"selector_hash"`
	Priority         int       `gorm:"column:priority" json:"priority"`
	Enabled          bool      `gorm:"column:enabled" json:"enabled"`
	RowVersion       int64     `gorm:"column:row_version" json:"row_version"`
	CreatedBy        string    `gorm:"column:created_by" json:"created_by"`
	UpdatedBy        string    `gorm:"column:updated_by" json:"updated_by"`
	CreatedAt        time.Time `gorm:"column:created_at" json:"created_at"`
	UpdatedAt        time.Time `gorm:"column:updated_at" json:"updated_at"`
}

func (PolicyBindingRecord) TableName() string { return "policy_bindings" }

type PolicyResolution struct {
	WorkspaceID      string          `json:"workspace_id"`
	InstanceUID      string          `json:"instance_uid"`
	BindingID        string          `json:"binding_id"`
	PolicyID         string          `json:"policy_id"`
	PolicyRevisionID string          `json:"policy_revision_id"`
	BodySHA256       string          `json:"body_sha256"`
	Body             json.RawMessage `json:"body"`
}

func policyIdentityOK(workspace, actor string) bool {
	return workspace != "" && utf8.RuneCountInString(workspace) <= 96 &&
		actor != "" && utf8.RuneCountInString(actor) <= 128
}

// CreatePolicy writes the logical identity and its first immutable Java Revision atomically.
func (s *Store) CreatePolicy(ctx context.Context, workspace, actor, name string, rate float64) (PolicyRecord, PolicyRevisionRecord, error) {
	if !policyIdentityOK(workspace, actor) || name == "" || utf8.RuneCountInString(name) > 128 {
		return PolicyRecord{}, PolicyRevisionRecord{}, policy.ErrInvalid
	}
	body, hash, err := policy.JavaSamplingBody(rate)
	if err != nil {
		return PolicyRecord{}, PolicyRevisionRecord{}, err
	}
	policyID, err := newReleaseUUID()
	if err != nil {
		return PolicyRecord{}, PolicyRevisionRecord{}, err
	}
	revisionID, err := newReleaseUUID()
	if err != nil {
		return PolicyRecord{}, PolicyRevisionRecord{}, err
	}
	now := time.Now().UTC()
	p := PolicyRecord{WorkspaceID: workspace, PolicyID: policyID, PolicyType: "head_sampling",
		AgentKind: config.JavaAgentKind, Name: name, Status: "ACTIVE", CreatedBy: actor,
		CreatedAt: now, UpdatedAt: now}
	rev := PolicyRevisionRecord{WorkspaceID: workspace, PolicyRevisionID: revisionID,
		PolicyID: policyID, RevisionNumber: 1, SchemaVersion: 1,
		BodyJSON: body, BodySHA256: hash, CreatedBy: actor, CreatedAt: now}
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&p).Error; err != nil {
			return err
		}
		return tx.Create(&rev).Error
	})
	if err != nil {
		return PolicyRecord{}, PolicyRevisionRecord{}, err
	}
	return p, rev, nil
}

// CreatePolicyRevision locks the parent Policy rather than MAX(revision)+1 without locking.
func (s *Store) CreatePolicyRevision(ctx context.Context, workspace, actor, policyID string, rate float64) (PolicyRevisionRecord, error) {
	if !policyIdentityOK(workspace, actor) || len(policyID) != 36 {
		return PolicyRevisionRecord{}, policy.ErrInvalid
	}
	body, hash, err := policy.JavaSamplingBody(rate)
	if err != nil {
		return PolicyRevisionRecord{}, err
	}
	id, err := newReleaseUUID()
	if err != nil {
		return PolicyRevisionRecord{}, err
	}
	var out PolicyRevisionRecord
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var p PolicyRecord
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("workspace_id = ? AND policy_id = ?", workspace, policyID).Take(&p).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return policy.ErrNotFound
			}
			return err
		}
		if p.Status != "ACTIVE" || p.AgentKind != config.JavaAgentKind {
			return policy.ErrInvalid
		}
		var current int64
		if err := tx.Model(&PolicyRevisionRecord{}).
			Where("workspace_id = ? AND policy_id = ?", workspace, policyID).
			Select("COALESCE(MAX(revision_number), 0)").Scan(&current).Error; err != nil {
			return err
		}
		if current == math.MaxInt64 {
			return policy.ErrConflict
		}
		out = PolicyRevisionRecord{WorkspaceID: workspace, PolicyRevisionID: id,
			PolicyID: policyID, RevisionNumber: current + 1, SchemaVersion: 1,
			BodyJSON: body, BodySHA256: hash, CreatedBy: actor, CreatedAt: time.Now().UTC()}
		return tx.Create(&out).Error
	})
	return out, err
}

type BindingInput struct {
	PolicyRevisionID string          `json:"policy_revision_id"`
	ScopeType        string          `json:"scope_type"`
	Selector         policy.Selector `json:"selector"`
	Priority         int             `json:"priority"`
	Enabled          bool            `json:"enabled"`
	RowVersion       int64           `json:"row_version,omitempty"`
}

func (s *Store) CreatePolicyBinding(ctx context.Context, workspace, actor string, in BindingInput) (PolicyBindingRecord, error) {
	if !policyIdentityOK(workspace, actor) || len(in.PolicyRevisionID) != 36 {
		return PolicyBindingRecord{}, policy.ErrInvalid
	}
	sel, hash, err := in.Selector.Canonical(in.ScopeType)
	if err != nil {
		return PolicyBindingRecord{}, err
	}
	id, err := newReleaseUUID()
	if err != nil {
		return PolicyBindingRecord{}, err
	}
	var rev PolicyRevisionRecord
	if err := s.db.WithContext(ctx).Where("workspace_id = ? AND policy_revision_id = ?",
		workspace, in.PolicyRevisionID).Take(&rev).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return PolicyBindingRecord{}, policy.ErrNotFound
		}
		return PolicyBindingRecord{}, err
	}
	now := time.Now().UTC()
	row := PolicyBindingRecord{WorkspaceID: workspace, BindingID: id,
		PolicyRevisionID: in.PolicyRevisionID, AgentKind: config.JavaAgentKind,
		ScopeType: in.ScopeType, SelectorJSON: sel, SelectorHash: hash,
		Priority: in.Priority, Enabled: true, RowVersion: 1,
		CreatedBy: actor, UpdatedBy: actor, CreatedAt: now, UpdatedAt: now}
	if err := s.db.WithContext(ctx).Create(&row).Error; err != nil {
		return PolicyBindingRecord{}, err
	}
	return row, nil
}

func (s *Store) UpdatePolicyBinding(ctx context.Context, workspace, actor, bindingID string, in BindingInput) (PolicyBindingRecord, error) {
	if !policyIdentityOK(workspace, actor) || len(bindingID) != 36 || len(in.PolicyRevisionID) != 36 || (in.RowVersion < 1 || in.RowVersion == math.MaxInt64) {
		return PolicyBindingRecord{}, policy.ErrInvalid
	}
	sel, hash, err := in.Selector.Canonical(in.ScopeType)
	if err != nil {
		return PolicyBindingRecord{}, err
	}
	var updated PolicyBindingRecord
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var rev PolicyRevisionRecord
		if err := tx.Where("workspace_id = ? AND policy_revision_id = ?", workspace, in.PolicyRevisionID).Take(&rev).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return policy.ErrNotFound
			}
			return err
		}
		now := time.Now().UTC()
		r := tx.Model(&PolicyBindingRecord{}).
			Where("workspace_id = ? AND binding_id = ? AND row_version = ?", workspace, bindingID, in.RowVersion).
			Updates(map[string]any{"policy_revision_id": in.PolicyRevisionID,
				"selector_json": sel, "selector_hash": hash, "scope_type": in.ScopeType,
				"priority": in.Priority, "enabled": in.Enabled, "row_version": in.RowVersion + 1,
				"updated_by": actor, "updated_at": now})
		if r.Error != nil {
			return r.Error
		}
		if r.RowsAffected != 1 {
			return policy.ErrConflict
		}
		return tx.Where("workspace_id = ? AND binding_id = ?", workspace, bindingID).Take(&updated).Error
	})
	return updated, err
}

// ResolveAgentPolicy is read-only. A matched Policy is never silently published as Desired.
func (s *Store) ResolveAgentPolicy(ctx context.Context, workspace, uid string) (PolicyResolution, error) {
	// 1. 可信 Workspace 范围内查找 Agent，Java 以外的 Kind 明确拒绝。
	agent, err := s.Get(ctx, workspace, uid)
	if err != nil {
		return PolicyResolution{}, err
	}
	if agent.AgentKind != config.JavaAgentKind {
		return PolicyResolution{}, ErrUnsupportedReleaseTarget
	}

	// 2. 一次读取当前有效 Binding 与不可变正文 Hash，再做确定性匹配。
	type candidate struct {
		BindingID        string `gorm:"column:binding_id"`
		PolicyID         string `gorm:"column:policy_id"`
		PolicyRevisionID string `gorm:"column:policy_revision_id"`
		ScopeType        string `gorm:"column:scope_type"`
		SelectorJSON     string `gorm:"column:selector_json"`
		Priority         int    `gorm:"column:priority"`
		BodySHA256       string `gorm:"column:body_sha256"`
	}
	var entries []candidate
	err = s.db.WithContext(ctx).Table("policy_bindings").
		Select("policy_bindings.binding_id, policy_revisions.policy_id, policy_bindings.policy_revision_id, policy_bindings.scope_type, policy_bindings.selector_json, policy_bindings.priority, policy_revisions.body_sha256").
		Joins("JOIN policy_revisions ON policy_revisions.workspace_id = policy_bindings.workspace_id AND policy_revisions.policy_revision_id = policy_bindings.policy_revision_id").
		Where("policy_bindings.workspace_id = ? AND policy_bindings.agent_kind = ? AND policy_bindings.enabled = ?", workspace, config.JavaAgentKind, true).
		Find(&entries).Error
	if err != nil {
		return PolicyResolution{}, err
	}

	target := policy.Agent{InstanceUID: uid, Environment: agent.Environment, ServiceName: agent.ServiceName}
	var selected *candidate
	conflict := false
	for i := range entries {
		item := &entries[i]
		var selector policy.Selector
		if err := json.Unmarshal([]byte(item.SelectorJSON), &selector); err != nil {
			return PolicyResolution{}, fmt.Errorf("invalid stored policy selector: %w", err)
		}
		if !selector.Matches(item.ScopeType, target) {
			continue
		}
		if selected == nil {
			selected = item
			continue
		}
		rank, top := policy.ScopeRank(item.ScopeType), policy.ScopeRank(selected.ScopeType)
		if rank > top || (rank == top && item.Priority > selected.Priority) {
			selected = item
			conflict = false
		} else if rank == top && item.Priority == selected.Priority {
			if item.BodySHA256 != selected.BodySHA256 {
				conflict = true
			} else if item.BindingID < selected.BindingID {
				selected = item
			}
		}
	}
	if selected == nil {
		return PolicyResolution{}, policy.ErrNotFound
	}
	if conflict {
		return PolicyResolution{}, policy.ErrConflict
	}

	// 3. 读取命中 Revision 的真实内容，不创建 Release 或修改 Assignment。
	var rev PolicyRevisionRecord
	err = s.db.WithContext(ctx).Where("workspace_id = ? AND policy_revision_id = ?", workspace, selected.PolicyRevisionID).Take(&rev).Error
	if err != nil {
		return PolicyResolution{}, err
	}
	return PolicyResolution{WorkspaceID: workspace, InstanceUID: uid,
		BindingID: selected.BindingID, PolicyID: selected.PolicyID,
		PolicyRevisionID: rev.PolicyRevisionID, BodySHA256: rev.BodySHA256,
		Body: json.RawMessage(rev.BodyJSON)}, nil
}
