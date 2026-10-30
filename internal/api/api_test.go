package api

import (
	"bytes"
	"encoding/json"
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/achmadss/hanse/internal/registry"
	"github.com/achmadss/hanse/internal/runtime"
	"github.com/achmadss/hanse/internal/runtime/builtin/echotest"
)

// update rewrites the golden files instead of comparing against them.
var update = flag.Bool("update", false, "rewrite the testdata golden files")

// pcManifest is the fixture capability: an exec capability with node
// filters, a secret, an option provider, and a sync operation, following
// docs/configuration.md section 6.5.
const pcManifest = `version: 1
id: pc
name: PC power
description: Wake, shut down, and check machines from another node
contract: hanse.handler/v1

handler:
  kind: exec
  entry:
    linux: [sh, handler.sh]
    darwin: [sh, handler.sh]

secrets:
  ssh_key: Private key that can log in to target nodes and shut them down

operations:
  up:
    description: Wake a node with Wake-on-LAN and wait until it answers
    mode: async
    timeout: 3m
    concurrency:
      limit: 1
      key: [node]
    node_filters:
      node:
        roles: [pc]
        attributes: [mac, broadcast]
    arguments:
      type: object
      required: [node]
      additionalProperties: false
      properties:
        node:
          type: string
          format: hanse:node
          description: The node to wake
        wait:
          type: boolean
          description: Wait until the node answers

  down:
    description: Shut down a node over SSH and wait until it stops answering
    mode: async
    timeout: 2m
    secrets: [ssh_key]
    node_filters:
      node:
        roles: [pc]
        attributes: [ssh_user]
    arguments:
      type: object
      required: [node]
      additionalProperties: false
      properties:
        node:
          type: string
          format: hanse:node
          description: The node to shut down

  status:
    description: Report whether a node is up
    mode: sync
    timeout: 15s
    node_filters:
      node:
        roles: [pc]
    arguments:
      type: object
      required: [node]
      additionalProperties: false
      properties:
        node:
          type: string
          format: hanse:node
        probe:
          type: string
          description: How to check the node
    options:
      probe: provider
`

// demoManifest is the second fixture capability, so the list endpoints
// sort more than one entry.
const demoManifest = `version: 1
id: demo
name: Demo
description: Remove the demonstration file from a node
contract: hanse.handler/v1

handler:
  kind: exec
  entry:
    linux: [sh, handler.sh]

operations:
  clean:
    description: Remove the demonstration file
    arguments:
      type: object
      additionalProperties: false
      properties:
        path:
          type: string
          description: The file to remove
`

// writeCapability writes one capability directory: its manifest and one
// handler file, so the bundle hash has more than the manifest.
func writeCapability(t *testing.T, root, id, manifest, handler string) {
	t.Helper()
	dir := filepath.Join(root, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create capability directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, handler), []byte("#!/bin/sh\nexit 0\n"), 0o644); err != nil {
		t.Fatalf("write handler file: %v", err)
	}
}

// fixtureInput is the node and routing configuration of the tests. Every
// value is a placeholder: 192.0.2.0/24 and 00:00:5e:00:53:xx are reserved
// for documentation.
func fixtureInput() registry.Input {
	return registry.Input{
		GatewayHost: "gateway",
		Nodes: []registry.Node{
			{ID: "gateway", OS: "linux", Arch: "amd64", Roles: []string{"server"}, Enabled: true},
			{ID: "node-a", OS: "linux", Arch: "arm64", Roles: []string{"server"}, Enabled: true},
			{ID: "node-b", OS: "linux", Arch: "arm64", Roles: []string{"server"}, Enabled: true},
			{
				ID: "node-c", OS: "windows", Arch: "amd64",
				Roles:   []string{"pc"},
				Enabled: true,
				Attributes: map[string]string{
					"mac":       "00:00:5e:00:53:01",
					"broadcast": "192.0.2.255",
					"ssh_user":  "hanse",
				},
			},
			{
				ID: "node-d", OS: "linux", Arch: "amd64",
				Roles:   []string{"pc"},
				Enabled: false,
				Attributes: map[string]string{
					"mac":       "00:00:5e:00:53:02",
					"broadcast": "192.0.2.255",
					"ssh_user":  "hanse",
				},
			},
			{
				ID: "node-e", OS: "linux", Arch: "amd64",
				Roles:      []string{"pc"},
				Enabled:    true,
				Attributes: map[string]string{"mac": "00:00:5e:00:53:03"},
			},
		},
		Routes: registry.Routes{Capabilities: map[string]registry.CapabilityRoute{
			"pc": {
				Executors:       []string{"node-a", "node-b"},
				DefaultExecutor: "node-a",
			},
			"demo": {
				Wildcard:        true,
				DefaultExecutor: "gateway",
			},
		}},
	}
}

