//go:build unix

package main

import (
	"io"
)

// runAsService reports whether the operating system's service manager
// started this process. Only Windows starts the agent as a service: on
// Linux and macOS systemd and launchd start an ordinary process.
func runAsService(string, io.Writer) (bool, int) { return false, 0 }
