package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const legacyConfig = `# 用户注释必须保留
listen = ["127.0.0.1:53"]
mode = "full"

[upstreams]
doh = ["https://dns.alidns.com/dns-query"]
timeout = "2s"

[log]
level = "debug"
`

func writeTempConfig(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestEnsureSchemaStampsLegacy(t *testing.T) {
	path := writeTempConfig(t, legacyConfig)

	migrated, err := EnsureSchema(path)
	if err != nil {
		t.Fatal(err)
	}
	if !migrated {
		t.Fatal("expected migration for legacy config")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	if !strings.Contains(s, "schema_version = 1") {
		t.Fatalf("schema stamp missing:\n%s", s)
	}
	// User parameters and comments preserved.
	for _, want := range []string{"# 用户注释必须保留", `mode = "full"`, `timeout = "2s"`, `level = "debug"`} {
		if !strings.Contains(s, want) {
			t.Fatalf("lost content %q after migration:\n%s", want, s)
		}
	}
	// Idempotent: second run is a no-op.
	migrated, err = EnsureSchema(path)
	if err != nil || migrated {
		t.Fatalf("second run should be no-op: migrated=%v err=%v", migrated, err)
	}
}

func TestEnsureSchemaCurrentIsNoop(t *testing.T) {
	path := writeTempConfig(t, "schema_version = 1\nlisten = [\"127.0.0.1:53\"]\n")
	migrated, err := EnsureSchema(path)
	if err != nil || migrated {
		t.Fatalf("current schema should be no-op: migrated=%v err=%v", migrated, err)
	}
}

func TestEnsureSchemaNewerRejected(t *testing.T) {
	path := writeTempConfig(t, "schema_version = 99\n")
	if _, err := EnsureSchema(path); err == nil {
		t.Fatal("expected error for newer schema")
	}
}

func TestDefaultCarriesSchemaVersion(t *testing.T) {
	cfg := Default()
	if cfg.SchemaVersion != CurrentSchemaVersion {
		t.Fatalf("Default().SchemaVersion = %d, want %d", cfg.SchemaVersion, CurrentSchemaVersion)
	}
}

func TestLoadLegacyThenEnsure(t *testing.T) {
	path := writeTempConfig(t, legacyConfig)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Mode != ModeFull {
		t.Fatalf("legacy params not decoded: mode=%s", cfg.Mode)
	}
	if _, err := EnsureSchema(path); err != nil {
		t.Fatal(err)
	}
	cfg2, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg2.SchemaVersion != CurrentSchemaVersion {
		t.Fatalf("reloaded SchemaVersion = %d", cfg2.SchemaVersion)
	}
}
