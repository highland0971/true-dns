//go:build !windows

package config

import (
	"errors"
	"os"
)

// ensureConfigACL is a no-op outside Windows (POSIX permissions are handled
// by umask/ownership).
func ensureConfigACL(path string) error { return nil }

// inPlaceReplace is unavailable outside Windows: there, rename failures are
// reported and the original file stays intact (fail-safe), so a locked
// destination simply retries on the next run.
func inPlaceReplace(path string, data []byte, mode os.FileMode) error {
	return errors.New("in-place fallback is only available on Windows")
}
