package validation_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	herrors "github.com/achmadss/hanse/internal/errors"
	"github.com/achmadss/hanse/internal/registry"
	"github.com/achmadss/hanse/internal/validation"
)

// testManifest is one capability whose operations cover the argument rules
// of docs/configuration.md sections 6.3 and 6.4.
const testManifest = `
version: 1
id: demo
name: Demo
description: Exercise argument validation
contract: hanse.handler/v1

handler:
  kind: exec
  entry:
    linux: [sh, handler.sh]

operations:
  run:
    description: Full schema with a roles filter
    node_filters:
      node:
        roles: [pc]
    arguments:
      type: object
      required: [node, count, mode]
      additionalProperties: false
      properties:
        node: {type: string, format: "hanse:node"}
        count: {type: integer, minimum: 1}
        mode: {type: string, enum: [fast, slow]}
        target:
          type: object
          additionalProperties: false
          properties:
            node: {type: string, format: "hanse:node"}
        nodes:
          type: array
          items: {type: string, format: "hanse:node"}
        chosen: {$ref: "#/$defs/node"}
        linked:
          allOf:
            - {type: string, format: "hanse:node"}
      $defs:
        node: {type: string, format: "hanse:node"}
  by-os:
    description: OS filter
    node_filters:
      node:
        os: [linux]
    arguments:
      type: object
      required: [node]
      additionalProperties: false
      properties:
        node: {type: string, format: "hanse:node"}
  by-arch:
    description: Architecture filter
    node_filters:
      node:
        arch: [arm64]
    arguments:
      type: object
      required: [node]
      additionalProperties: false
      properties:
        node: {type: string, format: "hanse:node"}
  by-attr:
    description: Attribute filter
    node_filters:
      node:
        attributes: [mac, broadcast]
    arguments:
      type: object
      required: [node]
      additionalProperties: false
      properties:
        node: {type: string, format: "hanse:node"}
  bare:
    description: No filter
    arguments:
      type: object
      required: [node]
      additionalProperties: false
      properties:
        node: {type: string, format: "hanse:node"}
  ping:
    description: No arguments
    arguments:
      type: object
      additionalProperties: false
`

// testNodes is a placeholder node set: a Gateway host, an arm64 server, an
// offline windows target, a disabled node, and a target with one of the two
// filtered attributes.
func testNodes() []registry.Node {
	return []registry.Node{
		{ID: "gateway", OS: "linux", Arch: "amd64", Roles: []string{"server"}, Enabled: true},
		{ID: "node-a", OS: "linux", Arch: "arm64", Roles: []string{"server"}, Enabled: true},
		{
			ID: "node-c", OS: "windows", Arch: "amd64", Roles: []string{"pc"},
			Attributes: map[string]string{"mac": "00:00:5e:00:53:01", "broadcast": "192.0.2.255"},
			Enabled:    true,
		},
		{ID: "node-d", OS: "linux", Arch: "amd64", Roles: []string{"pc"}, Enabled: false},
		{
			ID: "node-e", OS: "linux", Arch: "amd64", Roles: []string{"pc"},
			Attributes: map[string]string{"mac": "00:00:5e:00:53:02"},
			Enabled:    true,
		},
	}
}

// newRegistry loads the test manifest into a registry with the test nodes.
func newRegistry(t *testing.T) *registry.Registry {
	t.Helper()
	root := t.TempDir()
	capDir := filepath.Join(root, "caps", "demo")
	if err := os.MkdirAll(capDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(capDir, "manifest.yaml"), []byte(testManifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(capDir, "handler.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	reg, _, err := registry.Load(registry.Options{
		Dirs: []string{filepath.Join(root, "caps")},
		Input: registry.Input{
			Nodes:       testNodes(),
			GatewayHost: "gateway",
			Routes: registry.Routes{Capabilities: map[string]registry.CapabilityRoute{
				"demo": {Wildcard: true},
			}},
		},
	})
	if err != nil {
		t.Fatalf("load registry: %v", err)
	}
	return reg
}

// operation returns one loaded operation.
func operation(t *testing.T, reg *registry.Registry, id string) *registry.Operation {
	t.Helper()
	capability, ok := reg.Capability("demo")
	if !ok {
		t.Fatal("capability demo is not loaded")
	}
	op, ok := capability.Operation(id)
	if !ok {
		t.Fatalf("operation %q is not loaded", id)
	}
	return op
}

// wireError is the encoded error shape of docs/architecture.md section 14.
type wireError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Details struct {
		Errors []validation.Problem `json:"errors"`
	} `json:"details"`
}

