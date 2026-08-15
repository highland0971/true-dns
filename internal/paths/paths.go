// Package paths resolves the platform-specific directories used for
// configuration and persistent takeover state.
package paths

import (
	"os"
	"path/filepath"
	"runtime"
)

// StateDir returns the directory holding config.toml and takeover-state.json.
// Windows: %ProgramData%\truedns. Linux (root): /etc/truedns; otherwise the
// per-user XDG config directory so non-root development also works.
func StateDir() string {
	if runtime.GOOS == "windows" {
		pd := os.Getenv("ProgramData")
		if pd == "" {
			pd = `C:\ProgramData`
		}
		return filepath.Join(pd, "truedns")
	}
	if os.Geteuid() == 0 {
		return "/etc/truedns"
	}
	if h, err := os.UserHomeDir(); err == nil {
		return filepath.Join(h, ".config", "truedns")
	}
	return "."
}
