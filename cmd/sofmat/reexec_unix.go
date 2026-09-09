//go:build !windows

package main

import (
	"os"
	"syscall"
)

// reexec REPLACES this process with the freshly installed binary: same PID, no
// child, nothing left behind. syscall.Exec never returns on success.
//
// It used to spawn a child and os.Exit(0) the parent. Under systemd that was
// invisible — Restart=always kills the cgroup and starts the next one clean, so
// .63 sat at 2 processes through 272 restarts. Without a unit, the same code on
// .51 left 990 soflink processes holding ~11 GB of RAM after 90 cycles: the
// parent asked to leave, the child was already running, and with the AppImage's
// outer wrapper in the middle nobody reliably reaped anyone.
//
// Exec cannot do that. There is no second process to leak, whatever supervises
// the service — or does not.
func reexecReal(self string) error {
	args := append([]string{self}, os.Args[1:]...)
	return syscall.Exec(self, args, os.Environ())
}
