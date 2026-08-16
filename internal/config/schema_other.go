//go:build !windows

package config

import (
	"errors"
	"os"
)

// inPlaceReplace is unavailable outside Windows: there, rename failures are
// reported and the original file stays intact (fail-safe), so a locked
// destination simply retries on the next run.
func inPlaceReplace(path string, data []byte, mode os.FileMode) error {
	return errors.New("in-place fallback is only available on Windows")
}
