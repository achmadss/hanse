package registry

import (
	"errors"
	"fmt"
	"sync"
	"testing"

	herrors "github.com/achmadss/hanse/internal/errors"
)

// nodeRegistryInput is a small node set: the Gateway host, two executors,
// a target without an agent, and a disabled node.
func nodeRegistryInput() Input {
	return Input{
		Nodes: []Node{
			{ID: "gateway", OS: "linux", Arch: "amd64", Roles: []string{"server"}, Enabled: true},
			{ID: "node-a", OS: "linux", Arch: "arm64", Roles: []string{"server"}, Enabled: true},
			{ID: "node-b", OS: "linux", Arch: "arm64", Roles: []string{"server"}, Enabled: true},
			{ID: "node-c", OS: "windows", Arch: "amd64", Roles: []string{"pc"}, Enabled: true},
			{ID: "node-d", OS: "linux", Arch: "amd64", Roles: []string{"pc"}, Enabled: false},
		},
		GatewayHost: "gateway",
	}
}

// nodeRegistryCapabilities routes pc.up to the Gateway host and node-a,
// pc.down to node-a and node-b, and weather.current to the Gateway host
// only. node-c and node-d can execute nothing.
func nodeRegistryCapabilities() []*Capability {
	return []*Capability{
		{
			ID: "pc",
			Routing: map[string]OperationRouting{
				"up":   {Executors: []string{"gateway", "node-a"}, DefaultExecutor: "gateway"},
				"down": {Executors: []string{"node-a", "node-b"}, DefaultExecutor: "node-a"},
			},
		},
		{
			ID: "weather",
			Routing: map[string]OperationRouting{
				"current": {Executors: []string{"gateway"}, DefaultExecutor: "gateway"},
			},
		},
	}
}

func TestNewNodeRegistryLookupAndList(t *testing.T) {
	reg, err := NewNodeRegistry(nodeRegistryInput(), nodeRegistryCapabilities())
	if err != nil {
		t.Fatal(err)
	}
	n, ok := reg.Get("node-a")
	if !ok {
		t.Fatal("Get(node-a) is not found")
	}
	if n.ID != "node-a" || n.OS != "linux" || n.Arch != "arm64" || !n.Enabled {
		t.Errorf("Get(node-a) = %+v, want the configured linux/arm64 enabled node", n.Node)
	}
	if n.Roles[0] != "server" {
		t.Errorf("Get(node-a).Roles = %v, want [server]", n.Roles)
	}
	if _, ok := reg.Get("node-e"); ok {
		t.Error("Get(node-e) is found, want only configured nodes")
	}
	if host := reg.GatewayHost(); host != "gateway" {
		t.Errorf("GatewayHost() = %q, want gateway", host)
	}
	if enabled := reg.Enabled("node-c"); !enabled {
		t.Error("Enabled(node-c) = false, want true for a target node")
	}

	list := reg.List()
	if len(list) != 5 {
		t.Fatalf("List() has %d nodes, want 5", len(list))
	}
	ids := ""
	for i, n := range list {
		ids += n.ID
		if i < len(list)-1 {
			ids += ", "
		}
	}
	wantIDs := "gateway, node-a, node-b, node-c, node-d"
	if ids != wantIDs {
		t.Errorf("List() order = %q, want %q", ids, wantIDs)
	}
}

