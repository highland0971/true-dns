package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// CurrentSchemaVersion is the config file schema version this build writes
// and understands.
const CurrentSchemaVersion = 1

// ErrSchemaNewer reports a config file written by a newer build; callers must
// treat it as fatal instead of downgrading silently.
var ErrSchemaNewer = errors.New("config schema version is newer than this build")

// schemaVersionLineRe matches a line assigning schema_version, optionally
// followed by a trailing comment. The comment is dropped when a version-0
// line is rewritten to the canonical form.
var schemaVersionLineRe = regexp.MustCompile(`^\s*schema_version\s*=\s*(\d+)\s*(?:#.*)?$`)

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
// Each migration step writes atomically, so a mid-chain failure never leaves
// a corrupt file. A cross-process lock file serializes concurrent migrations
// (e.g. run and tray started together).
func EnsureSchema(path string) (bool, error) {
	migrated := false
	err := withConfigLock(path, func() error {
		m, err := ensureSchemaLocked(path)
		migrated = m
		return err
	})
	return migrated, err
}

// ensureSchemaLocked is the lock-free body of EnsureSchema.
func ensureSchemaLocked(path string) (bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	v := detectSchemaVersion(data)
	if v > CurrentSchemaVersion {
		return false, fmt.Errorf("%w: %s uses schema_version %d, this build supports up to %d; upgrade true-dns first",
			ErrSchemaNewer, path, v, CurrentSchemaVersion)
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

// detectSchemaVersion scans for a schema_version assignment line; files
// without one (or with an unrecognized form) are treated as v0 legacy.
func detectSchemaVersion(data []byte) int {
	for _, line := range strings.Split(string(data), "\n") {
		if m := schemaVersionLineRe.FindStringSubmatch(strings.TrimSuffix(line, "\r")); m != nil {
			var v int
			for _, c := range m[1] {
				v = v*10 + int(c-'0')
			}
			return v
		}
	}
	return 0
}

// migrateV0toV1 stamps the file with schema_version = 1, preserving the
// original content (comments, ordering, parameters). An explicit
// "schema_version = 0" line is rewritten in place; otherwise the stamp is
// prepended.
func migrateV0toV1(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	lines := strings.Split(string(data), "\n")
	replaced := false
	for i, line := range lines {
		trimmed := strings.TrimSuffix(line, "\r")
		if m := schemaVersionLineRe.FindStringSubmatch(trimmed); m != nil && m[1] == "0" {
			lines[i] = "schema_version = 1"
			replaced = true
			break
		}
	}
	var out []byte
	if replaced {
		out = []byte(strings.Join(lines, "\n"))
	} else {
		stamp := "# schema_version: managed by true-dns, do not edit\nschema_version = 1\n\n"
		out = append([]byte(stamp), data...)
	}
	return atomicWrite(path, out)
}

// withConfigLock serializes config migration across processes using a
// sidecar lock file with stale-lock recovery.
func withConfigLock(path string, fn func() error) error {
	lockPath := path + ".lock"
	deadline := time.Now().Add(10 * time.Second)
	for {
		f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			defer func() { f.Close(); os.Remove(lockPath) }()
			return fn()
		}
		if os.IsExist(err) {
			if info, serr := os.Stat(lockPath); serr == nil && time.Since(info.ModTime()) > 10*time.Second {
				os.Remove(lockPath) // stale lock from a crashed process
				continue
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("config lock timeout waiting on %s", lockPath)
			}
			time.Sleep(50 * time.Millisecond)
			continue
		}
		return err
	}
}

// atomicWrite replaces path atomically via a per-process unique temp file in
// the same directory (safe against concurrent migrations).
func atomicWrite(path string, data []byte) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Chmod(info.Mode().Perm()); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, path)
}