// decodeError asserts the invalid_arguments code and returns the wire form.
func decodeError(t *testing.T, err error) wireError {
	t.Helper()
	if err == nil {
		t.Fatal("Validate returned no error")
	}
	if !errors.Is(err, herrors.InvalidArguments) {
		t.Fatalf("Validate error = %v, want the invalid_arguments code", err)
	}
	if got := herrors.HTTPStatus(err); got != 422 {
		t.Fatalf("HTTPStatus = %d, want 422", got)
	}
	blob, marshalErr := herrors.JSONBytes(err)
	if marshalErr != nil {
		t.Fatalf("encode error: %v", marshalErr)
	}
	var decoded wireError
	if unmarshalErr := json.Unmarshal(blob, &decoded); unmarshalErr != nil {
		t.Fatalf("decode error JSON %s: %v", blob, unmarshalErr)
	}
	if decoded.Code != "invalid_arguments" {
		t.Fatalf("error code = %q, want invalid_arguments", decoded.Code)
	}
	return decoded
}

// assertProblems compares the reported problems with the expected ones.
func assertProblems(t *testing.T, got, want []validation.Problem) {
	t.Helper()
	if len(got) == 0 && len(want) == 0 {
		return
	}
	if !reflect.DeepEqual(got, want) {
		gotJSON, _ := json.Marshal(got)
		wantJSON, _ := json.Marshal(want)
		t.Fatalf("problems = %s, want %s", gotJSON, wantJSON)
	}
}

func TestValidateSchemaProblems(t *testing.T) {
	reg := newRegistry(t)
	run := operation(t, reg, "run")

	cases := []struct {
		name string
		op   *registry.Operation
		args string
		want []validation.Problem
	}{
		{
			name: "missing required fields",
			op:   run,
			args: `{"node":"node-c"}`,
			want: []validation.Problem{
				{Path: "/count", Rule: "required", Message: `missing required argument "count"`},
				{Path: "/mode", Rule: "required", Message: `missing required argument "mode"`},
			},
		},
		{
			name: "wrong type",
			op:   run,
			args: `{"node":"node-c","count":"many","mode":"fast"}`,
			want: []validation.Problem{
				{Path: "/count", Rule: "type", Message: "must be of type integer, got string"},
			},
		},
		{
			name: "below the minimum",
			op:   run,
			args: `{"node":"node-c","count":0,"mode":"fast"}`,
			want: []validation.Problem{
				{Path: "/count", Rule: "minimum", Message: "must be at least 1"},
			},
		},
		{
			name: "bad enum value",
			op:   run,
			args: `{"node":"node-c","count":2,"mode":"turbo"}`,
			want: []validation.Problem{
				{Path: "/mode", Rule: "enum", Message: "must be one of the allowed values"},
			},
		},
		{
			name: "unknown field",
			op:   run,
			args: `{"node":"node-c","count":2,"mode":"fast","extra":true}`,
			want: []validation.Problem{
				{Path: "/extra", Rule: "additionalProperties", Message: `unknown argument "extra"`},
			},
		},
		{
			name: "unknown field in a nested object",
			op:   run,
			args: `{"node":"node-c","count":2,"mode":"fast","target":{"node":"node-c","extra":1}}`,
			want: []validation.Problem{
				{Path: "/target/extra", Rule: "additionalProperties", Message: `unknown argument "extra"`},
			},
		},
		{
			name: "arguments are not an object",
			op:   run,
			args: `[]`,
			want: []validation.Problem{
				{Path: "", Rule: "type", Message: "must be of type object, got array"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validation.Validate(tc.op, reg.Nodes, json.RawMessage(tc.args))
			decoded := decodeError(t, err)
			assertProblems(t, decoded.Details.Errors, tc.want)
		})
	}
}

func TestValidateArgumentsNotJSON(t *testing.T) {
	reg := newRegistry(t)
	for _, args := range []string{`{"node":`, `{"node":"node-c"} trailing`} {
		decoded := decodeError(t, validation.Validate(operation(t, reg, "bare"), reg.Nodes, json.RawMessage(args)))
		want := []validation.Problem{{Path: "", Rule: "json", Message: "the request body is not a JSON value"}}
		assertProblems(t, decoded.Details.Errors, want)
	}
}

func TestValidateNodeProblems(t *testing.T) {
	reg := newRegistry(t)
	base := `"count":2,"mode":"fast"`

	cases := []struct {
		name string
		op   string
		args string
		want []validation.Problem
	}{
		{
			name: "unknown node",
			op:   "bare",
			args: `{"node":"node-x"}`,
			want: []validation.Problem{
				{Path: "/node", Rule: "format", Message: "must name a known, enabled node"},
			},
		},
		{
			name: "disabled node",
			op:   "bare",
			args: `{"node":"node-d"}`,
			want: []validation.Problem{
				{Path: "/node", Rule: "format", Message: "must name an enabled node"},
			},
		},
		{
			name: "offline target node passes",
			op:   "bare",
			args: `{"node":"node-c"}`,
			want: nil,
		},
		{
			name: "nested object",
			op:   "run",
			args: `{"node":"node-c",` + base + `,"target":{"node":"node-x"}}`,
			want: []validation.Problem{
				{Path: "/target/node", Rule: "format", Message: "must name a known, enabled node"},
			},
		},
		{
			name: "array item",
			op:   "run",
			args: `{"node":"node-c",` + base + `,"nodes":["node-c","node-x"]}`,
			want: []validation.Problem{
				{Path: "/nodes/1", Rule: "format", Message: "must name a known, enabled node"},
			},
		},
		{
			name: "referenced schema",
			op:   "run",
			args: `{"node":"node-c",` + base + `,"chosen":"node-x"}`,
			want: []validation.Problem{
				{Path: "/chosen", Rule: "format", Message: "must name a known, enabled node"},
			},
		},
		{
			name: "roles filter rejects a node without the role",
			op:   "run",
			args: `{"node":"node-a",` + base + `}`,
			want: []validation.Problem{
				{Path: "/node", Rule: "format", Message: "must name a node that passes the argument's roles filter"},
			},
		},
		{
			name: "os filter rejects another OS",
			op:   "by-os",
			args: `{"node":"node-c"}`,
			want: []validation.Problem{
				{Path: "/node", Rule: "format", Message: "must name a node that passes the argument's os filter"},
			},
		},
		{
			name: "os filter accepts a matching OS",
			op:   "by-os",
			args: `{"node":"node-a"}`,
			want: nil,
		},
		{
			name: "arch filter rejects another architecture",
			op:   "by-arch",
			args: `{"node":"node-c"}`,
			want: []validation.Problem{
				{Path: "/node", Rule: "format", Message: "must name a node that passes the argument's arch filter"},
			},
		},
		{
			name: "arch filter accepts a matching architecture",
			op:   "by-arch",
			args: `{"node":"node-a"}`,
			want: nil,
		},
		{
			name: "attributes filter requires every attribute",
			op:   "by-attr",
			args: `{"node":"node-e"}`,
			want: []validation.Problem{
				{Path: "/node", Rule: "format", Message: "must name a node that passes the argument's attributes filter"},
			},
		},
		{
			name: "attributes filter accepts a node with every attribute",
			op:   "by-attr",
			args: `{"node":"node-c"}`,
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validation.Validate(operation(t, reg, tc.op), reg.Nodes, json.RawMessage(tc.args))
			if len(tc.want) == 0 {
				if err != nil {
					t.Fatalf("Validate returned %v, want nil", err)
				}
				return
			}
			decoded := decodeError(t, err)
			assertProblems(t, decoded.Details.Errors, tc.want)
		})
	}
}

