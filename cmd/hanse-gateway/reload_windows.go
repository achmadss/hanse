//go:build windows

package main

// handleReloadSignals installs no reload signal on Windows: there is no
// SIGHUP. Reload the Gateway with POST /v1/capabilities/reload instead
// (docs/architecture.md section 11.2). The function returns the same stop
// hook as the POSIX build, so the caller needs no build tags.
func handleReloadSignals(g *gateway) func() {
	return func() {}
}