// newFixture loads the fixture registry from a temporary capability root.
func newFixture(t *testing.T) *registry.Registry {
	t.Helper()
	capsDir := filepath.Join(t.TempDir(), "capabilities")
	writeCapability(t, capsDir, "pc", pcManifest, "handler.sh")
	writeCapability(t, capsDir, "demo", demoManifest, "handler.sh")
	reg, warnings, err := registry.Load(registry.Options{
		Dirs:  []string{capsDir},
		Input: fixtureInput(),
	})
	if err != nil {
		t.Fatalf("load fixture registry: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("load fixture registry: unexpected warnings: %+v", warnings)
	}
	return reg
}

// get performs one GET request against the handler and returns the
// response and its body.
func get(t *testing.T, handler http.Handler, path string) (*http.Response, []byte) {
	t.Helper()
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec.Result(), rec.Body.Bytes()
}

// checkGolden compares got with testdata/<name>.golden, or rewrites the
// file under -update. Line endings are normalized so a Windows checkout
// with CRLF files compares equal to the LF JSON the handler writes.
func checkGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name+".golden")
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("create testdata directory: %v", err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s (run go test -run TestGolden -update to create it): %v", path, err)
	}
	want = bytes.ReplaceAll(want, []byte("\r\n"), []byte("\n"))
	if !bytes.Equal(want, got) {
		t.Errorf("%s differs from the response.\n--- want ---\n%s\n--- got ---\n%s", path, want, got)
	}
}

// TestGolden covers every discovery endpoint.
func TestGolden(t *testing.T) {
	handler := New(newFixture(t), nil)
	cases := []struct {
		name string
		path string
	}{
		{"version", "/v1/version"},
		{"capabilities", "/v1/capabilities"},
		{"capability", "/v1/capabilities/pc"},
		{"nodes", "/v1/nodes"},
		{"node", "/v1/nodes/node-c"},
		{"node_capabilities", "/v1/nodes/node-a/capabilities"},
		{"schema", "/v1/operations/pc/up/schema"},
		{"describe", "/v1/operations/pc/up/describe"},
		{"options", "/v1/operations/pc/up/options/node"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := get(t, handler, tc.path)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("GET %s: status %d, body %s", tc.path, resp.StatusCode, body)
			}
			if got := resp.Header.Get("Content-Type"); got != "application/json" {
				t.Errorf("GET %s: Content-Type %q, want application/json", tc.path, got)
			}
			checkGolden(t, tc.name, body)
		})
	}
}

// TestDeterministicResponses fetches every endpoint twice and requires
// byte-identical bodies, so a client can compare and cache responses.
func TestDeterministicResponses(t *testing.T) {
	handler := New(newFixture(t), nil)
	paths := []string{
		"/v1/capabilities",
		"/v1/nodes",
		"/v1/nodes/node-a/capabilities",
		"/v1/operations/pc/up/schema",
		"/v1/operations/pc/up/describe",
		"/v1/operations/pc/up/options/node",
	}
	for _, path := range paths {
		firstResp, first := get(t, handler, path)
		secondResp, second := get(t, handler, path)
		if firstResp.StatusCode != http.StatusOK || secondResp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: status %d and %d", path, firstResp.StatusCode, secondResp.StatusCode)
		}
		if !bytes.Equal(first, second) {
			t.Errorf("GET %s: two responses differ\n--- first ---\n%s\n--- second ---\n%s", path, first, second)
		}
	}
}

// wireError is the error object shape of the docs.
type wireError struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details"`
}

