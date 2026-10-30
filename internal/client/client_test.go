package client

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	herrors "github.com/achmadss/hanse/internal/errors"
)

// fixtureVersion is the body of GET /v1/version.
const fixtureVersion = `{
  "gateway_version": "dev",
  "api_version": "v1",
  "handler_contracts": ["hanse.handler/v1"],
  "agent_protocols": ["hanse.agent/v1"]
}`

// fixtureCapabilities is the body of GET /v1/capabilities.
const fixtureCapabilities = `{
  "capabilities": [
    {
      "id": "pc",
      "name": "PC power",
      "description": "Wake a node",
      "contract": "hanse.handler/v1",
      "handler": {"kind": "exec"},
      "operations": [
        {"id": "up", "description": "Wake a node", "mode": "async", "privileged": false}
      ]
    }
  ]
}`

// fixtureCapability is the body of GET /v1/capabilities/{id}.
const fixtureCapability = `{
  "capability": {
    "id": "pc",
    "name": "PC power",
    "description": "Wake a node",
    "contract": "hanse.handler/v1",
    "handler": {"kind": "exec"},
    "operations": [
      {"id": "up", "description": "Wake a node", "mode": "async", "privileged": false}
    ]
  }
}`

// newTestServer starts a server that requires the test token and dispatches
// on the request path.
func newTestServer(t *testing.T, routes map[string]http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"code":"authentication_failed","message":"the token is wrong"}`))
			return
		}
		route, ok := routes[r.URL.Path]
		if !ok {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"code":"unknown_capability","message":"no such route"}`))
			return
		}
		route(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// jsonRoute serves one fixed body.
func jsonRoute(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}
}

// newTestClient returns a client for srv.
func newTestClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	c, err := New(Config{BaseURL: srv.URL, Token: "test-token"})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	return c
}

// TestDiscoveryMethods covers every discovery request.
func TestDiscoveryMethods(t *testing.T) {
	srv := newTestServer(t, map[string]http.HandlerFunc{
		"/v1/version":                       jsonRoute(fixtureVersion),
		"/v1/capabilities":                  jsonRoute(fixtureCapabilities),
		"/v1/capabilities/pc":               jsonRoute(fixtureCapability),
		"/v1/nodes":                         jsonRoute(`{"nodes":[{"id":"node-a","os":"linux","arch":"amd64","roles":["server"],"attributes":{},"enabled":true,"status":"online"}]}`),
		"/v1/nodes/node-a":                  jsonRoute(`{"node":{"id":"node-a","os":"linux","arch":"amd64","roles":["server"],"attributes":{},"enabled":true,"status":"online"}}`),
		"/v1/nodes/node-a/capabilities":     jsonRoute(`{"node":"node-a","capabilities":[{"id":"pc","operations":["up"]}]}`),
		"/v1/operations/pc/up/schema":       jsonRoute(`{"type":"object","properties":{"node":{"type":"string"}},"required":["node"]}`),
		"/v1/operations/pc/up/options/node": jsonRoute(`{"options":[{"value":"node-a"}]}`),
	})
	c := newTestClient(t, srv)

	v, err := c.Version(context.Background())
	if err != nil {
		t.Fatalf("version: %v", err)
	}
	if v.APIVersion != "v1" || v.GatewayVersion != "dev" {
		t.Errorf("version: got %+v", v)
	}

	caps, err := c.Capabilities(context.Background())
	if err != nil {
		t.Fatalf("capabilities: %v", err)
	}
	if len(caps.Capabilities) != 1 || caps.Capabilities[0].ID != "pc" {
		t.Errorf("capabilities: got %+v", caps)
	}
	if len(caps.Capabilities[0].Operations) != 1 || caps.Capabilities[0].Operations[0].ID != "up" {
		t.Errorf("capabilities operations: got %+v", caps.Capabilities[0].Operations)
	}

	capability, err := c.Capability(context.Background(), "pc")
	if err != nil {
		t.Fatalf("capability: %v", err)
	}
	if capability.ID != "pc" || capability.Handler.Kind != "exec" {
		t.Errorf("capability: got %+v", capability)
	}

	nodes, err := c.Nodes(context.Background())
	if err != nil {
		t.Fatalf("nodes: %v", err)
	}
	if len(nodes.Nodes) != 1 || nodes.Nodes[0].Status != "online" {
		t.Errorf("nodes: got %+v", nodes)
	}

	node, err := c.Node(context.Background(), "node-a")
	if err != nil {
		t.Fatalf("node: %v", err)
	}
	if node.ID != "node-a" || !node.Enabled {
		t.Errorf("node: got %+v", node)
	}

	nodeCaps, err := c.NodeCapabilities(context.Background(), "node-a")
	if err != nil {
		t.Fatalf("node capabilities: %v", err)
	}
	if nodeCaps.Node != "node-a" || len(nodeCaps.Capabilities) != 1 {
		t.Errorf("node capabilities: got %+v", nodeCaps)
	}

	schema, err := c.Schema(context.Background(), "pc", "up")
	if err != nil {
		t.Fatalf("schema: %v", err)
	}
	object, ok := schema.(map[string]any)
	if !ok {
		t.Fatalf("schema: got %T, want a JSON object", schema)
	}
	if _, ok := object["required"]; !ok {
		t.Errorf("schema: got %+v, want a required list", object)
	}

	options, err := c.Options(context.Background(), "pc", "up", "node")
	if err != nil {
		t.Fatalf("options: %v", err)
	}
	if len(options.Options) != 1 || options.Options[0].Value != "node-a" {
		t.Errorf("options: got %+v", options)
	}
}

