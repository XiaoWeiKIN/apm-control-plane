package appconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testEnv(values map[string]string) func(string)string {
	return func(k string)string{return values[k]}
}

func TestLoadConfigurationAndOverrides(t *testing.T){
	path:=filepath.Join(t.TempDir(),"config.yaml")
	yaml:=`server:
  listen_address: "127.0.0.1:4321"
  read_header_timeout: "3s"
database:
  driver: "postgres"
  max_open_conns: 8
  max_idle_conns: 4
`
	if err:=os.WriteFile(path,[]byte(yaml),0600);err!=nil{t.Fatal(err)}
	cfg,err:=LoadWithEnv(testEnv(map[string]string{
		"APM_CONFIG_FILE":path,
		"APM_LISTEN_ADDR":"127.0.0.1:5500",
		"APM_DB_DRIVER":"mysql",
		"APM_DATABASE_DSN":"dummy",
		"APM_AGENT_CREDENTIALS_JSON":`[{"id":"a1","tenant_id":"t1","token":"agent-token-12345678"}]`,
		"APM_ADMIN_CREDENTIALS_JSON":`[{"id":"admin1","tenant_id":"t1","token":"admin-token-12345678"}]`,
	}))
	if err!=nil{t.Fatal(err)}
	if cfg.ListenAddress!="127.0.0.1:5500" || cfg.Database.Driver!="mysql" || cfg.Database.MaxOpen!=8 || cfg.ReadHeaderTimeout!=3*time.Second{
		t.Fatalf("unexpected config: %#v",cfg)
	}
}

func TestRejectUnknownConfigKey(t *testing.T){
	path:=filepath.Join(t.TempDir(),"config.yaml")
	if err:=os.WriteFile(path,[]byte("server:\n  unexpected: yes\n"),0600);err!=nil{t.Fatal(err)}
	_,err:=LoadWithEnv(testEnv(map[string]string{"APM_CONFIG_FILE":path}))
	if err==nil || !strings.Contains(err.Error(),"unsupported config key"){t.Fatalf("unknown key not rejected: %v",err)}
}

func TestRejectInvalidDriverAndDuplicateCredentials(t *testing.T){
	base:=map[string]string{
		"APM_DATABASE_DSN":"test",
		"APM_OPAMP_TOKEN":"long-agent-secret-123",
		"APM_ADMIN_TOKEN":"long-admin-secret-123",
	}
	_,err:=LoadWithEnv(testEnv(map[string]string{"APM_DATABASE_DSN":"test","APM_DB_DRIVER":"sqlite","APM_OPAMP_TOKEN":"long-agent-secret-123","APM_ADMIN_TOKEN":"long-admin-secret-123"}))
	if err==nil || !strings.Contains(err.Error(),"unsupported database driver"){t.Fatalf("invalid driver: %v",err)}
	base["APM_ADMIN_TOKEN"]=base["APM_OPAMP_TOKEN"]
	_,err=LoadWithEnv(testEnv(base))
	if err==nil || !strings.Contains(err.Error(),"unique"){t.Fatalf("duplicate credential tokens: %v",err)}
}

func TestLegacyCredentialsRemainScopedToDefault(t *testing.T){
	cfg,err:=LoadWithEnv(testEnv(map[string]string{
		"APM_DATABASE_DSN":"test",
		"APM_OPAMP_TOKEN":"long-agent-secret-123",
		"APM_ADMIN_TOKEN":"long-admin-secret-123",
	}))
	if err!=nil {t.Fatal(err)}
	if cfg.AgentCredentials[0].TenantID!="default" || cfg.AdminCredentials[0].TenantID!="default" {t.Fatalf("legacy credentials not scoped: %#v",cfg)}
}