func TestValidateOfflineTargetNode(t *testing.T) {
	reg := newRegistry(t)
	status, ok := reg.Nodes.Status("node-c")
	if !ok {
		t.Fatal("node-c is not in the node registry")
	}
	if status != registry.StatusOffline {
		t.Fatalf("node-c status = %q, want offline", status)
	}
	if !reg.Nodes.Enabled("node-c") {
		t.Fatal("node-c is not enabled")
	}
	if err := validation.Validate(operation(t, reg, "bare"), reg.Nodes, json.RawMessage(`{"node":"node-c"}`)); err != nil {
		t.Fatalf("Validate(offline target node) = %v, want nil", err)
	}
}

func TestValidateNoArguments(t *testing.T) {
	reg := newRegistry(t)
	ping := operation(t, reg, "ping")
	if err := validation.Validate(ping, reg.Nodes, nil); err != nil {
		t.Fatalf("Validate(nil arguments) = %v, want nil", err)
	}
	if err := validation.Validate(ping, reg.Nodes, json.RawMessage(`  `)); err != nil {
		t.Fatalf("Validate(blank arguments) = %v, want nil", err)
	}
	decoded := decodeError(t, validation.Validate(operation(t, reg, "bare"), reg.Nodes, nil))
	assertProblems(t, decoded.Details.Errors, []validation.Problem{
		{Path: "/node", Rule: "required", Message: `missing required argument "node"`},
	})
}

func TestValidateNilNodeRegistry(t *testing.T) {
	reg := newRegistry(t)
	decoded := decodeError(t, validation.Validate(operation(t, reg, "bare"), nil, json.RawMessage(`{"node":"node-c"}`)))
	assertProblems(t, decoded.Details.Errors, []validation.Problem{
		{Path: "/node", Rule: "format", Message: "must name a known, enabled node"},
	})
}

func TestValidateMissingSchema(t *testing.T) {
	if err := validation.Validate(nil, nil, nil); !errors.Is(err, herrors.Internal) {
		t.Fatalf("Validate(nil operation) = %v, want the internal code", err)
	}
	if err := validation.Validate(&registry.Operation{ID: "demo.run"}, nil, nil); !errors.Is(err, herrors.Internal) {
		t.Fatalf("Validate(no schema) = %v, want the internal code", err)
	}
}

