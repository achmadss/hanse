//go:build unix

package main

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestSIGHUPReloadsConfiguration requires SIGHUP to run the same reload as
// POST /v1/capabilities/reload: a capability package and its route that
// appeared after startup become runnable without a restart.
func TestSIGHUPReloadsConfiguration(t *testing.T) {
	dir := t.TempDir()
	writeConfigTree(t, dir, "public_url: http://127.0.0.1:8443\n")
	g, token := startGatewayWithToken(t, dir)
	stop := handleReloadSignals(g)
	t.Cleanup(stop)

	writeReloadCapability(t, dir, filepath.Join(dir, "ran"))
	if err := os.WriteFile(filepath.Join(dir, "routes.yaml"), []byte(reloadRoutes), 0o644); err != nil {
		t.Fatalf("add the route: %v", err)
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatalf("send SIGHUP: %v", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		status, body := doJSON(t, g, token, http.MethodGet, "/v1/capabilities", "")
		if status == http.StatusOK && strings.Contains(string(body), `"echo"`) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("SIGHUP did not reload the capability set: %d %s", status, body)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
