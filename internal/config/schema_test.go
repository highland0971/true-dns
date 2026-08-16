package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
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

func TestEnsureSchemaCommentMentionMigrates(t *testing.T) {
	// A comment mentioning schema_version must not block migration.
	path := writeTempConfig(t, "# 不要手动设置 schema_version, 由程序管理\nlisten = [\"127.0.0.1:53\"]\n")
	migrated, err := EnsureSchema(path)
	if err != nil || !migrated {
		t.Fatalf("comment-mention case: migrated=%v err=%v", migrated, err)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "schema_version = 1") {
		t.Fatal("stamp missing")
	}
}

func TestEnsureSchemaExplicitZeroRewritten(t *testing.T) {
	path := writeTempConfig(t, "schema_version = 0\nlisten = [\"127.0.0.1:53\"]\n")
	migrated, err := EnsureSchema(path)
	if err != nil || !migrated {
		t.Fatalf("explicit zero: migrated=%v err=%v", migrated, err)
	}
	data, _ := os.ReadFile(path)
	sd := string(data)
	if !strings.Contains(sd, "schema_version = 1") || strings.Contains(sd, "schema_version = 0") {
		t.Fatalf("zero not rewritten:\n%s", sd)
	}
	if !strings.Contains(sd, "listen") {
		t.Fatal("parameters lost")
	}
}

func TestEnsureSchemaNewerSentinel(t *testing.T) {
	path := writeTempConfig(t, "schema_version = 99\n")
	_, err := EnsureSchema(path)
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, ErrSchemaNewer) {
		t.Fatalf("error = %v, want ErrSchemaNewer", err)
	}
}

func TestConcurrentEnsureSchemaIdempotent(t *testing.T) {
	path := writeTempConfig(t, legacyConfig)
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := EnsureSchema(path)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent migration error: %v", err)
		}
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("config corrupted by concurrent migration: %v", err)
	}
	if cfg.SchemaVersion != 1 || cfg.Mode != ModeFull {
		t.Fatalf("post-migration config = %+v", cfg)
	}
}

func TestEnsureSchemaTrailingCommentForms(t *testing.T) {
	// Current version with a trailing comment: detected as current, no
	// rewrite needed.
	path := writeTempConfig(t, "schema_version = 1 # note\nlisten = [\"127.0.0.1:53\"]\n")
	migrated, err := EnsureSchema(path)
	if err != nil || migrated {
		t.Fatalf("v1 trailing comment should be no-op: migrated=%v err=%v", migrated, err)
	}
	// Version 0 with a trailing comment: rewritten in place, no duplicate
	// key, parameters preserved, file still loadable.
	path = writeTempConfig(t, "schema_version = 0 # note\nlisten = [\"127.0.0.1:53\"]\n")
	migrated, err = EnsureSchema(path)
	if err != nil || !migrated {
		t.Fatalf("v0 trailing comment: migrated=%v err=%v", migrated, err)
	}
	data, _ := os.ReadFile(path)
	sd := string(data)
	if strings.Count(sd, "schema_version") != 1 || !strings.Contains(sd, "schema_version = 1") {
		t.Fatalf("duplicate or missing key:\n%s", sd)
	}
	if _, err := Load(path); err != nil {
		t.Fatalf("config not loadable after migration: %v", err)
	}
	if !strings.Contains(sd, "listen") {
		t.Fatal("parameters lost")
	}
}

func TestAtomicWriteRetryThenSucceed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	orig := renameFile
	defer func() { renameFile = orig }()
	fails := 3
	renameFile = func(from, to string) error {
		if fails > 0 {
			fails--
			return syscall.EACCES
		}
		return os.Rename(from, to)
	}
	if err := atomicWrite(path, []byte("new")); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "new" {
		t.Fatalf("content = %q", data)
	}
}

func TestAtomicWriteFailSafeOnPersistentLock(t *testing.T) {
	// Non-Windows: a persistently locked destination must NOT be rewritten
	// in place; the original file stays intact and an error is reported.
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	orig := renameFile
	defer func() { renameFile = orig }()
	renameFile = func(from, to string) error { return syscall.EACCES } // always locked
	if err := atomicWrite(path, []byte("new")); err == nil {
		t.Fatal("expected error for persistent lock (fail-safe)")
	}
	data, _ := os.ReadFile(path)
	if string(data) != "old" {
		t.Fatalf("original content modified: %q", data)
	}
}

func TestEnsureSchemaMigrationSelfHeals(t *testing.T) {
	// Simulates the Windows editor-lock scenario: the first rename hits a
	// locked destination, a retry within the same run succeeds.
	path := writeTempConfig(t, legacyConfig)
	orig := renameFile
	defer func() { renameFile = orig }()
	locked := true
	renameFile = func(from, to string) error {
		if locked {
			locked = false
			return syscall.EACCES
		}
		return os.Rename(from, to)
	}
	if _, err := EnsureSchema(path); err != nil {
		t.Fatal(err) // falls back to in-place write; content must be intact
	}
	if _, err := Load(path); err != nil {
		t.Fatalf("config corrupted: %v", err)
	}
	migrated, err := EnsureSchema(path)
	if err != nil || migrated {
		t.Fatalf("second run: migrated=%v err=%v", migrated, err)
	}
}

func TestIsPermissionErr(t *testing.T) {
	if IsPermissionErr(nil) {
		t.Fatal("nil is not a permission error")
	}
	if !IsPermissionErr(syscall.EACCES) {
		t.Fatal("EACCES should be a permission error")
	}
	if !IsPermissionErr(fmt.Errorf("wrapped: %w", syscall.EACCES)) {
		t.Fatal("wrapped EACCES should be detected")
	}
	if !IsPermissionErr(errors.New("rename x y: Access is denied.")) {
		t.Fatal("Windows access-denied wording should be detected")
	}
	if IsPermissionErr(errors.New("some other error")) {
		t.Fatal("unrelated error misdetected")
	}
}

func TestSchemaVersionOf(t *testing.T) {
	// Legacy file → 0.
	path := writeTempConfig(t, legacyConfig)
	v, err := SchemaVersionOf(path)
	if err != nil || v != 0 {
		t.Fatalf("legacy: v=%d err=%v", v, err)
	}
	// Stamped file → 1.
	if _, err := EnsureSchema(path); err != nil {
		t.Fatal(err)
	}
	v, err = SchemaVersionOf(path)
	if err != nil || v != 1 {
		t.Fatalf("stamped: v=%d err=%v", v, err)
	}
	// Missing file → 0, no error (caller treats as will-be-created).
	v, err = SchemaVersionOf(filepath.Join(t.TempDir(), "nope.toml"))
	if err != nil || v != 0 {
		t.Fatalf("missing: v=%d err=%v", v, err)
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