func TestValidateDoesNotEchoValues(t *testing.T) {
	reg := newRegistry(t)
	const secret = "s3cr3t-node-value"
	err := validation.Validate(operation(t, reg, "bare"), reg.Nodes,
		json.RawMessage(`{"node":"`+secret+`"}`))
	blob, marshalErr := herrors.JSONBytes(err)
	if marshalErr != nil {
		t.Fatalf("encode error: %v", marshalErr)
	}
	if strings.Contains(string(blob), secret) {
		t.Fatalf("error JSON repeats the argument value: %s", blob)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error message repeats the argument value: %v", err)
	}
}

func TestMatchNode(t *testing.T) {
	node := registry.NodeView{
		Node: registry.Node{
			ID:         "node-c",
			OS:         "windows",
			Arch:       "amd64",
			Roles:      []string{"pc", "laptop"},
			Attributes: map[string]string{"mac": "00:00:5e:00:53:03", "zone": ""},
		},
		Status: registry.StatusOffline,
	}
	cases := []struct {
		name   string
		filter registry.NodeFilter
		want   bool
	}{
		{name: "empty filter accepts every node", filter: registry.NodeFilter{}, want: true},
		{name: "os match", filter: registry.NodeFilter{OS: []string{"linux", "windows"}}, want: true},
		{name: "os miss", filter: registry.NodeFilter{OS: []string{"linux", "darwin"}}, want: false},
		{name: "arch match", filter: registry.NodeFilter{Arch: []string{"amd64"}}, want: true},
		{name: "arch miss", filter: registry.NodeFilter{Arch: []string{"arm64"}}, want: false},
		{name: "roles need one match", filter: registry.NodeFilter{Roles: []string{"server", "laptop"}}, want: true},
		{name: "roles with no match", filter: registry.NodeFilter{Roles: []string{"server"}}, want: false},
		{name: "attributes with an empty value count", filter: registry.NodeFilter{Attributes: []string{"mac", "zone"}}, want: true},
		{name: "attributes need all names", filter: registry.NodeFilter{Attributes: []string{"mac", "broadcast"}}, want: false},
		{
			name: "every field must pass",
			filter: registry.NodeFilter{
				OS: []string{"windows"}, Arch: []string{"amd64"},
				Roles: []string{"pc"}, Attributes: []string{"mac"},
			},
			want: true,
		},
		{
			name: "one failing field rejects the node",
			filter: registry.NodeFilter{
				OS: []string{"windows"}, Roles: []string{"server"},
			},
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := validation.MatchNode(node, tc.filter); got != tc.want {
				t.Fatalf("MatchNode(%s) = %t, want %t", tc.name, got, tc.want)
			}
		})
	}
}

func TestValidateValidArguments(t *testing.T) {
	reg := newRegistry(t)
	args := `{"node":"node-c","count":3,"mode":"slow","target":{"node":"node-e"},"nodes":["node-a"],"chosen":"gateway"}`
	if err := validation.Validate(operation(t, reg, "run"), reg.Nodes, json.RawMessage(args)); err != nil {
		t.Fatalf("Validate(valid arguments) = %v, want nil", err)
	}
}

// TestNodeIDs checks the collection of `hanse:node` values for the request
// envelope. The walk follows the schema and the actual arguments, so a
// value counts wherever the schema puts it: a top-level argument, an array
// item, a nested object property, a `$ref`, or an `allOf` branch.
func TestNodeIDs(t *testing.T) {
	reg := newRegistry(t)
	op := operation(t, reg, "run")
	args := `{"node":"gateway","count":1,"mode":"fast",` +
		`"target":{"node":"node-a"},"nodes":["node-c","node-c"],` +
		`"chosen":"node-e","linked":"node-c"}`
	got := validation.NodeIDs(op, json.RawMessage(args))
	want := []string{"gateway", "node-a", "node-c", "node-e"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("NodeIDs = %v, want %v", got, want)
	}
}

// TestNodeIDsWithoutValues checks the empty results: no operation, no
// schema, no arguments, and arguments that name no node.
func TestNodeIDsWithoutValues(t *testing.T) {
	reg := newRegistry(t)
	op := operation(t, reg, "run")
	cases := []struct {
		name string
		op   *registry.Operation
		args string
	}{
		{name: "no operation"},
		{name: "no schema", op: &registry.Operation{ID: "demo.run"}},
		{name: "no arguments", op: op},
		{name: "no node values", op: op, args: `{"count":2,"mode":"fast"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := validation.NodeIDs(tc.op, json.RawMessage(tc.args)); len(got) != 0 {
				t.Fatalf("NodeIDs = %v, want none", got)
			}
		})
	}
}
