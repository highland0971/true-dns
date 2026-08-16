//go:build windows

package config

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// TestAtomicWriteInPlaceFallbackWindows exercises the Windows-only in-place
// fallback (runs only on Windows; the Linux CI covers the fail-safe branch).
func TestAtomicWriteInPlaceFallbackWindows(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	orig := renameFile
	defer func() { renameFile = orig }()
	renameFile = func(from, to string) error { return syscall.EACCES } // always locked
	if err := atomicWrite(path, []byte("new")); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "new" {
		t.Fatalf("fallback content = %q", data)
	}
}
