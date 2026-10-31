//go:build unix

package main

import (
	"io"
)

// openAgentLog returns the writer for the agent's own lines, and a function
// that releases it. systemd and launchd capture the process's standard
// error, so a terminal and a service both use it.
func openAgentLog(_ bool, stderr io.Writer) (io.Writer, func()) {
	return stderr, func() {}
}
