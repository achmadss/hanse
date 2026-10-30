package registry

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestReloadAddsAndDropsCapabilities scans a capability root, adds a package,
// reloads, then removes it and reloads again. The node registry follows the
// new routing, and a capability looked up before the reload keeps its
// snapshot.
func TestReloadAddsAndDropsCapabilities(t *testing.T) {
	root := t.TempDir()
	writeCapability(t, root, "pc", pcManifest)
	input := baseInput()
	input.Routes.Capabilities["weather"] = CapabilityRoute{
		Wildcard:        true,
		DefaultExecutor: "gateway",
	}
	reg, _, err := Load(Options{Dirs: []string{root}, Input: input})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	before, ok := reg.Capability("pc")
	if !ok {
		t.Fatal("pc did not load")
	}
	if _, ok := reg.Capability("weather"); ok {
		t.Fatal("weather loaded before it was written")
	}

	weatherDir := writeCapability(t, root, "weather", httpManifest)
	if _, err := reg.Reload(); err != nil {
		t.Fatalf("Reload after add: %v", err)
	}
	if _, ok := reg.Capability("weather"); !ok {
		t.Fatal("weather did not load after reload")
	}
	if caps, ok := reg.Nodes.Capabilities("gateway"); !ok || !hasCapability(caps, "weather") {
		t.Errorf("gateway capabilities after reload = %+v, want weather", caps)
	}
	if len(before.Operations) == 0 {
		t.Error("the snapshot taken before the reload lost its operations")
	}

	if err := os.RemoveAll(weatherDir); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Reload(); err != nil {
		t.Fatalf("Reload after remove: %v", err)
	}
	if _, ok := reg.Capability("weather"); ok {
		t.Fatal("weather still loads after its directory was removed")
	}
	if caps, ok := reg.Nodes.Capabilities("gateway"); !ok || hasCapability(caps, "weather") {
		t.Errorf("gateway capabilities after remove = %+v, want no weather", caps)
	}
}

// hasCapability reports whether the node capability list holds id.
func hasCapability(caps []NodeCapability, id string) bool {
	for _, c := range caps {
		if c.Capability == id {
			return true
		}
	}
	return false
}

// TestReloadRefusesDuplicateCapability checks that a package whose ID is
// already loaded fails the reload and leaves the registry unchanged.
func TestReloadRefusesDuplicateCapability(t *testing.T) {
	root := t.TempDir()
	writeCapability(t, root, "pc", pcManifest)
	reg, _, err := Load(Options{Dirs: []string{root}, Input: baseInput()})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// A second directory with the same capability ID.
	second := filepath.Join(root, "copy")
	if err := os.MkdirAll(second, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(second, manifestFile), []byte(pcManifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Reload(); err == nil {
		t.Fatal("Reload accepted a duplicate capability ID")
	}
	if _, ok := reg.Capability("pc"); !ok {
		t.Fatal("a failed reload dropped the loaded capability")
	}
}

// TestReloadKeepsBuiltin checks that a builtin capability survives a reload
// and that a package cannot take its ID.
func TestReloadKeepsBuiltin(t *testing.T) {
	root := t.TempDir()
	reg, _, err := Load(Options{Dirs: []string{root}, Input: refreshInput()})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	builtin := builtinTestCapability(t)
	route := OperationRouting{Executors: []string{"host-1"}, DefaultExecutor: "host-1"}
	builtin.Routing = map[string]OperationRouting{"say": route, "sleep": route}
	if err := reg.RegisterBuiltin(builtin); err != nil {
		t.Fatalf("RegisterBuiltin: %v", err)
	}

	writeCapability(t, root, "pc", pcManifest)
	if _, err := reg.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if _, ok := reg.Capability("echo"); !ok {
		t.Fatal("the reload dropped the builtin capability")
	}
	if _, ok := reg.Capability("pc"); !ok {
		t.Fatal("the reload dropped the disk capability")
	}
	if caps, ok := reg.Nodes.Capabilities("host-1"); !ok || !hasCapability(caps, "echo") {
		t.Errorf("host capabilities after reload = %+v, want echo", caps)
	}

	// A package with the builtin's ID is refused and the registry keeps
	// the builtin.
	writeCapability(t, root, "echo", strings.Replace(pcManifest, "id: pc", "id: echo", 1))
	if _, err := reg.Reload(); err == nil {
		t.Fatal("Reload let a package replace a builtin capability")
	}
	if _, ok := reg.Capability("echo"); !ok {
		t.Fatal("a failed reload dropped the builtin capability")
	}
}
