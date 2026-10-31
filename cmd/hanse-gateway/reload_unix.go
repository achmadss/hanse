//go:build unix

package main

import (
	"os"
	"os/signal"
	"syscall"
)

// handleReloadSignals runs the Gateway's configuration reload on every
// SIGHUP and returns a function that stops listening. The returned function
// is safe to call once, after the Gateway stopped serving. POSIX systems
// have SIGHUP; the Windows build has a no-op implementation.
func handleReloadSignals(g *gateway) func() {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGHUP)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-signals:
				g.reloadConfig()
			case <-done:
				return
			}
		}
	}()
	return func() {
		signal.Stop(signals)
		close(done)
	}
}