func TestNodeRegistryStatusDefaults(t *testing.T) {
	reg, err := NewNodeRegistry(nodeRegistryInput(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"node-a", "node-b", "node-c", "node-d"} {
		if status, ok := reg.Status(id); !ok || status != StatusOffline {
			t.Errorf("Status(%q) = %q, want offline: a started Gateway has no connected agent", id, status)
		}
	}
	if status, ok := reg.Status("gateway"); !ok || status != StatusOnline {
		t.Errorf("Status(gateway) = %q, want online: the embedded agent never disconnects", status)
	}
	if _, ok := reg.Status("node-e"); ok {
		t.Error("Status(node-e) is found, want an unknown node")
	}
}

func TestNodeRegistrySetStatus(t *testing.T) {
	reg, err := NewNodeRegistry(nodeRegistryInput(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.SetStatus("node-a", StatusOnline); err != nil {
		t.Fatalf("SetStatus(node-a, online): %s", err)
	}
	if status, _ := reg.Status("node-a"); status != StatusOnline {
		t.Errorf("Status(node-a) = %q, want online after the agent connected", status)
	}
	view, _ := reg.Get("node-a")
	if view.Status != StatusOnline {
		t.Errorf("Get(node-a).Status = %q, want online", view.Status)
	}
	if err := reg.SetStatus("node-a", StatusOffline); err != nil {
		t.Fatalf("SetStatus(node-a, offline): %s", err)
	}
	if status, _ := reg.Status("node-a"); status != StatusOffline {
		t.Errorf("Status(node-a) = %q, want offline after the connection was lost", status)
	}
}

func TestNodeRegistrySetStatusRejectsBadCalls(t *testing.T) {
	reg, err := NewNodeRegistry(nodeRegistryInput(), nil)
	if err != nil {
		t.Fatal(err)
	}
	// The embedded agent keeps the Gateway host online.
	if err := reg.SetStatus("gateway", StatusOffline); err == nil {
		t.Error("SetStatus(gateway, offline) succeeds, want the fixed online status")
	}
	if err := reg.SetStatus("gateway", StatusOnline); err == nil {
		t.Error("SetStatus(gateway, online) succeeds, want the fixed online status")
	}
	if status, _ := reg.Status("gateway"); status != StatusOnline {
		t.Errorf("Status(gateway) = %q, want online after the rejected SetStatus calls", status)
	}
	if err := reg.SetStatus("node-e", StatusOnline); !errors.Is(err, herrors.Internal) {
		t.Errorf("SetStatus(node-e, online) error = %v, want the unknown node error", err)
	}
	if err := reg.SetStatus("node-a", "ready"); err == nil {
		t.Error("SetStatus(node-a, ready) succeeds, want an invalid status error")
	}
}

func TestNodeRegistryCapabilities(t *testing.T) {
	reg, err := NewNodeRegistry(nodeRegistryInput(), nodeRegistryCapabilities())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]NodeCapability{
		"gateway": {{Capability: "pc", Operations: []string{"up"}}, {Capability: "weather", Operations: []string{"current"}}},
		"node-a":  {{Capability: "pc", Operations: []string{"down", "up"}}},
		"node-b":  {{Capability: "pc", Operations: []string{"down"}}},
		"node-c":  {},
		"node-d":  {},
	}
	for id, wantCaps := range want {
		caps, ok := reg.Capabilities(id)
		if !ok {
			t.Fatalf("Capabilities(%q) is not found", id)
		}
		if fmt.Sprint(caps) != fmt.Sprint(wantCaps) {
			t.Errorf("Capabilities(%q) = %v, want %v", id, caps, wantCaps)
		}
	}
	if _, ok := reg.Capabilities("node-e"); ok {
		t.Error("Capabilities(node-e) is found, want an unknown node")
	}
}

func TestNewNodeRegistryRejectsBadNodes(t *testing.T) {
	in := nodeRegistryInput()
	in.Nodes = append(in.Nodes, Node{ID: "node-a", OS: "linux", Arch: "amd64", Enabled: true})
	_, err := NewNodeRegistry(in, nil)
	if err == nil {
		t.Error("NewNodeRegistry with a duplicate node ID succeeds, want an error")
	}
	in = nodeRegistryInput()
	in.Nodes[0].ID = "node_a"
	_, err = NewNodeRegistry(in, nil)
	if err == nil {
		t.Error("NewNodeRegistry with an invalid node ID succeeds, want an error")
	}
	in = nodeRegistryInput()
	in.GatewayHost = "node-e"
	_, err = NewNodeRegistry(in, nil)
	if err == nil {
		t.Error("NewNodeRegistry with an unknown Gateway host succeeds, want an error")
	}
}

func TestLoadBuildsNodeRegistry(t *testing.T) {
	root := t.TempDir()
	writeCapability(t, root, "pc", pcManifest)
	reg, warnings, err := Load(Options{Dirs: []string{root}, Input: baseInput()})
	if err != nil {
		t.Fatalf("Load: %s, warnings: %v", err, warnings)
	}
	if len(warnings) != 0 {
		t.Errorf("Load warnings = %v, want none", warnings)
	}
	// The wildcard executor list of section 5.1 covers the Gateway host
	// and both linux executors; node-c has no linux entry and node-d is
	// disabled.
	want := map[string][]NodeCapability{
		"gateway": {{Capability: "pc", Operations: []string{"down", "status", "up"}}},
		"node-a":  {{Capability: "pc", Operations: []string{"down", "status", "up"}}},
		"node-b":  {{Capability: "pc", Operations: []string{"down", "status", "up"}}},
		"node-c":  {},
		"node-d":  {},
	}
	for id, wantCaps := range want {
		caps, ok := reg.Nodes.Capabilities(id)
		if !ok {
			t.Fatalf("Nodes.Capabilities(%q) is not found", id)
		}
		if fmt.Sprint(caps) != fmt.Sprint(wantCaps) {
			t.Errorf("Nodes.Capabilities(%q) = %v, want %v", id, caps, wantCaps)
		}
	}
	if status, _ := reg.Nodes.Status("gateway"); status != StatusOnline {
		t.Errorf("Status(gateway) = %q, want online from the embedded agent", status)
	}
	if status, _ := reg.Nodes.Status("node-a"); status != StatusOffline {
		t.Errorf("Status(node-a) = %q, want offline at startup", status)
	}
	if err := reg.Nodes.SetStatus("node-a", StatusOnline); err != nil {
		t.Fatalf("SetStatus through the loaded registry: %s", err)
	}
	if status, _ := reg.Nodes.Status("node-a"); status != StatusOnline {
		t.Errorf("Status(node-a) = %q, want online", status)
	}
}

func TestLoadWithoutNodesYieldsEmptyNodeRegistry(t *testing.T) {
	reg, warnings, err := Load(Options{})
	if err != nil {
		t.Fatalf("Load: %s", err)
	}
	if len(warnings) != 0 {
		t.Errorf("Load warnings = %v, want none", warnings)
	}
	if nodes := reg.Nodes.List(); len(nodes) != 0 {
		t.Errorf("Nodes.List() = %v, want none", nodes)
	}
	if err := reg.Nodes.SetStatus("gateway", StatusOnline); err == nil {
		t.Error("SetStatus succeeds on an unconfigured Gateway host, want an unknown node error")
	}
}

func TestNodeRegistryConcurrentStatusAndList(t *testing.T) {
	reg, err := NewNodeRegistry(nodeRegistryInput(), nodeRegistryCapabilities())
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		id := fmt.Sprintf("node-%c", 'a'+i)
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			status := StatusOnline
			for j := 0; j < 100; j++ {
				if err := reg.SetStatus(id, status); err != nil {
					t.Errorf("SetStatus(%q, %q): %s", id, status, err)
					return
				}
				view, ok := reg.Get(id)
				if !ok || view.ID != id {
					t.Errorf("Get(%q) = %+v, want the configured node", id, view.Node)
					return
				}
				reg.List()
				reg.Capabilities(id)
				status = StatusOffline
				if view.Status == StatusOffline {
					status = StatusOnline
				}
			}
		}(id)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; j < 100; j++ {
			// The disabled node keeps its status, so the list stays
			// while other connections change theirs.
			if _, ok := reg.Get("node-d"); !ok {
				t.Error("Get(node-d) is not found")
				return
			}
			reg.List()
		}
	}()
	wg.Wait()
}

