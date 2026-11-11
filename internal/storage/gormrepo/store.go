package gormrepo

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"time"
	"unicode/utf8"

	"github.com/XiaoWeiKIN/apm-control-plane/internal/appconfig"
	"github.com/XiaoWeiKIN/apm-control-plane/internal/auth"
	"github.com/datafoxlabs/storekit/sqlstore"
	storemysql "github.com/datafoxlabs/storekit/sqlstore/mysql"
	storepostgres "github.com/datafoxlabs/storekit/sqlstore/postgres"
	"go.opentelemetry.io/otel/trace/noop"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
)

var (
	ErrNotFound           = errors.New("agent not found")
	ErrOwnership          = errors.New("agent instance owned by a different credential")
	ErrInvalidObservation = errors.New("invalid agent observation")
)

type AgentRecord struct {
	OrgID               string    `gorm:"column:org_id;primaryKey"`
	InstanceUID         string    `gorm:"column:instance_uid;primaryKey"`
	CredentialID        string    `gorm:"column:credential_id"`
	AgentKind           string    `gorm:"column:agent_kind"`
	Distribution        string    `gorm:"column:distribution"`
	DistributionVersion string    `gorm:"column:distribution_version"`
	ServiceName         string    `gorm:"column:service_name"`
	ServiceNamespace    string    `gorm:"column:service_namespace"`
	Environment         string    `gorm:"column:environment"`
	ServiceVersion      string    `gorm:"column:service_version"`
	LastSeen            time.Time `gorm:"column:last_seen"`
	LastTransport       string    `gorm:"column:last_transport"`
	Healthy             *bool     `gorm:"column:healthy"`
	Capabilities        int64     `gorm:"column:capabilities"`
	ReportedHash        string    `gorm:"column:reported_hash"`
	ConfigStatus        string    `gorm:"column:config_status"`
	ConfigError         string    `gorm:"column:config_error"`
}

func (AgentRecord) TableName() string { return "agent_instances" }

type Description struct {
	Kind, Distribution, DistributionVersion         string
	Service, Namespace, Environment, ServiceVersion string
}

type ConfigStatus struct {
	Hash, Status, Error string
}

type Observation struct {
	SeenAt       time.Time
	Transport    string
	Capabilities uint64
	Description  *Description
	Healthy      *bool
	Config       *ConfigStatus
}

type Store struct{ db *gorm.DB }

