//go:build windows

package main

import "os"

// terminationSignals are the signals that stop the agent cleanly. Windows
// has no SIGTERM; the service manager stops the process instead.
func terminationSignals() []os.Signal {
	return []os.Signal{os.Interrupt}
}
