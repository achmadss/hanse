package operations_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/achmadss/hanse/internal/config"
	herrors "github.com/achmadss/hanse/internal/errors"
	"github.com/achmadss/hanse/internal/operations"
	"github.com/achmadss/hanse/internal/registry"
	"github.com/achmadss/hanse/internal/runtime"
)

// nodesManifest is a capability whose `hanse:node` arguments have no node
// filter. Every operation takes the node in another shape: one top-level
// argument, an array, or a nested object.
const nodesManifest = `version: 1
id: shapes
name: Argument shapes
description: Refer to nodes in several argument shapes
contract: hanse.handler/v1

handler:
  kind: exec
  entry:
    linux: [sh, handler.sh]

operations:
  top:
    description: One top-level node argument
    mode: sync
    arguments:
      type: object
      required: [node]
      additionalProperties: false
      properties:
        node:
          type: string
          format: hanse:node

  list:
    description: An array of nodes
    mode: sync
    arguments:
      type: object
      required: [nodes]
      additionalProperties: false
      properties:
        nodes:
          type: array
          items:
            type: string
            format: hanse:node

  nested:
    description: A node inside a nested object
    mode: sync
    arguments:
      type: object
      required: [target]
      additionalProperties: false
      properties:
        target:
          type: object
          required: [node]
          additionalProperties: false
          properties:
            node:
              type: string
              format: hanse:node
`

// The shape fixture nodes: the Gateway host is the default executor, and
// node-b and node-c exist only as `hanse:node` targets.
const (
	shapesHost   = "gateway"
	shapesTarget = "node-b"
	shapesOther  = "node-c"
)

// newShapesFixture loads the shapes capability with three enabled nodes.
func newShapesFixture(t *testing.T) *registry.Registry {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "capabilities")
	if err := os.MkdirAll(filepath.Join(dir, "shapes"), 0o755); err != nil {
		t.Fatalf("create capability directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "shapes", "manifest.yaml"), []byte(nodesManifest), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "shapes", "handler.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o644); err != nil {
		t.Fatalf("write handler: %v", err)
	}
	reg, warnings, err := registry.Load(registry.Options{
		Dirs: []string{dir},
		Input: registry.Input{
			GatewayHost: shapesHost,
			Nodes: []registry.Node{
				{
					ID:      shapesHost,
					Name:    "Gateway host",
					OS:      "linux",
					Arch:    "amd64",
					Address: "192.0.2.10",
					Enabled: true,
				},
				{
					ID:      shapesTarget,
					Name:    "Target machine",
					OS:      "windows",
					Arch:    "amd64",
					Address: "192.0.2.20",
					Enabled: true,
					Attributes: map[string]string{
						"mac": "00:00:5e:00:53:01",
					},
				},
				{
					ID:      shapesOther,
					Name:    "Other machine",
					OS:      "linux",
					Arch:    "arm64",
					Enabled: true,
				},
			},
			Routes: registry.Routes{Capabilities: map[string]registry.CapabilityRoute{
				"shapes": {
					Executors:       []string{shapesHost},
					DefaultExecutor: shapesHost,
				},
			}},
		},
	})
	if err != nil {
		t.Fatalf("load fixture: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("load fixture: unexpected warnings: %+v", warnings)
	}
	return reg
}

// newShapesManager builds a manager over the shapes fixture.
func newShapesManager(t *testing.T, dispatcher operations.Dispatcher) *operations.Manager {
	t.Helper()
	manager, err := operations.New(operations.Config{
		Registry:   newShapesFixture(t),
		Dispatcher: dispatcher,
		Timeouts: config.Timeouts{
			Operation:   5 * time.Second,
			GracePeriod: time.Second,
		},
	})
	if err != nil {
		t.Fatalf("build manager: %v", err)
	}
	return manager
}

// TestEnvelopeNodesFromSchema checks the request envelope's `nodes` object
// when the operation has no node filters at all, handler contract section
// 2.1: a `hanse:node` value anywhere in the arguments brings its node.
func TestEnvelopeNodesFromSchema(t *testing.T) {
	cases := []struct {
		name string
		op   string
		args string
		want []string
	}{
		{
			name: "top-level argument",
			op:   "top",
			args: `{"node":"` + shapesTarget + `"}`,
			want: []string{shapesTarget},
		},
		{
			name: "array of nodes",
			op:   "list",
			args: `{"nodes":["` + shapesTarget + `","` + shapesOther + `"]}`,
			want: []string{shapesTarget, shapesOther},
		},
		{
			name: "nested object",
			op:   "nested",
			args: `{"target":{"node":"` + shapesOther + `"}}`,
			want: []string{shapesOther},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := newDispatcher()
			manager := newShapesManager(t, d)

			rec, err := manager.Submit(operations.Request{
				Capability: "shapes",
				Operation:  tc.op,
				Arguments:  json.RawMessage(tc.args),
				Requester:  "tester",
			})
			if err != nil {
				t.Fatalf("submit: %v", err)
			}
			if rec.Status != operations.StateCompleted {
				t.Fatalf("status %q, want completed", rec.Status)
			}
			call := <-d.started
			if call.Request == nil {
				t.Fatal("dispatch received no request envelope")
			}
			got := make([]string, 0, len(call.Request.Nodes))
			for id := range call.Request.Nodes {
				got = append(got, id)
			}
			sort.Strings(got)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("envelope nodes = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestEnvelopeNodeObject checks that a node reached through the schema is
// the registry's non-secret node object, handler contract section 2.1.
func TestEnvelopeNodeObject(t *testing.T) {
	d := newDispatcher()
	manager := newShapesManager(t, d)

	if _, err := manager.Submit(operations.Request{
		Capability: "shapes",
		Operation:  "top",
		Arguments:  json.RawMessage(`{"node":"` + shapesTarget + `"}`),
		Requester:  "tester",
	}); err != nil {
		t.Fatalf("submit: %v", err)
	}
	call := <-d.started
	want := runtime.Node{
		ID:         shapesTarget,
		Name:       "Target machine",
		OS:         "windows",
		Arch:       "amd64",
		Address:    "192.0.2.20",
		Attributes: map[string]any{"mac": "00:00:5e:00:53:01"},
	}
	got, ok := call.Request.Nodes[shapesTarget]
	if !ok {
		t.Fatalf("envelope nodes %v have no target %s", call.Request.Nodes, shapesTarget)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("target node %+v, want %+v", got, want)
	}
}

// TestUnknownNodeRejectedBeforeDispatch checks that validation still
// rejects a `hanse:node` value that names no node, even without a node
// filter, before any handler runs.
func TestUnknownNodeRejectedBeforeDispatch(t *testing.T) {
	d := newDispatcher()
	manager := newShapesManager(t, d)

	_, err := manager.Submit(operations.Request{
		Capability: "shapes",
		Operation:  "top",
		Arguments:  json.RawMessage(`{"node":"node-unknown"}`),
		Requester:  "tester",
	})
	if !errors.Is(err, herrors.InvalidArguments) {
		t.Fatalf("submit with an unknown node = %v, want invalid_arguments", err)
	}
	if got := d.callCount(); got != 0 {
		t.Errorf("dispatched %d calls, want none", got)
	}
	select {
	case call := <-d.started:
		t.Errorf("dispatched a call for an unknown node: %+v", call)
	default:
	}
}