// TestErrors covers the documented codes of the discovery routes.
func TestErrors(t *testing.T) {
	handler := New(newFixture(t), nil)
	cases := []struct {
		name   string
		path   string
		status int
		code   string
	}{
		{"unknown capability", "/v1/capabilities/nope", http.StatusNotFound, "unknown_capability"},
		{"unknown capability of operation", "/v1/operations/nope/up/schema", http.StatusNotFound, "unknown_capability"},
		{"unknown operation", "/v1/operations/pc/nope/describe", http.StatusNotFound, "unknown_operation"},
		{"unknown operation options", "/v1/operations/pc/nope/options/node", http.StatusNotFound, "unknown_operation"},
		{"unknown node", "/v1/nodes/nope", http.StatusNotFound, "invalid_executor"},
		{"unknown node capabilities", "/v1/nodes/nope/capabilities", http.StatusNotFound, "invalid_executor"},
		{"unknown option field", "/v1/operations/pc/status/options/nope", http.StatusUnprocessableEntity, "invalid_arguments"},
		{"no provider for field", "/v1/operations/pc/status/options/probe", http.StatusUnprocessableEntity, "invalid_arguments"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := get(t, handler, tc.path)
			if resp.StatusCode != tc.status {
				t.Fatalf("GET %s: status %d, want %d, body %s", tc.path, resp.StatusCode, tc.status, body)
			}
			var got wireError
			if err := json.Unmarshal(body, &got); err != nil {
				t.Fatalf("GET %s: body is not a JSON error: %v (%s)", tc.path, err, body)
			}
			if got.Code != tc.code {
				t.Errorf("GET %s: code %q, want %q (body %s)", tc.path, got.Code, tc.code, body)
			}
			if got.Message == "" {
				t.Errorf("GET %s: the error has no message", tc.path)
			}
		})
	}
}

// TestMethodNotAllowed rejects every method but GET.
func TestMethodNotAllowed(t *testing.T) {
	handler := New(newFixture(t), nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/version", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /v1/version: status %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
}

// TestNoRegistry reports the Internal code instead of panicking when the
// Gateway has no loaded registry.
func TestNoRegistry(t *testing.T) {
	handler := New(nil, nil)
	for _, path := range []string{"/v1/capabilities", "/v1/nodes", "/v1/capabilities/pc"} {
		resp, body := get(t, handler, path)
		if resp.StatusCode != http.StatusInternalServerError {
			t.Fatalf("GET %s: status %d, want %d", path, resp.StatusCode, http.StatusInternalServerError)
		}
		var got wireError
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("GET %s: body is not a JSON error: %v (%s)", path, err, body)
		}
		if got.Code != "internal" {
			t.Errorf("GET %s: code %q, want internal", path, got.Code)
		}
	}
}

// TestNodeOptions covers the node filter semantics of the options
// endpoint (docs/configuration.md section 6.4).
func TestNodeOptions(t *testing.T) {
	handler := New(newFixture(t), nil)
	cases := []struct {
		name string
		path string
		want []string
	}{
		{
			// roles pc and the mac and broadcast attributes: node-d is
			// disabled, node-e has no broadcast.
			name: "roles and attributes",
			path: "/v1/operations/pc/up/options/node",
			want: []string{"node-c"},
		},
		{
			// roles pc only, sorted by node ID.
			name: "roles only",
			path: "/v1/operations/pc/status/options/node",
			want: []string{"node-c", "node-e"},
		},
		{
			// roles pc and the ssh_user attribute.
			name: "roles and ssh attribute",
			path: "/v1/operations/pc/down/options/node",
			want: []string{"node-c"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := get(t, handler, tc.path)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("GET %s: status %d, body %s", tc.path, resp.StatusCode, body)
			}
			var got optionsResponse
			if err := json.Unmarshal(body, &got); err != nil {
				t.Fatalf("GET %s: body is not options: %v (%s)", tc.path, err, body)
			}
			values := make([]string, 0, len(got.Options))
			for _, option := range got.Options {
				values = append(values, option.Value)
			}
			if !equalStrings(values, tc.want) {
				t.Errorf("GET %s: options %v, want %v", tc.path, values, tc.want)
			}
		})
	}
}

