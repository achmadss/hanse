package resolver

import (
	"errors"
	"testing"

	herrors "github.com/achmadss/hanse/internal/errors"
	"github.com/achmadss/hanse/internal/registry"
)

// fakeCapabilities is the loaded-capability view of one test.
type fakeCapabilities map[string]*registry.Capability

func (f fakeCapabilities) Capability(id string) (*registry.Capability, bool) {
	c, ok := f[id]
	return c, ok
}

// fakeNodes is the node view of one test.
type fakeNodes map[string]registry.NodeView

func (f fakeNodes) Get(id string) (registry.NodeView, bool) {
	view, ok := f[id]
	return view, ok
}

// onlineNode builds an enabled, online node view.
func onlineNode(id string) registry.NodeView {
	return registry.NodeView{
		Node:   registry.Node{ID: id, Enabled: true},
		Status: registry.StatusOnline,
	}
}

// offlineNode builds an enabled, offline node view.
func offlineNode(id string) registry.NodeView {
	return registry.NodeView{
		Node:   registry.Node{ID: id, Enabled: true},
		Status: registry.StatusOffline,
	}
}

// disabledNode builds a disabled, online node view.
func disabledNode(id string) registry.NodeView {
	return registry.NodeView{
		Node:   registry.Node{ID: id, Enabled: false},
		Status: registry.StatusOnline,
	}
}

// capability builds one loaded capability with a single operation and its
// routing. Pass a nil routing to model an operation without a routing
// entry, as a capability that routes.yaml does not mention has.
func capability(opID string, routing *registry.OperationRouting) *registry.Capability {
	c := &registry.Capability{
		ID:         "pc",
		Operations: map[string]*registry.Operation{opID: {ID: opID}},
		Routing:    map[string]registry.OperationRouting{},
	}
	if routing != nil {
		c.Routing[opID] = *routing
	}
	return c
}

// testNodes is the node set of the tables: two eligible executors, a known
// target that is not eligible, a disabled executor, and an offline one.
func testNodes() fakeNodes {
	return fakeNodes{
		"gateway": onlineNode("gateway"),
		"node-a":  onlineNode("node-a"),
		"node-b":  onlineNode("node-b"),
		"node-c":  onlineNode("node-c"),
		"node-d":  disabledNode("node-d"),
		"node-e":  offlineNode("node-e"),
	}
}

