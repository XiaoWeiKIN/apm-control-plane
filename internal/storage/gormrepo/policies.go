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
	OrgID string    `gorm:"column:org_id;primaryKey" json:"org_id"`
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
	OrgID      string    `gorm:"column:org_id;primaryKey" json:"org_id"`
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
	OrgID      string    `gorm:"column:org_id;primaryKey" json:"org_id"`
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
	OrgID      string          `json:"org_id"`
	InstanceUID      string          `json:"instance_uid"`
	BindingID        string          `json:"binding_id"`
	PolicyID         string          `json:"policy_id"`
	PolicyRevisionID string          `json:"policy_revision_id"`
	BodySHA256       string          `json:"body_sha256"`
	Body             json.RawMessage `json:"body"`
}

func policyIdentityOK(org, actor string) bool {
	return org != "" && utf8.RuneCountInString(org) <= 96 &&
		actor != "" && utf8.RuneCountInString(actor) <= 128
}

// CreatePolicy writes the logical identity and its first immutable Java Revision atomically.
func (s *Store) CreatePolicy(ctx context.Context, org, actor, name string, rate float64) (PolicyRecord, PolicyRevisionRecord, error) {
	if !policyIdentityOK(org, actor) || name == "" || utf8.RuneCountInString(name) > 128 {
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
	p := PolicyRecord{OrgID: org, PolicyID: policyID, PolicyType: "head_sampling",
		AgentKind: config.JavaAgentKind, Name: name, Status: "ACTIVE", CreatedBy: actor,
		CreatedAt: now, UpdatedAt: now}
	rev := PolicyRevisionRecord{OrgID: org, PolicyRevisionID: revisionID,
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
func (s *Store) CreatePolicyRevision(ctx context.Context, org, actor, policyID string, rate float64) (PolicyRevisionRecord, error) {
	if !policyIdentityOK(org, actor) || len(policyID) != 36 {
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
			Where("org_id = ? AND policy_id = ?", org, policyID).Take(&p).Error; err != nil {
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
			Where("org_id = ? AND policy_id = ?", org, policyID).
			Select("COALESCE(MAX(revision_number), 0)").Scan(&current).Error; err != nil {
			return err
		}
		if current == math.MaxInt64 {
			return policy.ErrConflict
		}
		out = PolicyRevisionRecord{OrgID: org, PolicyRevisionID: id,
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

func (s *Store) CreatePolicyBinding(ctx context.Context, org, actor string, in BindingInput) (PolicyBindingRecord, error) {
	if !policyIdentityOK(org, actor) || len(in.PolicyRevisionID) != 36 ||
		in.Priority < math.MinInt32 || in.Priority > math.MaxInt32 {
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
	if err := s.db.WithContext(ctx).Where("org_id = ? AND policy_revision_id = ?",
		org, in.PolicyRevisionID).Take(&rev).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return PolicyBindingRecord{}, policy.ErrNotFound
		}
		return PolicyBindingRecord{}, err
	}
	now := time.Now().UTC()
	row := PolicyBindingRecord{OrgID: org, BindingID: id,
		PolicyRevisionID: in.PolicyRevisionID, AgentKind: config.JavaAgentKind,
		ScopeType: in.ScopeType, SelectorJSON: sel, SelectorHash: hash,
		Priority: in.Priority, Enabled: true, RowVersion: 1,
		CreatedBy: actor, UpdatedBy: actor, CreatedAt: now, UpdatedAt: now}
	if err := s.db.WithContext(ctx).Create(&row).Error; err != nil {
		return PolicyBindingRecord{}, err
	}
	return row, nil
}

func (s *Store) UpdatePolicyBinding(ctx context.Context, org, actor, bindingID string, in BindingInput) (PolicyBindingRecord, error) {
	if !policyIdentityOK(org, actor) || len(bindingID) != 36 || len(in.PolicyRevisionID) != 36 ||
		in.Priority < math.MinInt32 || in.Priority > math.MaxInt32 || in.RowVersion < 1 || in.RowVersion == math.MaxInt64 {
		return PolicyBindingRecord{}, policy.ErrInvalid
	}
	sel, hash, err := in.Selector.Canonical(in.ScopeType)
	if err != nil {
		return PolicyBindingRecord{}, err
	}
	var updated PolicyBindingRecord
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var rev PolicyRevisionRecord
		if err := tx.Where("org_id = ? AND policy_revision_id = ?", org, in.PolicyRevisionID).Take(&rev).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return policy.ErrNotFound
			}
			return err
		}
		now := time.Now().UTC()
		r := tx.Model(&PolicyBindingRecord{}).
			Where("org_id = ? AND binding_id = ? AND row_version = ?", org, bindingID, in.RowVersion).
			Updates(map[string]any{"policy_revision_id": in.PolicyRevisionID,
				"selector_json": sel, "selector_hash": hash, "scope_type": in.ScopeType,
				"priority": in.Priority, "enabled": in.Enabled, "row_version": in.RowVersion + 1,
				"updated_by": actor, "updated_at": now})
		if r.Error != nil {
			return r.Error
		}
		if r.RowsAffected != 1 {
			var count int64
			if err := tx.Model(&PolicyBindingRecord{}).
				Where("org_id = ? AND binding_id = ?", org, bindingID).Count(&count).Error; err != nil {
				return err
			}
			if count == 0 {
				return policy.ErrNotFound
			}
			return policy.ErrConflict
		}
		return tx.Where("org_id = ? AND binding_id = ?", org, bindingID).Take(&updated).Error
	})
	return updated, err
}

type policyBindingCandidate struct {
	BindingID        string `gorm:"column:binding_id"`
	PolicyID         string `gorm:"column:policy_id"`
	PolicyRevisionID string `gorm:"column:policy_revision_id"`
	ScopeType        string `gorm:"column:scope_type"`
	SelectorJSON     string `gorm:"column:selector_json"`
	Priority         int    `gorm:"column:priority"`
	BodySHA256       string `gorm:"column:body_sha256"`
}

// policyBindingSnapshot reads enabled bindings once; publishing locks existing rows.
func (s *Store) policyBindingSnapshot(ctx context.Context, org string, lockBindings bool) ([]policyBindingCandidate, error) {
	var entries []policyBindingCandidate
	query := s.db.WithContext(ctx).Table("policy_bindings").
		Select("policy_bindings.binding_id, policy_revisions.policy_id, policy_bindings.policy_revision_id, policy_bindings.scope_type, policy_bindings.selector_json, policy_bindings.priority, policy_revisions.body_sha256").
		Joins("JOIN policy_revisions ON policy_revisions.org_id = policy_bindings.org_id AND policy_revisions.policy_revision_id = policy_bindings.policy_revision_id").
		Where("policy_bindings.org_id = ? AND policy_bindings.agent_kind = ? AND policy_bindings.enabled = ?", org, config.JavaAgentKind, true)
	if lockBindings {
		// 使用当前读并阻止已有 Binding 被并发修改。
		query = query.Clauses(clause.Locking{Strength: "SHARE"})
	}
	err := query.Find(&entries).Error
	return entries, err
}

// selectPolicyBinding resolves one Agent against a stable Binding snapshot.
func selectPolicyBinding(agent AgentRecord, entries []policyBindingCandidate) (*policyBindingCandidate, error) {
	target := policy.Agent{InstanceUID: agent.InstanceUID, Environment: agent.Environment, ServiceName: agent.ServiceName}
	var selected *policyBindingCandidate
	conflict := false
	for i := range entries {
		item := &entries[i]
		var selector policy.Selector
		if err := json.Unmarshal([]byte(item.SelectorJSON), &selector); err != nil {
			return nil, fmt.Errorf("invalid stored policy selector: %w", err)
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
		return nil, policy.ErrNotFound
	}
	if conflict {
		return nil, policy.ErrConflict
	}
	return selected, nil
}

// ResolveAgentPolicy is read-only. A matched Policy is never silently published as Desired.
func (s *Store) ResolveAgentPolicy(ctx context.Context, org, uid string) (PolicyResolution, error) {
	agent, err := s.Get(ctx, org, uid)
	if err != nil {
		return PolicyResolution{}, err
	}
	return s.resolveAgentPolicy(ctx, org, agent, false)
}

// Publishing uses the already locked Agent and locks existing Bindings; preview is read-only.
func (s *Store) resolveAgentPolicy(ctx context.Context, org string, agent AgentRecord, lockBindings bool) (PolicyResolution, error) {
	// 1. 校验 Java 类型，不对不支持的 Agent 加载 Binding。
	if agent.AgentKind != config.JavaAgentKind {
		return PolicyResolution{}, ErrUnsupportedReleaseTarget
	}
	// 2. 读取当前有效 Binding 并按 Scope/Priority 求解。
	entries, err := s.policyBindingSnapshot(ctx, org, lockBindings)
	if err != nil {
		return PolicyResolution{}, err
	}
	selected, err := selectPolicyBinding(agent, entries)
	if err != nil {
		return PolicyResolution{}, err
	}
	// 3. 读取命中 Revision 的真实内容，不创建 Release 或修改 Assignment。
	var rev PolicyRevisionRecord
	revisionQuery := s.db.WithContext(ctx).
		Where("org_id = ? AND policy_revision_id = ?", org, selected.PolicyRevisionID)
	if lockBindings {
		revisionQuery = revisionQuery.Clauses(clause.Locking{Strength: "SHARE"})
	}
	err = revisionQuery.Take(&rev).Error
	if err != nil {
		return PolicyResolution{}, err
	}
	return PolicyResolution{OrgID: org, InstanceUID: agent.InstanceUID,
		BindingID: selected.BindingID, PolicyID: selected.PolicyID,
		PolicyRevisionID: rev.PolicyRevisionID, BodySHA256: rev.BodySHA256,
		Body: json.RawMessage(rev.BodyJSON)}, nil
}
