package registry

import (
	"errors"
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

// TestReloadReadsFreshNodesAndRoutes reloads with a ReloadInput that adds a
// node and a route, the way the Gateway re-reads nodes.yaml and routes.yaml.
// The new node and the new routing reach the live registry.
func TestReloadReadsFreshNodesAndRoutes(t *testing.T) {
	root := t.TempDir()
	writeCapability(t, root, "pc", pcManifest)
	input := baseInput()
	input.Routes.Capabilities = map[string]CapabilityRoute{}
	current := input
	reg, _, err := Load(Options{
		Dirs:        []string{root},
		Input:       input,
		ReloadInput: func() (Input, error) { return current, nil },
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	pc, ok := reg.Capability("pc")
	if !ok {
		t.Fatal("pc did not load")
	}
	if routing, _ := pc.RoutingOf("up"); len(routing.Executors) != 0 {
		t.Fatalf("pc started routed: %+v", routing)
	}

	current = baseInput()
	current.Nodes = append(current.Nodes, Node{ID: "node-e", OS: "linux", Arch: "amd64", Enabled: true})
	current.Routes.Capabilities["pc"] = CapabilityRoute{Executors: []string{"node-e"}, DefaultExecutor: "node-e"}
	if _, err := reg.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if _, ok := reg.Nodes.Get("node-e"); !ok {
		t.Fatal("the reload did not add node-e")
	}
	reloaded, ok := reg.Capability("pc")
	if !ok {
		t.Fatal("pc disappeared after the reload")
	}
	routing, ok := reloaded.RoutingOf("up")
	if !ok || len(routing.Executors) != 1 || routing.Executors[0] != "node-e" {
		t.Errorf("pc.up routing after reload = %+v, want node-e", routing)
	}
	if routing, _ := pc.RoutingOf("up"); len(routing.Executors) != 0 {
		t.Errorf("the snapshot taken before the reload changed: %+v", routing)
	}
	if caps, ok := reg.Nodes.Capabilities("node-e"); !ok || !hasCapability(caps, "pc") {
		t.Errorf("node-e capabilities after reload = %+v, want pc", caps)
	}
}

// TestReloadKeepsPresenceAndRemovesNodes keeps the presence of a node that
// stays configured, including its agent version, and reports a node the new
// nodes.yaml dropped through the removal hook.
func TestReloadKeepsPresenceAndRemovesNodes(t *testing.T) {
	root := t.TempDir()
	writeCapability(t, root, "pc", pcManifest)
	current := baseInput()
	reg, _, err := Load(Options{
		Dirs:        []string{root},
		Input:       current,
		ReloadInput: func() (Input, error) { return current, nil },
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := reg.Nodes.SetAgentPresence("node-a", StatusOnline, "1.2.3"); err != nil {
		t.Fatalf("set presence: %v", err)
	}
	var removed []string
	reg.SetNodeRemovedHook(func(id string) { removed = append(removed, id) })

	// node-a stays, node-b and node-c disappear from nodes.yaml.
	kept := baseInput()
	kept.Nodes = []Node{
		{ID: "gateway", OS: "linux", Arch: "amd64", Enabled: true},
		{ID: "node-a", Name: "renamed", OS: "linux", Arch: "arm64", Enabled: true},
		{ID: "node-d", OS: "linux", Arch: "amd64", Enabled: false},
	}
	current = kept
	if _, err := reg.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}

	view, ok := reg.Nodes.Get("node-a")
	if !ok {
		t.Fatal("node-a disappeared from the reloaded registry")
	}
	if view.Status != StatusOnline || view.AgentVersion != "1.2.3" {
		t.Errorf("node-a presence after reload = %s/%q, want online/1.2.3", view.Status, view.AgentVersion)
	}
	if view.Name != "renamed" {
		t.Errorf("node-a name after reload = %q, want the new nodes.yaml value", view.Name)
	}
	if _, ok := reg.Nodes.Get("node-b"); ok {
		t.Error("node-b is still configured after the reload dropped it")
	}
	if len(removed) != 2 || removed[0] != "node-b" || removed[1] != "node-c" {
		t.Errorf("removed nodes = %v, want [node-b node-c]", removed)
	}
	if status, ok := reg.Nodes.Status("gateway"); !ok || status != StatusOnline {
		t.Errorf("the Gateway host is %s after reload, want online", status)
	}
	if disabled, ok := reg.Nodes.Get("node-d"); !ok || disabled.Enabled {
		t.Errorf("node-d after reload = %+v, want a configured disabled node", disabled)
	}
}

// TestReloadHostWithoutEmbeddedAgentStaysOffline checks the other Gateway
// layout: with embedded_agent: false the host node keeps the presence its
// agent connection gave it, and a host with no connection stays offline.
func TestReloadHostWithoutEmbeddedAgentStaysOffline(t *testing.T) {
	root := t.TempDir()
	writeCapability(t, root, "pc", pcManifest)
	input := baseInput()
	input.GatewayHostNoEmbeddedAgent = true
	current := input
	reg, _, err := Load(Options{
		Dirs:        []string{root},
		Input:       input,
		ReloadInput: func() (Input, error) { return current, nil },
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, err := reg.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if status, ok := reg.Nodes.Status("gateway"); !ok || status != StatusOffline {
		t.Errorf("the host node is %s with no agent, want offline", status)
	}
	if err := reg.Nodes.SetAgentPresence("gateway", StatusOnline, "2.0.0"); err != nil {
		t.Fatalf("set host presence: %v", err)
	}
	if _, err := reg.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	view, ok := reg.Nodes.Get("gateway")
	if !ok || view.Status != StatusOnline || view.AgentVersion != "2.0.0" {
		t.Errorf("the host node after reload = %+v, want the agent's presence", view)
	}
}

// TestReloadFailureKeepsState requires a reload that cannot read the new
// configuration to report the problem and leave the running state alone.
func TestReloadFailureKeepsState(t *testing.T) {
	root := t.TempDir()
	writeCapability(t, root, "pc", pcManifest)
	current := baseInput()
	fail := false
	reg, _, err := Load(Options{
		Dirs:  []string{root},
		Input: current,
		ReloadInput: func() (Input, error) {
			if fail {
				return Input{}, errReloadInput
			}
			return current, nil
		},
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	fail = true
	if _, err := reg.Reload(); !errors.Is(err, errReloadInput) {
		t.Fatalf("Reload error = %v, want the ReloadInput error", err)
	}
	if _, ok := reg.Capability("pc"); !ok {
		t.Fatal("a failed reload dropped a loaded capability")
	}
	if _, ok := reg.Nodes.Get("node-a"); !ok {
		t.Fatal("a failed reload dropped a configured node")
	}
}

// errReloadInput is the failure a ReloadInput returns in the tests.
var errReloadInput = errors.New("nodes.yaml is unreadable")

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
