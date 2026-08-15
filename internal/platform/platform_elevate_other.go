//go:build !windows

package platform

// IsElevated is a no-op outside Windows, where elevation is handled through
// sudo/root instead. Returning true keeps CLI elevation logic inert.
func IsElevated() bool { return true }

// Elevate is a no-op outside Windows.
func Elevate() (bool, error) { return false, nil }

// ElevateArgs is a no-op outside Windows.
func ElevateArgs(_ []string) (bool, error) { return false, nil }