func TestNodeRegistryNilReceiver(t *testing.T) {
	var reg *NodeRegistry
	if _, ok := reg.Get("gateway"); ok {
		t.Error("Get succeeds on a nil registry")
	}
	if nodes := reg.List(); nodes != nil {
		t.Errorf("List on a nil registry = %v, want nil", nodes)
	}
	if reg.SetStatus("gateway", StatusOnline) != nil {
		t.Error("SetStatus on a nil registry returns an error")
	}
	if _, ok := reg.Capabilities("gateway"); ok {
		t.Error("Capabilities succeeds on a nil registry")
	}
	if _, ok := reg.Status("gateway"); ok {
		t.Error("Status succeeds on a nil registry")
	}
	if reg.Enabled("gateway") {
		t.Error("Enabled on a nil registry reports enabled")
	}
	if host := reg.GatewayHost(); host != "" {
		t.Error("GatewayHost on a nil registry is not empty")
	}
}

// nodeManifestForOS is a minimal exec capability whose entry covers OSname.
func nodeManifestForOS(osname string) string {
	return fmt.Sprintf(`
version: 1
id: osprobe
name: OS probe
description: Report what the node runs
contract: hanse.handler/v1

handler:
  kind: exec
  entry:
    %s: [echo, hello]

operations:
  hello:
    description: say hello
    mode: sync
    arguments:
      type: object
`, osname)
}

// TestNodeRegistryWildcardSkipsEnabledOSFilter walks the Load wildcard
// expansion against nodes with different OSes and enabled flags, to prove
// the node registry maps exactly the routing lists.
func TestNodeRegistryWildcardSkipsEnabledOSFilter(t *testing.T) {
	root := t.TempDir()
	writeCapability(t, root, "osprobe", nodeManifestForOS("linux"))
	in := baseInput()
	in.Routes = Routes{Capabilities: map[string]CapabilityRoute{}}
	in.Routes.Capabilities["osprobe"] = CapabilityRoute{Wildcard: true}
	reg, warnings, err := Load(Options{Dirs: []string{root}, Input: in})
	if err != nil {
		t.Fatalf("Load: %s, warnings: %v", err, warnings)
	}
	if len(warnings) != 0 {
		t.Errorf("Load warnings = %v, want none", warnings)
	}
	// The wildcard list keeps gateway, node-a, and node-b; node-c runs
	// windows and node-d is disabled.
	got, _ := reg.Nodes.Capabilities("node-a")
	if len(got) != 1 || got[0].Capability != "osprobe" || len(got[0].Operations) != 1 || got[0].Operations[0] != "hello" {
		t.Errorf("Capabilities(node-a) = %v, want osprobe hello", got)
	}
	got, _ = reg.Nodes.Capabilities("node-c")
	if len(got) != 0 {
		t.Errorf("Capabilities(node-c) = %v, want an empty list for a node without an entry command", got)
	}
	got, _ = reg.Nodes.Capabilities("node-d")
	if len(got) != 0 {
		t.Errorf("Capabilities(node-d) = %v, want an empty list for a disabled node", got)
	}
}
