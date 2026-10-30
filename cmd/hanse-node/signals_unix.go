//go:build unix

package main

import (
	"os"
	"syscall"
)

// terminationSignals are the signals that stop the agent cleanly.
func terminationSignals() []os.Signal {
	return []os.Signal{os.Interrupt, syscall.SIGTERM}
}