// TestNodeWithoutCapabilities covers a node that only exists as a target.
func TestNodeWithoutCapabilities(t *testing.T) {
	handler := New(newFixture(t), nil)
	resp, body := get(t, handler, "/v1/nodes/node-c/capabilities")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, body %s", resp.StatusCode, body)
	}
	var got nodeCapabilitiesResponse
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("body is not node capabilities: %v (%s)", err, body)
	}
	if got.Node != "node-c" || len(got.Capabilities) != 0 {
		t.Errorf("node-c capabilities %+v, want none", got)
	}
	if !bytes.Contains(body, []byte(`"capabilities": []`)) {
		t.Errorf("empty capabilities must be an empty list, got %s", body)
	}
}

// TestBuiltinCapability covers a capability built in memory, which carries
// no manifest value: the discovery routes must serve it without failing.
func TestBuiltinCapability(t *testing.T) {
	reg := newFixture(t)
	handlers := runtime.NewRegistry()
	if err := echotest.Register(handlers, reg, echotest.Options{GatewayHost: "gateway"}); err != nil {
		t.Fatalf("register echo: %v", err)
	}
	handler := New(reg, nil)

	resp, body := get(t, handler, "/v1/capabilities/echo")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET echo: status %d, body %s", resp.StatusCode, body)
	}
	var capability capabilityResponse
	if err := json.Unmarshal(body, &capability); err != nil {
		t.Fatalf("echo body is not a capability: %v (%s)", err, body)
	}
	if capability.Capability.ID != "echo" || len(capability.Capability.Operations) != 2 {
		t.Errorf("echo capability %+v, want its two operations", capability.Capability)
	}

	resp, body = get(t, handler, "/v1/operations/echo/say/describe")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET echo.say describe: status %d, body %s", resp.StatusCode, body)
	}
	var described describeResponse
	if err := json.Unmarshal(body, &described); err != nil {
		t.Fatalf("echo describe body: %v (%s)", err, body)
	}
	if !equalStrings(described.Required, []string{"message"}) {
		t.Errorf("echo.say required %v, want [message]", described.Required)
	}
	var describedWire struct {
		Schema struct {
			Required []string `json:"required"`
		} `json:"schema"`
	}
	if err := json.Unmarshal(body, &describedWire); err != nil {
		t.Fatalf("echo describe body: %v (%s)", err, body)
	}
	if !equalStrings(describedWire.Schema.Required, []string{"message"}) {
		t.Errorf("echo.say describe schema required %v, want [message]", describedWire.Schema.Required)
	}
	if len(described.Executors) != 1 || described.Executors[0].ID != "gateway" || !described.Executors[0].Default {
		t.Errorf("echo.say executors %+v, want the Gateway host as default", described.Executors)
	}

	// The schema of an in-memory builtin capability comes from its manifest
	// operation spec, so clients see the required arguments.
	resp, body = get(t, handler, "/v1/operations/echo/say/schema")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET echo.say schema: status %d, body %s", resp.StatusCode, body)
	}
	var schemaObject struct {
		Required   []string                   `json:"required"`
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(body, &schemaObject); err != nil {
		t.Fatalf("echo.say schema body: %v (%s)", err, body)
	}
	if !equalStrings(schemaObject.Required, []string{"message"}) {
		t.Errorf("echo.say schema required %v, want [message]", schemaObject.Required)
	}
	if _, ok := schemaObject.Properties["message"]; !ok {
		t.Errorf("echo.say schema has no message property: %s", body)
	}
	checkGolden(t, "builtin_schema", body)
}

// TestMissingRawSchema reports the Internal code instead of serving an
// empty schema that accepts arguments the Gateway rejects.
func TestMissingRawSchema(t *testing.T) {
	reg := newFixture(t)
	c, ok := reg.Capability("pc")
	if !ok {
		t.Fatal("the fixture has no pc capability")
	}
	delete(c.Manifest.Operations, "up")
	handler := New(reg, nil)
	for _, path := range []string{
		"/v1/operations/pc/up/schema",
		"/v1/operations/pc/up/describe",
	} {
		resp, body := get(t, handler, path)
		if resp.StatusCode != http.StatusInternalServerError {
			t.Fatalf("GET %s: status %d, body %s", path, resp.StatusCode, body)
		}
		var got wireError
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("GET %s: body is not a JSON error: %v (%s)", path, err, body)
		}
		if got.Code != "internal" {
			t.Errorf("GET %s: code %q, want internal", path, got.Code)
		}
	}
}

// equalStrings reports whether two string slices hold the same values in
// the same order.
func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