// TestNullListsBecomeEmpty requires an empty slice rather than null, so a
// caller always sees the same JSON type.
func TestNullListsBecomeEmpty(t *testing.T) {
	srv := newTestServer(t, map[string]http.HandlerFunc{
		"/v1/capabilities": jsonRoute(`{"capabilities":null}`),
		"/v1/nodes":        jsonRoute(`{"nodes":null}`),
	})
	c := newTestClient(t, srv)

	caps, err := c.Capabilities(context.Background())
	if err != nil {
		t.Fatalf("capabilities: %v", err)
	}
	if caps.Capabilities == nil {
		t.Error("capabilities: got nil, want an empty slice")
	}
	nodes, err := c.Nodes(context.Background())
	if err != nil {
		t.Fatalf("nodes: %v", err)
	}
	if nodes.Nodes == nil {
		t.Error("nodes: got nil, want an empty slice")
	}
}

// TestErrorMapping maps Gateway error bodies to the shared codes and keeps
// their exit codes.
func TestErrorMapping(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		body    string
		code    *herrors.Code
		message string
	}{
		{
			name:    "documented code",
			status:  http.StatusNotFound,
			body:    `{"code":"unknown_capability","message":"capability \"nope\" does not exist","details":{"capability":"nope"}}`,
			code:    herrors.UnknownCapability,
			message: `capability "nope" does not exist`,
		},
		{
			name:    "authentication",
			status:  http.StatusUnauthorized,
			body:    `{"code":"authentication_failed","message":"the token is wrong"}`,
			code:    herrors.AuthenticationFailed,
			message: "the token is wrong",
		},
		{
			name:    "unknown code",
			status:  http.StatusTeapot,
			body:    `{"code":"something_new","message":"a new error"}`,
			code:    herrors.Internal,
			message: `a new error (unknown error code "something_new")`,
		},
		{
			name:    "plain body",
			status:  http.StatusInternalServerError,
			body:    "gateway exploded",
			code:    herrors.Internal,
			message: "HTTP 500: gateway exploded",
		},
		{
			name:    "empty body",
			status:  http.StatusBadGateway,
			body:    "",
			code:    herrors.Internal,
			message: "HTTP 502: Bad Gateway",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newTestServer(t, map[string]http.HandlerFunc{
				"/v1/version": func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(tc.status)
					_, _ = w.Write([]byte(tc.body))
				},
			})
			c := newTestClient(t, srv)
			_, err := c.Version(context.Background())
			if err == nil {
				t.Fatal("version: got no error")
			}
			if !errors.Is(err, tc.code) {
				t.Errorf("version: error %v does not match code %s", err, tc.code)
			}
			if got := herrors.ExitCode(err); got != tc.code.ExitCode() {
				t.Errorf("version: exit code %d, want %d", got, tc.code.ExitCode())
			}
			apiErr, ok := err.(*herrors.Error)
			if !ok {
				t.Fatalf("version: error type %T, want *herrors.Error", err)
			}
			if !strings.Contains(apiErr.Message, tc.message) {
				t.Errorf("version: message %q does not contain %q", apiErr.Message, tc.message)
			}
		})
	}
}

// TestInvalidJSON reports a malformed success body as an Internal error.
func TestInvalidJSON(t *testing.T) {
	srv := newTestServer(t, map[string]http.HandlerFunc{
		"/v1/version": jsonRoute(`{"api_version":`),
	})
	c := newTestClient(t, srv)
	_, err := c.Version(context.Background())
	if !errors.Is(err, herrors.Internal) {
		t.Fatalf("version: error %v, want the Internal code", err)
	}
	if !strings.Contains(err.Error(), "invalid JSON response") {
		t.Errorf("version: error %v, want an invalid JSON message", err)
	}
}

