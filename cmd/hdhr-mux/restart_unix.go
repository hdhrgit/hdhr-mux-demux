//go:build unix

package main

import (
	"os"
	"syscall"
)

// execSelf replaces the process with a fresh copy of itself, with the same
// arguments and environment. The process id stays the same, so whatever
// supervises it (Docker, systemd, an LXC init) sees one continuous process.
// Descriptors Go opened are close-on-exec, so nothing leaks into the new image.
// It returns only if it could not.
func execSelf() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	return syscall.Exec(exe, os.Args, os.Environ())
}
