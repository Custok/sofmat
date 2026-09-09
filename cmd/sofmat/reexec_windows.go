//go:build windows

package main

import (
	"os"
	"os/exec"
)

// reexec on Windows: there is no exec() that replaces the image, so the only
// option is to start the new binary and leave. That is what every platform used
// to do, and on Windows it is correct — measured on .30 after three consecutive
// auto-updates: exactly one soflink.exe, 32 MB.
//
// The parent exits only AFTER the child has started, so a failure to start
// leaves the current version running rather than nothing at all.
func reexec(self string) error {
	cmd := exec.Command(self, os.Args[1:]...)
	cmd.Stdout, cmd.Stderr, cmd.Stdin = os.Stdout, os.Stderr, os.Stdin
	if err := cmd.Start(); err != nil {
		return err
	}
	os.Exit(0)
	return nil
}
