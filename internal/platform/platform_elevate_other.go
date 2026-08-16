//go:build !windows

package platform

import "os/exec"

// IsElevated is a no-op outside Windows, where elevation is handled through
// sudo/root instead. Returning true keeps CLI elevation logic inert.
func IsElevated() bool { return true }

// Elevate is a no-op outside Windows.
func Elevate() (bool, error) { return false, nil }

// ElevateArgs is a no-op outside Windows.
func ElevateArgs(_ []string) (bool, error) { return false, nil }

// ShellOpen opens path with xdg-open (best effort).
func ShellOpen(path string) error {
	cmd := exec.Command("xdg-open", path)
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }() // reap to avoid zombies
	return nil
}