// TestResolvePrecedence covers the four precedence steps of
// docs/architecture.md section 5. The registry joins routes.yaml, so the
// operation routing already carries the capability default when the
// operation sets none.
func TestResolvePrecedence(t *testing.T) {
	tests := []struct {
		name      string
		routing   *registry.OperationRouting
		requested string
		want      Resolution
	}{
		{
			name: "explicit executor wins over the default",
			routing: &registry.OperationRouting{
				Executors:       []string{"node-a", "node-b"},
				DefaultExecutor: "node-a",
			},
			requested: "node-b",
			want:      Resolution{Executor: "node-b", Source: SourceExplicit},
		},
		{
			name: "explicit executor without a default",
			routing: &registry.OperationRouting{
				Executors: []string{"node-a", "node-b"},
			},
			requested: "node-b",
			want:      Resolution{Executor: "node-b", Source: SourceExplicit},
		},
		{
			name: "operation or capability default when no executor is named",
			routing: &registry.OperationRouting{
				Executors:       []string{"node-a", "node-b"},
				DefaultExecutor: "node-a",
			},
			want: Resolution{Executor: "node-a", Source: SourceDefault},
		},
		{
			name: "default executor that is not first in the list",
			routing: &registry.OperationRouting{
				Executors:       []string{"node-a", "node-b"},
				DefaultExecutor: "node-b",
			},
			want: Resolution{Executor: "node-b", Source: SourceDefault},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := New(fakeCapabilities{"pc": capability("down", tc.routing)}, testNodes())
			got, err := r.Resolve("pc", "down", tc.requested)
			if err != nil {
				t.Fatalf("Resolve returned %v, want %+v", err, tc.want)
			}
			if got != tc.want {
				t.Fatalf("Resolve returned %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestResolveRejections covers every failure case and the error code of
// docs/architecture.md section 14.
func TestResolveRejections(t *testing.T) {
	routing := &registry.OperationRouting{
		Executors:       []string{"node-a", "node-d", "node-e"},
		DefaultExecutor: "node-a",
	}
	noDefault := &registry.OperationRouting{Executors: []string{"node-a"}}
	badDefault := &registry.OperationRouting{
		Executors:       []string{"node-a"},
		DefaultExecutor: "node-b",
	}

	tests := []struct {
		name       string
		capability *registry.Capability
		nodes      fakeNodes
		noNodes    bool
		capID      string
		opID       string
		requested  string
		code       *herrors.Code
	}{
		{
			name:       "unknown capability",
			capability: capability("down", routing),
			capID:      "nope",
			opID:       "down",
			code:       herrors.UnknownCapability,
		},
		{
			name:       "unknown operation",
			capability: capability("down", routing),
			capID:      "pc",
			opID:       "up",
			code:       herrors.UnknownOperation,
		},
		{
			name:       "explicit executor is not a known node",
			capability: capability("down", routing),
			opID:       "down",
			requested:  "node-x",
			code:       herrors.InvalidExecutor,
		},
		{
			name:       "explicit executor is a known node but not eligible",
			capability: capability("down", routing),
			opID:       "down",
			requested:  "node-c",
			code:       herrors.ExecutorNotEligible,
		},
		{
			name:       "explicit executor is disabled",
			capability: capability("down", routing),
			opID:       "down",
			requested:  "node-d",
			code:       herrors.ExecutorUnavailable,
		},
		{
			name:       "explicit executor is offline",
			capability: capability("down", routing),
			opID:       "down",
			requested:  "node-e",
			code:       herrors.ExecutorUnavailable,
		},
		{
			name:       "no executor named and no default",
			capability: capability("down", noDefault),
			opID:       "down",
			code:       herrors.NoDefaultExecutor,
		},
		{
			name:       "no executor named and the capability has no routing",
			capability: capability("down", nil),
			opID:       "down",
			code:       herrors.NoDefaultExecutor,
		},
		{
			name:       "default executor is disabled",
			capability: capability("down", &registry.OperationRouting{Executors: []string{"node-d"}, DefaultExecutor: "node-d"}),
			opID:       "down",
			code:       herrors.ExecutorUnavailable,
		},
		{
			name:       "default executor is offline",
			capability: capability("down", &registry.OperationRouting{Executors: []string{"node-e"}, DefaultExecutor: "node-e"}),
			opID:       "down",
			code:       herrors.ExecutorUnavailable,
		},
		{
			name:       "default executor is not eligible",
			capability: capability("down", badDefault),
			opID:       "down",
			code:       herrors.ExecutorNotEligible,
		},
		{
			name:       "default executor is not a known node",
			capability: capability("down", &registry.OperationRouting{Executors: []string{"node-x"}, DefaultExecutor: "node-x"}),
			opID:       "down",
			code:       herrors.InvalidExecutor,
		},
		{
			name:       "explicit executor with no routing entry",
			capability: capability("down", nil),
			opID:       "down",
			requested:  "node-a",
			code:       herrors.ExecutorNotEligible,
		},
		{
			name:       "nil node lookup",
			capability: capability("down", routing),
			noNodes:    true,
			opID:       "down",
			requested:  "node-a",
			code:       herrors.InvalidExecutor,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			nodes := tc.nodes
			if nodes == nil && !tc.noNodes {
				nodes = testNodes()
			}
			capID := tc.capID
			if capID == "" {
				capID = "pc"
			}
			r := New(fakeCapabilities{"pc": tc.capability}, nodes)
			got, err := r.Resolve(capID, tc.opID, tc.requested)
			if err == nil {
				t.Fatalf("Resolve returned %+v, want error %s", got, tc.code.Name())
			}
			if !errors.Is(err, tc.code) {
				t.Fatalf("Resolve error is %v, want code %s", err, tc.code.Name())
			}
			if got != (Resolution{}) {
				t.Fatalf("Resolve returned %+v with an error, want the zero Resolution", got)
			}
		})
	}
}

// TestResolveNoFallback proves that an unavailable default executor fails
// the request instead of moving to another eligible executor
// (docs/architecture.md section 5).
func TestResolveNoFallback(t *testing.T) {
	routing := &registry.OperationRouting{
		Executors:       []string{"node-a", "node-b"},
		DefaultExecutor: "node-a",
	}
	nodes := fakeNodes{
		"node-a": offlineNode("node-a"),
		"node-b": onlineNode("node-b"),
	}
	r := New(fakeCapabilities{"pc": capability("down", routing)}, nodes)
	got, err := r.Resolve("pc", "down", "")
	if err == nil {
		t.Fatalf("Resolve returned %+v, want an error for the offline default", got)
	}
	if !errors.Is(err, herrors.ExecutorUnavailable) {
		t.Fatalf("Resolve error is %v, want code %s", err, herrors.ExecutorUnavailable.Name())
	}
	if got != (Resolution{}) {
		t.Fatalf("Resolve returned %+v, want the zero Resolution", got)
	}

	// The same request with the healthy executor named explicitly still
	// works, so the failure above came from the disabled default and not
	// from the routing.
	got, err = r.Resolve("pc", "down", "node-b")
	if err != nil {
		t.Fatalf("Resolve returned %v, want node-b", err)
	}
	if want := (Resolution{Executor: "node-b", Source: SourceExplicit}); got != want {
		t.Fatalf("Resolve returned %+v, want %+v", got, want)
	}
}

// TestResolveNodeRegistry proves resolution against the real node registry:
// the Gateway host node is online from construction, every other node
// starts offline, and a default that is not the host fails with
// ExecutorUnavailable.
func TestResolveNodeRegistry(t *testing.T) {
	in := registry.Input{
		Nodes: []registry.Node{
			{ID: "gateway", OS: "linux", Arch: "amd64", Enabled: true},
			{ID: "node-a", OS: "linux", Arch: "amd64", Enabled: true},
		},
		GatewayHost: "gateway",
	}
	pc := capability("down", &registry.OperationRouting{
		Executors:       []string{"gateway", "node-a"},
		DefaultExecutor: "gateway",
	})
	nodes, err := registry.NewNodeRegistry(in, []*registry.Capability{pc})
	if err != nil {
		t.Fatalf("NewNodeRegistry returned %v", err)
	}
	r := New(fakeCapabilities{"pc": pc}, nodes)
	got, err := r.Resolve("pc", "down", "")
	if err != nil {
		t.Fatalf("Resolve returned %v, want the Gateway host node", err)
	}
	if want := (Resolution{Executor: "gateway", Source: SourceDefault}); got != want {
		t.Fatalf("Resolve returned %+v, want %+v", got, want)
	}
	_, err = r.Resolve("pc", "down", "node-a")
	if !errors.Is(err, herrors.ExecutorUnavailable) {
		t.Fatalf("Resolve error is %v, want code %s for a node that starts offline",
			err, herrors.ExecutorUnavailable.Name())
	}
}

// TestResolveNilResolver keeps a partially built Gateway from panicking.
func TestResolveNilResolver(t *testing.T) {
	var r *Resolver
	if _, err := r.Resolve("pc", "down", "node-a"); !errors.Is(err, herrors.UnknownCapability) {
		t.Fatalf("nil Resolver error is %v, want code %s", err, herrors.UnknownCapability.Name())
	}
}
