//go:build windows

package config

import (
	"fmt"
	"os"
)

// inPlaceReplace rewrites the destination in place with fsync. It exists for
// Windows, where an editor/antivirus holding the destination makes rename
// fail with access denied; the rewrite lands once write sharing is allowed.
// Trade-off: O_TRUNC + write has a small crash window (a kill mid-write can
// truncate the file), accepted because migration content is tiny and the
// alternative is migration never landing. The atomic rename path is always
// tried first.
func inPlaceReplace(path string, data []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("fsync %s: %w", path, err)
	}
	return f.Close()
}
