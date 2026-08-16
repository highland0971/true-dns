package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

// CurrentSchemaVersion is the config file schema version this build writes
// and understands.
const CurrentSchemaVersion = 1

var schemaVersionRe = regexp.MustCompile(`(?m)^\s*schema_version\s*=\s*(\d+)\s*$`)

// migration upgrades a config file from one schema version to the next.
// Returning an error aborts the chain; conflicts must be reported clearly so
// user parameters are never silently dropped.
type migration struct {
	from  int
	apply func(path string) error
}

// migrations is the upgrade chain. v0 (no schema_version field) has the same
// shape as v1: the migration is a pure version stamp. Field-level migrations
// (renames/moves) register here in the future.
var migrations = []migration{
	{from: 0, apply: migrateV0toV1},
}

// EnsureSchema upgrades the config file at path to the current schema
// version. It returns true when a migration was applied. The file is only
// touched when an upgrade is needed; parameters and comments are preserved.
func EnsureSchema(path string) (bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	v := detectSchemaVersion(data)
	if v > CurrentSchemaVersion {
		return false, fmt.Errorf("config %s uses schema_version %d, newer than this build supports (%d); upgrade true-dns first", path, v, CurrentSchemaVersion)
	}
	migrated := false
	for v < CurrentSchemaVersion {
		var next *migration
		for i := range migrations {
			if migrations[i].from == v {
				next = &migrations[i]
				break
			}
		}
		if next == nil {
			return migrated, fmt.Errorf("no migration from schema_version %d to %d for %s", v, v+1, path)
		}
		if err := next.apply(path); err != nil {
			return migrated, fmt.Errorf("migrating %s from schema_version %d: %w", path, v, err)
		}
		migrated = true
		v++
	}
	return migrated, nil
}

func detectSchemaVersion(data []byte) int {
	m := schemaVersionRe.FindSubmatch(data)
	if m == nil {
		return 0 // legacy files without the field
	}
	var v int
	for _, c := range m[1] {
		v = v*10 + int(c-'0')
	}
	return v
}

// migrateV0toV1 stamps the file with schema_version = 1, preserving the
// original content (comments, ordering, parameters).
func migrateV0toV1(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if bytes.Contains(data, []byte("schema_version")) {
		return fmt.Errorf("file already carries schema_version but was detected as v0")
	}
	stamp := []byte("# schema_version: managed by true-dns, do not edit\nschema_version = 1\n\n")
	out := append(stamp, data...)
	return atomicWrite(path, out)
}

// atomicWrite replaces path atomically, preserving permissions where the OS
// reports them.
func atomicWrite(path string, data []byte) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	tmp := filepath.Join(filepath.Dir(path), ".config.tmp")
	if err := os.WriteFile(tmp, data, info.Mode().Perm()); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
