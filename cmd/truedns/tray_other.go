//go:build !windows

package main

import (
	"errors"
	"flag"
)

// cmdTray is Windows-only; on other platforms it explains itself.
func defaultCommandIsTray() bool { return false }

func cmdTray(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		return err
	}
	return errors.New("tray GUI is only available on Windows")
}
