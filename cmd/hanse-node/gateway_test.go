package main

import (
	"io"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/achmadss/hanse/internal/agentconn"
	"github.com/achmadss/hanse/internal/api"
	"github.com/achmadss/hanse/internal/enroll"
	"github.com/achmadss/hanse/internal/registry"
)

// testGateway is an in-process Gateway with node-a and the echo
// capability. It lets a command test enroll a node and connect its agent
// without root or an external process.
type testGateway struct {
	hub        *agentconn.Hub
	enrollment *enroll.Service
	server     *httptest.Server
}

// newTestGateway builds the Gateway side of a command test.
func newTestGateway(t *testing.T) *testGateway {
	t.Helper()
	reg, warnings, err := registry.Load(registry.Options{
		Dirs: []string{filepath.Join("..", "..", "capabilities")},
		Input: registry.Input{
			Nodes: []registry.Node{{
				ID: "node-a", OS: runtime.GOOS, Arch: runtime.GOARCH,
				Roles: []string{"server"}, Enabled: true,
			}},
			Routes: registry.Routes{Capabilities: map[string]registry.CapabilityRoute{
				"echo": {Executors: []string{"node-a"}, DefaultExecutor: "node-a"},
			}},
		},
	})
	if err != nil {
		t.Fatalf("load the registry: %v", err)
	}
	for _, warning := range warnings {
		// This Gateway test loads every capability directory, and only echo
		// is routed here. Another capability's missing route is not this
		// test's concern.
		if warning.Capability != "echo" &&
			strings.Contains(warning.Message, "routes.yaml does not mention this capability") {
			continue
		}
		t.Fatalf("warnings: %+v", warnings)
	}
	var enrollment *enroll.Service
	hub, err := agentconn.New(agentconn.Config{
		Registry:       reg,
		GatewayVersion: "1.0.0",
		Verify: func(credential string) (string, error) {
			if enrollment == nil {
				return "", io.ErrUnexpectedEOF
			}
			return enrollment.Verify(credential)
		},
		HeartbeatInterval: time.Second,
		HeartbeatTimeout:  5 * time.Second,
		ReconnectWindow:   time.Minute,
	})
	if err != nil {
		t.Fatalf("build the hub: %v", err)
	}
	enrollment, err = enroll.Open(enroll.Options{StateDir: t.TempDir(), Disconnect: hub})
	if err != nil {
		t.Fatalf("open the enrollment store: %v", err)
	}
	t.Cleanup(func() { _ = enrollment.Close() })
	server := httptest.NewServer(api.New(reg, nil, enrollment, api.WithAgentHub(hub)))
	t.Cleanup(server.Close)
	return &testGateway{hub: hub, enrollment: enrollment, server: server}
}