// Open does not create tables. Schema changes are owned by datafox-schema.
func Open(ctx context.Context, cfg appconfig.Database) (*Store, error) {
	// 1. 根据驱动配置选择 SQL Dialector，未知类型提前失败。
	var driver sqlstore.Driver
	switch cfg.Driver {
	case "postgres":
		driver = storepostgres.Open
	case "mysql":
		driver = storemysql.Open
	default:
		return nil, fmt.Errorf("unsupported database driver %q", cfg.Driver)
	}
	dbConfig, err := sqlstore.NewDatabaseConfig(
		sqlstore.WithDBType(cfg.Driver),
		sqlstore.WithConnectionString(cfg.DSN),
	)
	if err != nil {
		return nil, fmt.Errorf("database configuration: %w", err)
	}
	dbConfig.LogLevel = logger.Silent
	managed, err := sqlstore.Open(
		dbConfig, driver, slog.New(slog.NewTextHandler(io.Discard, nil)), noop.NewTracerProvider(),
	)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	db := managed.GetDB()
	sqlDB, err := db.DB()
	if err != nil {
		_ = managed.Close()
		return nil, err
	}
	// 2. 配置连接池，并确认数据库能在启动期限内响应。
	sqlDB.SetMaxOpenConns(cfg.MaxOpen)
	sqlDB.SetMaxIdleConns(cfg.MaxIdle)
	sqlDB.SetConnMaxLifetime(cfg.MaxLifetime)
	if err := sqlDB.PingContext(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	// 3. 验证统一 Schema 迁移历史，不能把现存表推断为已完成迁移。
	var applied int64
	if err := db.WithContext(ctx).Raw(
		"SELECT COUNT(*) FROM migration_log WHERE migration_id = ? AND success = ?",
		"controlplane_init_schema", true,
	).Scan(&applied).Error; err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("inspect DataFox controlplane schema: %w", err)
	}
	if applied != 1 {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("DataFox controlplane schema is not initialized: run datafox-schema migrate up --domain=controlplane")
	}
	// 校验必需的表与 Agent Inventory 字段；缺失时明确启动失败。
	if !db.Migrator().HasTable(&AgentRecord{}) {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("agent_instances schema missing: run DataFox controlplane schema migration for %s", cfg.Driver)
	}
	columns, err := db.Migrator().ColumnTypes(&AgentRecord{})
	if err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("inspect agent_instances schema: %w", err)
	}
	present := make(map[string]bool, len(columns))
	for _, column := range columns {
		present[column.Name()] = true
	}
	for _, name := range []string{
		"org_id", "instance_uid", "credential_id", "agent_kind",
		"distribution", "distribution_version", "service_name",
		"service_namespace", "environment", "service_version",
		"last_seen", "last_transport", "healthy", "capabilities",
		"reported_hash", "config_status", "config_error",
	} {
		if !present[name] {
			_ = sqlDB.Close()
			return nil, fmt.Errorf("agent_instances column %q missing: initialize the current DataFox controlplane schema", name)
		}
	}
	// P2.2 需要完整事务表，缺少 Migration 00003 时不得启动可接受发布的 API。
	for _, table := range []struct {
		name  string
		model any
	}{
		{"config_revisions", &ConfigRevisionRecord{}},
		{"config_releases", &ConfigReleaseRecord{}},
		{"release_targets", &ReleaseTargetRecord{}},
		{"agent_assignments", &AgentAssignmentRecord{}},
		{"config_events", &ConfigEventRecord{}},
		{"policies", &PolicyRecord{}},
		{"policy_revisions", &PolicyRevisionRecord{}},
		{"policy_bindings", &PolicyBindingRecord{}},
	} {
		if !db.Migrator().HasTable(table.model) {
			_ = sqlDB.Close()
			return nil, fmt.Errorf("%s schema missing: run DataFox controlplane schema migration for %s", table.name, cfg.Driver)
		}
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error {
	db, err := s.db.DB()
	if err != nil {
		return err
	}
	return db.Close()
}

func (s *Store) Observe(ctx context.Context, p auth.Principal, uid string, obs Observation) (AgentRecord, error) {
	// 1. 验证可信身份与 Observation 字段范围，拒绝无效状态写入。
	if p.OrgID == "" || p.CredentialID == "" || len(uid) != 32 || obs.SeenAt.IsZero() {
		return AgentRecord{}, fmt.Errorf("%w: invalid agent identity or observation", ErrInvalidObservation)
	}
	if obs.Capabilities > math.MaxInt64 {
		return AgentRecord{}, fmt.Errorf("%w: unsupported capabilities bitmask", ErrInvalidObservation)
	}
	if obs.Transport != "http" && obs.Transport != "websocket" {
		return AgentRecord{}, fmt.Errorf("%w: unsupported transport", ErrInvalidObservation)
	}
	if obs.Description != nil {
		for _, field := range []struct {
			name  string
			value string
			max   int
		}{
			{"agent_kind", obs.Description.Kind, 64},
			{"distribution", obs.Description.Distribution, 128},
			{"distribution_version", obs.Description.DistributionVersion, 64},
			{"service_name", obs.Description.Service, 255},
			{"service_namespace", obs.Description.Namespace, 255},
			{"environment", obs.Description.Environment, 128},
			{"service_version", obs.Description.ServiceVersion, 64},
		} {
			if utf8.RuneCountInString(field.value) > field.max {
				return AgentRecord{}, fmt.Errorf("%w: %s exceeds %d characters", ErrInvalidObservation, field.name, field.max)
			}
		}
	}
	if obs.Config != nil {
		if len(obs.Config.Hash) > 64 || len(obs.Config.Status) > 32 || len(obs.Config.Error) > 8192 {
			return AgentRecord{}, fmt.Errorf("%w: remote config status exceeds storage limits", ErrInvalidObservation)
		}
	}
	// 2. 构造首次注册记录，仅复制本次 Agent 实际上报的元数据。
	seenAt := obs.SeenAt.UTC()
	seed := AgentRecord{
		OrgID: p.OrgID, InstanceUID: uid, CredentialID: p.CredentialID,
		AgentKind: "unknown", LastSeen: seenAt, LastTransport: obs.Transport,
	}
	if obs.Description != nil {
		seed.AgentKind = obs.Description.Kind
		seed.Distribution = obs.Description.Distribution
		seed.DistributionVersion = obs.Description.DistributionVersion
		seed.ServiceName = obs.Description.Service
		seed.ServiceNamespace = obs.Description.Namespace
		seed.Environment = obs.Description.Environment
		seed.ServiceVersion = obs.Description.ServiceVersion
	}
	if obs.Healthy != nil {
		value := *obs.Healthy
		seed.Healthy = &value
	}
	seed.Capabilities = int64(obs.Capabilities)
	if obs.Config != nil {
		seed.ReportedHash, seed.ConfigStatus, seed.ConfigError = obs.Config.Hash, obs.Config.Status, obs.Config.Error
	}
	// 3. 在事务内绑定 UID 所有权并更新最新状态，失败时整体回滚。
	var result AgentRecord
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// Atomic insert-if-absent preserves the original credential binding,
		// including under concurrent first registration from different tokens.
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&seed).Error; err != nil {
			return err
		}
		if err := tx.Where("org_id = ? AND instance_uid = ?", p.OrgID, uid).Take(&result).Error; err != nil {
			return err
		}
		if result.CredentialID != p.CredentialID {
			return ErrOwnership
		}
		fields := map[string]any{
			"last_seen":      seenAt,
			"last_transport": obs.Transport,
			"capabilities":   int64(obs.Capabilities),
		}
		if obs.Description != nil {
			fields["agent_kind"] = obs.Description.Kind
			fields["distribution"] = obs.Description.Distribution
			fields["distribution_version"] = obs.Description.DistributionVersion
			fields["service_name"] = obs.Description.Service
			fields["service_namespace"] = obs.Description.Namespace
			fields["environment"] = obs.Description.Environment
			fields["service_version"] = obs.Description.ServiceVersion
		}
		if obs.Healthy != nil {
			fields["healthy"] = *obs.Healthy
		}
		if obs.Config != nil {
			fields["reported_hash"] = obs.Config.Hash
			fields["config_status"] = obs.Config.Status
			fields["config_error"] = obs.Config.Error
		}
		if err := tx.Model(&AgentRecord{}).
			Where("org_id = ? AND instance_uid = ? AND credential_id = ?", p.OrgID, uid, p.CredentialID).
			Updates(fields).Error; err != nil {
			return err
		}
		return tx.Where("org_id = ? AND instance_uid = ?", p.OrgID, uid).Take(&result).Error
	})
	return result, err
}

func (s *Store) Get(ctx context.Context, org, uid string) (AgentRecord, error) {
	var a AgentRecord
	err := s.db.WithContext(ctx).Where("org_id = ? AND instance_uid = ?", org, uid).Take(&a).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return a, ErrNotFound
	}
	return a, err
}

func (s *Store) List(ctx context.Context, org string) ([]AgentRecord, error) {
	var records []AgentRecord
	err := s.db.WithContext(ctx).Where("org_id = ?", org).
		Order("instance_uid ASC").Find(&records).Error
	if records == nil {
		records = []AgentRecord{}
	}
	return records, err
}
