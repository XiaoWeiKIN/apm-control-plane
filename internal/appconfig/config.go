package appconfig

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"
)

// Credential is loaded from secret environment variables, never from public YAML.
type Credential struct {
	ID       string `json:"id"`
	TenantID string `json:"tenant_id"`
	Token    string `json:"token"`
}

type Database struct {
	Driver      string
	DSN         string
	MaxOpen     int
	MaxIdle     int
	MaxLifetime time.Duration
}

type Config struct {
	ListenAddress     string
	ReadHeaderTimeout time.Duration
	HTTPPollingTTL    time.Duration
	Database          Database
	AgentCredentials  []Credential
	AdminCredentials  []Credential
}

// Load reads optional YAML, overlays an explicit environment variable allowlist,
// then validates a complete immutable startup configuration.
func Load() (Config, error) { return LoadWithEnv(os.Getenv) }

func LoadWithEnv(env func(string) string) (Config, error) {
	cfg := Config{
		ListenAddress:     "127.0.0.1:4320",
		ReadHeaderTimeout: 5 * time.Second,
		HTTPPollingTTL:    30 * time.Second,
		Database: Database{
			Driver: "postgres", MaxOpen: 10, MaxIdle: 5, MaxLifetime: 30 * time.Minute,
		},
	}
	if path := env("APM_CONFIG_FILE"); path != "" {
		k := koanf.New(".")
		if err := k.Load(file.Provider(path), yaml.Parser()); err != nil {
			return cfg, fmt.Errorf("load config file: %w", err)
		}
		allowed := map[string]bool{
			"server.listen_address": true, "server.read_header_timeout": true,
			"server.http_polling_ttl": true, "database.driver": true,
			"database.max_open_conns": true, "database.max_idle_conns": true,
			"database.conn_max_lifetime": true,
		}
		for _, key := range k.Keys() {
			if !allowed[key] {
				return cfg, fmt.Errorf("unsupported config key %q", key)
			}
		}
		if k.Exists("server.listen_address") {
			cfg.ListenAddress = k.String("server.listen_address")
		}
		if k.Exists("server.read_header_timeout") {
			d, err := time.ParseDuration(k.String("server.read_header_timeout"))
			if err != nil {
				return cfg, fmt.Errorf("server.read_header_timeout: %w", err)
			}
			cfg.ReadHeaderTimeout = d
		}
		if k.Exists("server.http_polling_ttl") {
			d, err := time.ParseDuration(k.String("server.http_polling_ttl"))
			if err != nil {
				return cfg, fmt.Errorf("server.http_polling_ttl: %w", err)
			}
			cfg.HTTPPollingTTL = d
		}
		if k.Exists("database.driver") {
			cfg.Database.Driver = k.String("database.driver")
		}
		if k.Exists("database.max_open_conns") {
			cfg.Database.MaxOpen = k.Int("database.max_open_conns")
		}
		if k.Exists("database.max_idle_conns") {
			cfg.Database.MaxIdle = k.Int("database.max_idle_conns")
		}
		if k.Exists("database.conn_max_lifetime") {
			d, err := time.ParseDuration(k.String("database.conn_max_lifetime"))
			if err != nil {
				return cfg, fmt.Errorf("database.conn_max_lifetime: %w", err)
			}
			cfg.Database.MaxLifetime = d
		}
	}
	if val := env("APM_LISTEN_ADDR"); val != "" {
		cfg.ListenAddress = val
	}
	if val := env("APM_DB_DRIVER"); val != "" {
		cfg.Database.Driver = val
	}
	cfg.Database.DSN = env("APM_DATABASE_DSN")
	if val := env("APM_DB_MAX_OPEN_CONNS"); val != "" {
		n, err := strconv.Atoi(val)
		if err != nil {
			return cfg, fmt.Errorf("APM_DB_MAX_OPEN_CONNS: %w", err)
		}
		cfg.Database.MaxOpen = n
	}
	if val := env("APM_HTTP_POLL_TTL"); val != "" {
		d, err := time.ParseDuration(val)
		if err != nil {
			return cfg, fmt.Errorf("APM_HTTP_POLL_TTL: %w", err)
		}
		cfg.HTTPPollingTTL = d
	}
	if val := env("APM_AGENT_CREDENTIALS_JSON"); val != "" {
		if err := json.Unmarshal([]byte(val), &cfg.AgentCredentials); err != nil {
			return cfg, fmt.Errorf("APM_AGENT_CREDENTIALS_JSON: %w", err)
		}
	} else if legacy := env("APM_OPAMP_TOKEN"); legacy != "" {
		cfg.AgentCredentials = []Credential{{ID: "legacy-agent", TenantID: "default", Token: legacy}}
	}
	if val := env("APM_ADMIN_CREDENTIALS_JSON"); val != "" {
		if err := json.Unmarshal([]byte(val), &cfg.AdminCredentials); err != nil {
			return cfg, fmt.Errorf("APM_ADMIN_CREDENTIALS_JSON: %w", err)
		}
	} else if legacy := env("APM_ADMIN_TOKEN"); legacy != "" {
		cfg.AdminCredentials = []Credential{{ID: "legacy-admin", TenantID: "default", Token: legacy}}
	}
	return cfg, cfg.Validate()
}

func (c Config) Validate() error {
	if c.ListenAddress == "" || strings.Contains(c.ListenAddress, " ") {
		return fmt.Errorf("invalid server listen address")
	}
	if c.ReadHeaderTimeout <= 0 || c.HTTPPollingTTL <= 0 {
		return fmt.Errorf("timeouts must be positive")
	}
	if c.Database.Driver != "postgres" && c.Database.Driver != "mysql" {
		return fmt.Errorf("unsupported database driver %q", c.Database.Driver)
	}
	if c.Database.DSN == "" {
		return fmt.Errorf("APM_DATABASE_DSN is required")
	}
	if c.Database.MaxOpen < 1 || c.Database.MaxIdle < 0 || c.Database.MaxIdle > c.Database.MaxOpen || c.Database.MaxLifetime <= 0 {
		return fmt.Errorf("invalid database pool configuration")
	}
	if len(c.AgentCredentials) == 0 || len(c.AdminCredentials) == 0 {
		return fmt.Errorf("agent and admin credentials are required")
	}
	seenID, seenToken := map[string]bool{}, map[string]bool{}
	for _, credentials := range [][]Credential{c.AgentCredentials, c.AdminCredentials} {
		for _, cred := range credentials {
			if cred.ID == "" || cred.TenantID == "" || len(cred.Token) < 16 {
				return fmt.Errorf("credentials require nonempty id/tenant and token length >= 16")
			}
			if seenID[cred.ID] || seenToken[cred.Token] {
				return fmt.Errorf("credential ids and tokens must be unique across roles")
			}
			seenID[cred.ID], seenToken[cred.Token] = true, true
		}
	}
	return nil
}