// TestUnreachable reports a network failure as an Internal error.
func TestUnreachable(t *testing.T) {
	srv := newTestServer(t, nil)
	url := srv.URL
	srv.Close()

	c, err := New(Config{BaseURL: url, Token: "test-token"})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	_, err = c.Version(context.Background())
	if !errors.Is(err, herrors.Internal) {
		t.Fatalf("version: error %v, want the Internal code", err)
	}
	if !strings.Contains(err.Error(), "cannot reach the Gateway") {
		t.Errorf("version: error %v, want an unreachable message", err)
	}
}

// TestSchemaKeepsNumbers requires json.Number, so a large integer keeps
// every digit when the CLI prints the schema.
func TestSchemaKeepsNumbers(t *testing.T) {
	srv := newTestServer(t, map[string]http.HandlerFunc{
		"/v1/operations/pc/up/schema": jsonRoute(`{"minimum":9007199254740993}`),
	})
	c := newTestClient(t, srv)
	schema, err := c.Schema(context.Background(), "pc", "up")
	if err != nil {
		t.Fatalf("schema: %v", err)
	}
	data, err := json.Marshal(schema)
	if err != nil {
		t.Fatalf("marshal schema: %v", err)
	}
	if !strings.Contains(string(data), "9007199254740993") {
		t.Errorf("schema: %s lost digits", data)
	}
}

// TestNewRejectsBadURLs covers the URL validation.
func TestNewRejectsBadURLs(t *testing.T) {
	cases := []string{"", "not a url", "ftp://hanse.example", "http:///v1", "https://user:secret@hanse.example"}
	for _, raw := range cases {
		t.Run(raw, func(t *testing.T) {
			if _, err := New(Config{BaseURL: raw}); err == nil {
				t.Errorf("New(%q): got no error", raw)
			}
		})
	}
}

// TestInvalidSegmentsRejected requires a hostile identifier to fail before
// any request, so it cannot reach another endpoint.
func TestInvalidSegmentsRejected(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fixtureVersion))
	}))
	t.Cleanup(srv.Close)
	c := newTestClient(t, srv)

	for _, id := range []string{"../version", "a/b", "..", "", "a\\b"} {
		_, err := c.Capability(context.Background(), id)
		if !errors.Is(err, herrors.InvalidArguments) {
			t.Errorf("Capability(%q): error %v, want InvalidArguments", id, err)
		}
	}
	if requests != 0 {
		t.Errorf("sent %d requests with a hostile identifier, want 0", requests)
	}
}

// TestTrailingDataRejected requires the whole body to be one JSON value.
func TestTrailingDataRejected(t *testing.T) {
	srv := newTestServer(t, map[string]http.HandlerFunc{
		"/v1/version": jsonRoute(`{"api_version":"v1"}{}`),
	})
	c := newTestClient(t, srv)
	_, err := c.Version(context.Background())
	if !errors.Is(err, herrors.Internal) {
		t.Fatalf("version: error %v, want the Internal code", err)
	}
	if !strings.Contains(err.Error(), "trailing data") {
		t.Errorf("version: error %v, want a trailing data message", err)
	}
}

// TestNodeAttributesBecomeEmpty requires a stable JSON type for nodes.
func TestNodeAttributesBecomeEmpty(t *testing.T) {
	srv := newTestServer(t, map[string]http.HandlerFunc{
		"/v1/nodes": jsonRoute(`{"nodes":[{"id":"node-a","os":"linux","arch":"amd64","roles":null,"attributes":null,"enabled":true,"status":"online"}]}`),
	})
	c := newTestClient(t, srv)
	nodes, err := c.Nodes(context.Background())
	if err != nil {
		t.Fatalf("nodes: %v", err)
	}
	if nodes.Nodes[0].Roles == nil {
		t.Error("nodes: roles are nil, want an empty slice")
	}
	if nodes.Nodes[0].Attributes == nil {
		t.Error("nodes: attributes are nil, want an empty map")
	}
}

// TestBaseURLNormalization trims a trailing slash and query.
func TestBaseURLNormalization(t *testing.T) {
	c, err := New(Config{BaseURL: "https://hanse.example/gateway/?x=1"})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	if got, want := c.BaseURL(), "https://hanse.example/gateway"; got != want {
		t.Errorf("BaseURL() = %q, want %q", got, want)
	}
}
