package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/achmadss/hanse/internal/agentconn"
	"github.com/achmadss/hanse/internal/agentproto"
	"github.com/achmadss/hanse/internal/auth"
	"github.com/achmadss/hanse/internal/config"
	herrors "github.com/achmadss/hanse/internal/errors"
	"github.com/achmadss/hanse/internal/operations"
	"github.com/achmadss/hanse/internal/registry"
	"github.com/achmadss/hanse/internal/runtime"
)

// startTestGateway starts a Gateway on a loopback port the operating system
// chooses, and stops it when the test ends.
func startTestGateway(t *testing.T) *gateway {
	t.Helper()
	dir := configFixture(t, "")
	ctx, cancel := context.WithCancel(context.Background())
	g, err := startGateway(ctx, filepath.Join(dir, "gateway.yaml"), startOptions{
		listen: "127.0.0.1:0",
		stderr: io.Discard,
	})
	if err != nil {
		cancel()
		t.Fatalf("start the Gateway: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-g.Done():
			if err != nil {
				t.Errorf("shutdown: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("the Gateway did not stop")
		}
	})
	return g
}

// probeURL performs one unauthenticated GET request and returns the status
// and body.
func probeURL(t *testing.T, url string) (int, string) {
	t.Helper()
	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	return response.StatusCode, string(body)
}

// TestStartGatewayRefusesInvalidConfig requires every configuration problem
// to stop the server before it opens a listener, exactly as --check-config
// reports it.
func TestStartGatewayRefusesInvalidConfig(t *testing.T) {
	dir := configFixture(t, "timeuouts:\n  operation: 5m\n")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	g, err := startGateway(ctx, filepath.Join(dir, "gateway.yaml"), startOptions{
		listen: "127.0.0.1:0",
		stderr: io.Discard,
	})
	if err == nil {
		cancel()
		if err := <-g.Done(); err != nil {
			t.Fatalf("shutdown: %v", err)
		}
		t.Fatal("the Gateway started with an invalid configuration")
	}
	if !strings.Contains(err.Error(), "configuration problem(s)") {
		t.Errorf("error = %q, want it to name the configuration problems", err)
	}
	if !strings.Contains(err.Error(), filepath.Join(dir, "gateway.yaml")) {
		t.Errorf("error = %q, want it to name the configuration file", err)
	}
	if !strings.Contains(err.Error(), "field timeuouts not found") {
		t.Errorf("error = %q, want the unknown-field problem", err)
	}
}

// TestStartGatewayRefusesNonLoopbackPlainHTTP requires the transport gate:
// no TLS and no allow_insecure_http means loopback only.
func TestStartGatewayRefusesNonLoopbackPlainHTTP(t *testing.T) {
	dir := t.TempDir()
	writeConfigTree(t, dir, "listen: 0.0.0.0:8443\npublic_url: http://192.0.2.10:8443\n")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	g, err := startGateway(ctx, filepath.Join(dir, "gateway.yaml"), startOptions{stderr: io.Discard})
	if err == nil {
		cancel()
		if err := <-g.Done(); err != nil {
			t.Fatalf("shutdown: %v", err)
		}
		t.Fatal("the Gateway listened on a non-loopback address without TLS")
	}
	if !strings.Contains(err.Error(), "must be a loopback address") {
		t.Errorf("error = %q, want the loopback rule from the configuration check", err)
	}
}

// TestCheckTransport covers the transport gate directly, including the
// settings that make a non-loopback address acceptable.
func TestCheckTransport(t *testing.T) {
	tls := &config.GatewayTLS{CertFile: "/etc/hanse/tls/cert.pem", KeyFile: "/etc/hanse/tls/key.pem"}
	cases := []struct {
		name    string
		listen  string
		cfg     config.GatewayConfig
		wantErr bool
	}{
		{name: "loopback", listen: "127.0.0.1:8443"},
		{name: "loopback IPv6", listen: "[::1]:8443"},
		{name: "localhost", listen: "localhost:8443"},
		{name: "non-loopback refused", listen: "0.0.0.0:8443", wantErr: true},
		{name: "empty host refused", listen: ":8443", wantErr: true},
		{name: "private address refused", listen: "192.0.2.10:8443", wantErr: true},
		{name: "allow_insecure_http", listen: "0.0.0.0:8443", cfg: config.GatewayConfig{AllowInsecureHTTP: true}},
		{name: "TLS", listen: "0.0.0.0:8443", cfg: config.GatewayConfig{TLS: tls}},
		{name: "bad address", listen: "not-an-address", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkTransport(tc.listen, tc.cfg)
			if tc.wantErr && err == nil {
				t.Fatalf("checkTransport(%q) = nil, want an error", tc.listen)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("checkTransport(%q) = %v, want nil", tc.listen, err)
			}
		})
	}
}

// TestHealthAndReadinessAreUnauthenticated requires both probes to answer
// without a token, while the versioned API keeps its authentication.
func TestHealthAndReadinessAreUnauthenticated(t *testing.T) {
	g := startTestGateway(t)
	base := "http://" + g.Addr

	if status, body := probeURL(t, base+"/healthz"); status != http.StatusOK || body != "ok\n" {
		t.Errorf("GET /healthz = %d %q, want 200 %q", status, body, "ok\n")
	}
	if status, body := probeURL(t, base+"/readyz"); status != http.StatusOK || body != "ready\n" {
		t.Errorf("GET /readyz = %d %q, want 200 %q", status, body, "ready\n")
	}
	if status, _ := probeURL(t, base+"/v1/version"); status != http.StatusUnauthorized {
		t.Errorf("GET /v1/version = %d, want %d", status, http.StatusUnauthorized)
	}
}

// TestGracefulShutdown requires the Gateway to stop accepting connections
// and report a clean exit when its context is cancelled.
func TestGracefulShutdown(t *testing.T) {
	dir := configFixture(t, "")
	ctx, cancel := context.WithCancel(context.Background())
	g, err := startGateway(ctx, filepath.Join(dir, "gateway.yaml"), startOptions{
		listen: "127.0.0.1:0",
		stderr: io.Discard,
	})
	if err != nil {
		cancel()
		t.Fatalf("start the Gateway: %v", err)
	}
	base := "http://" + g.Addr
	if status, _ := probeURL(t, base+"/healthz"); status != http.StatusOK {
		t.Fatalf("GET /healthz = %d, want %d before shutdown", status, http.StatusOK)
	}

	cancel()
	select {
	case err := <-g.Done():
		if err != nil {
			t.Fatalf("shutdown: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the Gateway did not stop")
	}

	// The listener is closed, but the port may still accept a queued
	// connection briefly, so the check is a request with a short timeout.
	client := &http.Client{Timeout: 2 * time.Second}
	if response, err := client.Get(base + "/healthz"); err == nil {
		_ = response.Body.Close()
		t.Error("the Gateway still answers after shutdown")
	}
	if _, err := net.DialTimeout("tcp", g.Addr, time.Second); err == nil {
		t.Error("the listen address still accepts connections after shutdown")
	}
}

// writeConfigTree writes a minimal Gateway configuration with the given
// extra gateway.yaml lines. Placeholder addresses only: 192.0.2.0/24 is
// reserved for documentation.
// writeConfigTree writes gateway.yaml with node: gateway, nodes.yaml
// holding that node, an empty routes.yaml, and the directories they name.
func writeConfigTree(t *testing.T, dir, extra string) {
	t.Helper()
	writeConfigFiles(t, dir, "version: 1\nnode: gateway\npaths:\n"+
		"  nodes: nodes.yaml\n  routes: routes.yaml\n"+
		"  capabilities:\n    - "+dir+"/capabilities\n"+
		"  credentials_dir: "+dir+"/credentials\n  state_dir: "+dir+"/state\n"+
		extra,
		"version: 1\nnodes:\n  gateway:\n    os: linux\n    arch: amd64\n",
		"version: 1\ncapabilities: {}\n")
}

// writeConfigFiles writes gateway.yaml, nodes.yaml, and routes.yaml into
// dir and creates the directories gateway.yaml names.
func writeConfigFiles(t *testing.T, dir, gateway, nodes, routes string) {
	t.Helper()
	for name, content := range map[string]string{
		"gateway.yaml": gateway,
		"nodes.yaml":   nodes,
		"routes.yaml":  routes,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	for _, sub := range []string{"capabilities", "credentials", "state"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o700); err != nil {
			t.Fatalf("create %s: %v", sub, err)
		}
	}
}

// TestStartGatewayServesTLS requires the Gateway to serve HTTPS when the
// configuration names a certificate, and to load it before it reports ready.
func TestStartGatewayServesTLS(t *testing.T) {
	dir := t.TempDir()
	writeConfigTree(t, dir, "listen: 127.0.0.1:8443\n"+
		"public_url: https://gateway.example.com\n"+
		"tls:\n  cert_file: "+dir+"/tls/cert.pem\n  key_file: "+dir+"/tls/key.pem\n")
	writeSelfSignedCertificate(t, filepath.Join(dir, "tls"), "127.0.0.1")

	ctx, cancel := context.WithCancel(context.Background())
	g, err := startGateway(ctx, filepath.Join(dir, "gateway.yaml"), startOptions{
		listen: "127.0.0.1:0",
		stderr: io.Discard,
	})
	if err != nil {
		cancel()
		t.Fatalf("start the Gateway: %v", err)
	}
	defer func() {
		cancel()
		if err := <-g.Done(); err != nil {
			t.Errorf("shutdown: %v", err)
		}
	}()

	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			// The certificate is self-signed for this test only.
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}
	response, err := client.Get("https://" + g.Addr + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz over TLS: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read /healthz: %v", err)
	}
	if response.StatusCode != http.StatusOK || string(body) != "ok\n" {
		t.Errorf("GET /healthz = %d %q, want 200 %q", response.StatusCode, body, "ok\n")
	}
}

// TestStartGatewayRefusesABadCertificate requires the certificate to load
// before the Gateway reports ready, so a corrupt key never starts a server.
func TestStartGatewayRefusesABadCertificate(t *testing.T) {
	dir := t.TempDir()
	writeConfigTree(t, dir, "listen: 127.0.0.1:8443\n"+
		"public_url: https://gateway.example.com\n"+
		"tls:\n  cert_file: "+dir+"/tls/cert.pem\n  key_file: "+dir+"/tls/key.pem\n")
	if err := os.MkdirAll(filepath.Join(dir, "tls"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"cert.pem", "key.pem"} {
		if err := os.WriteFile(filepath.Join(dir, "tls", name), []byte("not a PEM"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	g, err := startGateway(ctx, filepath.Join(dir, "gateway.yaml"), startOptions{listen: "127.0.0.1:0", stderr: io.Discard})
	if err == nil {
		cancel()
		if err := <-g.Done(); err != nil {
			t.Fatalf("shutdown: %v", err)
		}
		t.Fatal("the Gateway started with a corrupt certificate")
	}
	if !strings.Contains(err.Error(), "TLS certificate") {
		t.Errorf("error = %q, want it to name the TLS certificate", err)
	}
}

// codeOfBody reads the error code of one API error response.
func codeOfBody(t *testing.T, body []byte) string {
	t.Helper()
	var e struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("decode the error body %s: %v", body, err)
	}
	return e.Code
}

// echoManifest is the exec capability of the tests without an embedded
// agent: it has no built-in executor, so only a node agent can run it.
const echoManifest = `version: 1
id: echo
name: Echo
description: Print a line from a node
contract: hanse.handler/v1

handler:
  kind: exec
  entry:
    linux: [sh, handler.sh]

operations:
  say:
    description: Write a marker file and print a line
    arguments:
      type: object
      additionalProperties: false
      properties:
        message:
          type: string
          description: The text to print
    options:
      message: provider
`

// writeEmbeddedFalseTree writes a Gateway configuration with
// embedded_agent: false: node gateway is the host node, one exec capability
// routes to it, and no agent connection is open for it. The handler creates
// marker when it runs, so a test can prove that it never ran in the Gateway
// process.
func writeEmbeddedFalseTree(t *testing.T, dir, marker string) {
	t.Helper()
	writeConfigFiles(t, dir,
		"version: 1\nnode: gateway\nembedded_agent: false\n"+
			"public_url: http://127.0.0.1:8443\nlisten: 127.0.0.1:8443\npaths:\n"+
			"  nodes: nodes.yaml\n  routes: routes.yaml\n"+
			"  capabilities:\n    - "+dir+"/capabilities\n"+
			"  credentials_dir: "+dir+"/credentials\n  state_dir: "+dir+"/state\n",
		"version: 1\nnodes:\n  gateway:\n    os: linux\n    arch: amd64\n",
		"version: 1\ncapabilities:\n  echo:\n    executors: [gateway]\n    default_executor: gateway\n")
	capDir := filepath.Join(dir, "capabilities", "echo")
	if err := os.MkdirAll(capDir, 0o755); err != nil {
		t.Fatalf("create the capability directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(capDir, "manifest.yaml"), []byte(echoManifest), 0o644); err != nil {
		t.Fatalf("write the manifest: %v", err)
	}
	handler := "#!/bin/sh\ntouch '" + marker + "'\nexit 0\n"
	if err := os.WriteFile(filepath.Join(capDir, "handler.sh"), []byte(handler), 0o644); err != nil {
		t.Fatalf("write the handler: %v", err)
	}
}

// TestGatewayWithoutEmbeddedAgentStarts requires the Gateway to accept
// embedded_agent: false and to serve the API.
func TestGatewayWithoutEmbeddedAgentStarts(t *testing.T) {
	dir := t.TempDir()
	writeEmbeddedFalseTree(t, dir, filepath.Join(dir, "ran-in-process"))
	ctx, cancel := context.WithCancel(context.Background())
	g, err := startGateway(ctx, filepath.Join(dir, "gateway.yaml"), startOptions{
		listen: "127.0.0.1:0",
		stderr: io.Discard,
	})
	if err != nil {
		cancel()
		t.Fatalf("start the Gateway without an embedded agent: %v", err)
	}
	defer func() {
		cancel()
		if err := <-g.Done(); err != nil {
			t.Errorf("shutdown: %v", err)
		}
	}()
	if status, body := probeURL(t, "http://"+g.Addr+"/healthz"); status != http.StatusOK || body != "ok\n" {
		t.Errorf("GET /healthz = %d %q, want 200 %q", status, body, "ok\n")
	}
	if status, body := probeURL(t, "http://"+g.Addr+"/readyz"); status != http.StatusOK || body != "ready\n" {
		t.Errorf("GET /readyz = %d %q, want 200 %q", status, body, "ready\n")
	}
}

// startGatewayWithToken starts the Gateway in dir with one API token and
// returns it with that token. dir already holds a complete configuration.
func startGatewayWithToken(t *testing.T, dir string) (*gateway, string) {
	t.Helper()
	store, err := auth.LoadStore(filepath.Join(dir, "state", "tokens.json"))
	if err != nil {
		t.Fatalf("load the token store: %v", err)
	}
	token, err := store.Add("admin", nil)
	if err != nil {
		t.Fatalf("add an API token: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	g, err := startGateway(ctx, filepath.Join(dir, "gateway.yaml"), startOptions{
		listen: "127.0.0.1:0",
		stderr: io.Discard,
	})
	if err != nil {
		cancel()
		t.Fatalf("start the Gateway without an embedded agent: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-g.Done():
			if err != nil {
				t.Errorf("shutdown: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("the Gateway did not stop")
		}
	})
	return g, token
}

// TestGatewayWithoutEmbeddedAgentSendsExecToTheHostAgent requires an exec
// run for the host node without a connection to fail with
// executor_unavailable, and never to run in the Gateway process.
func TestGatewayWithoutEmbeddedAgentSendsExecToTheHostAgent(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "ran-in-process")
	writeEmbeddedFalseTree(t, dir, marker)
	g, token := startGatewayWithToken(t, dir)

	request, err := http.NewRequest(http.MethodPost, "http://"+g.Addr+"/v1/operations",
		strings.NewReader(`{"capability":"echo","operation":"say","arguments":{}}`))
	if err != nil {
		t.Fatalf("build the request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := (&http.Client{Timeout: 30 * time.Second}).Do(request)
	if err != nil {
		t.Fatalf("submit the operation: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read the response: %v", err)
	}
	// Executor resolution refuses an offline node before anything runs, with
	// the executor_unavailable code of docs/architecture.md §14. The Gateway
	// never falls back to its own process.
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("submit status = %d, want 503, body %s", response.StatusCode, body)
	}
	if code := codeOfBody(t, body); code != "executor_unavailable" {
		t.Errorf("submit code = %q, want executor_unavailable, body %s", code, body)
	}
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Fatalf("the handler ran in the Gateway process (marker %s: %v)", marker, statErr)
	}
}

// TestGatewayWithoutEmbeddedAgentOptionsGoToTheHostAgent requires an exec
// option provider call to follow the same route as a run: to the host
// node's agent, never to the Gateway process.
func TestGatewayWithoutEmbeddedAgentOptionsGoToTheHostAgent(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "ran-in-process")
	writeEmbeddedFalseTree(t, dir, marker)
	g, token := startGatewayWithToken(t, dir)

	request, err := http.NewRequest(http.MethodGet,
		"http://"+g.Addr+"/v1/operations/echo/say/options/message", nil)
	if err != nil {
		t.Fatalf("build the request: %v", err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := (&http.Client{Timeout: 30 * time.Second}).Do(request)
	if err != nil {
		t.Fatalf("request the options: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read the response: %v", err)
	}
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("options status = %d, want 503, body %s", response.StatusCode, body)
	}
	if code := codeOfBody(t, body); code != "executor_unavailable" {
		t.Errorf("options code = %q, want executor_unavailable, body %s", code, body)
	}
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Fatalf("the option provider ran in the Gateway process (marker %s: %v)", marker, statErr)
	}
}

// TestDispatcherWithoutEmbeddedAgentUsesTheHostAgentPath calls the dispatcher
// directly, past executor resolution, and requires an exec call for the host
// node to go to the node agent path. With embedded_agent: false the Gateway
// builds no exec dispatcher, so the only possible outcome is the agent's
// answer.
func TestDispatcherWithoutEmbeddedAgentUsesTheHostAgentPath(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "ran-in-process")
	writeEmbeddedFalseTree(t, dir, marker)

	cfg, problems, err := config.Load(filepath.Join(dir, "gateway.yaml"))
	if err != nil || len(problems) > 0 {
		t.Fatalf("load the configuration: %v, problems: %v", err, problems)
	}
	reg, _, err := registry.Load(registry.Options{
		Dirs:  cfg.Gateway.Paths.Capabilities,
		Input: registryInput(cfg),
	})
	if err != nil {
		t.Fatalf("load the registry: %v", err)
	}
	hub, err := agentconn.New(agentconn.Config{
		Registry: reg,
		Verify:   func(string) (string, error) { return "", herrors.New(herrors.AuthenticationFailed, "", nil) },
	})
	if err != nil {
		t.Fatalf("build the hub: %v", err)
	}
	d := &dispatcher{
		registry: reg,
		host:     "gateway",
		embedded: false,
		agent:    operations.NewAgentDispatcher(hub, reg, config.Timeouts{}),
	}
	_, err = d.Execute(context.Background(), &operations.Call{Request: &runtime.Request{
		ID:         "op_test",
		Capability: "echo",
		Operation:  "say",
		Executor:   runtime.Node{ID: "gateway"},
	}})
	if !errors.Is(err, herrors.ExecutorUnavailable) {
		t.Fatalf("exec on gateway = %v, want executor_unavailable", err)
	}
	if !strings.Contains(err.Error(), "no agent connection") {
		t.Errorf("error = %q, want the node agent's answer", err)
	}
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Fatalf("the handler ran in the Gateway process (marker %s: %v)", marker, statErr)
	}
}

// writeEmbeddedFalseHTTPTree writes the embedded_agent: false configuration
// with an http capability routed to the host node. The Gateway calls url
// itself, so no node agent is involved.
func writeEmbeddedFalseHTTPTree(t *testing.T, dir, url string) {
	t.Helper()
	writeConfigFiles(t, dir,
		"version: 1\nnode: gateway\nembedded_agent: false\n"+
			"public_url: http://127.0.0.1:8443\nlisten: 127.0.0.1:8443\npaths:\n"+
			"  nodes: nodes.yaml\n  routes: routes.yaml\n"+
			"  capabilities:\n    - "+dir+"/capabilities\n"+
			"  credentials_dir: "+dir+"/credentials\n  state_dir: "+dir+"/state\n",
		"version: 1\nnodes:\n  gateway:\n    os: linux\n    arch: amd64\n",
		"version: 1\ncapabilities:\n  remote:\n    executors: [gateway]\n    default_executor: gateway\n")
	capDir := filepath.Join(dir, "capabilities", "remote")
	if err := os.MkdirAll(capDir, 0o755); err != nil {
		t.Fatalf("create the capability directory: %v", err)
	}
	manifest := "version: 1\nid: remote\nname: Remote\ndescription: Remote service capability\n" +
		"contract: hanse.handler/v1\n\nhandler:\n  kind: http\n  url: " + url + "\n\n" +
		"operations:\n  ping:\n    description: Ping the service\n    arguments:\n" +
		"      type: object\n      additionalProperties: false\n"
	if err := os.WriteFile(filepath.Join(capDir, "manifest.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatalf("write the manifest: %v", err)
	}
}

// TestGatewayWithoutEmbeddedAgentServesHTTPCapability requires an http
// capability routed to the host node to work while that node has no agent
// connection: the Gateway makes the call itself.
func TestGatewayWithoutEmbeddedAgentServesHTTPCapability(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"contract":"hanse.handler/v1","status":"ok","result":{"state":"online"}}`)
	}))
	defer server.Close()

	dir := t.TempDir()
	writeEmbeddedFalseHTTPTree(t, dir, server.URL)
	g, token := startGatewayWithToken(t, dir)

	request, err := http.NewRequest(http.MethodPost, "http://"+g.Addr+"/v1/operations",
		strings.NewReader(`{"capability":"remote","operation":"ping","arguments":{}}`))
	if err != nil {
		t.Fatalf("build the request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := (&http.Client{Timeout: 30 * time.Second}).Do(request)
	if err != nil {
		t.Fatalf("submit the operation: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read the response: %v", err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("submit status = %d, want 200, body %s", response.StatusCode, body)
	}
	if !strings.Contains(string(body), `"completed"`) {
		t.Errorf("body = %s, want a completed operation", body)
	}
}

// TestGatewayWithoutEmbeddedAgentAcceptsTheHostNode requires the host node
// to enroll and connect when embedded_agent is false: its node agent is the
// only executor for that node in this layout.
func TestGatewayWithoutEmbeddedAgentAcceptsTheHostNode(t *testing.T) {
	dir := t.TempDir()
	writeEmbeddedFalseTree(t, dir, filepath.Join(dir, "ran-in-process"))
	g, _ := startGatewayWithToken(t, dir)

	token, err := g.enroll.IssueJoinToken("gateway")
	if err != nil {
		t.Fatalf("issue a join token: %v", err)
	}
	join := `{"protocol":"hanse.agent/v1","join_token":"` + token.Plain +
		`","agent_version":"1.0.0","os":"linux","arch":"amd64"}`
	request, err := http.NewRequest(http.MethodPost, "http://"+g.Addr+"/v1/agent/join", strings.NewReader(join))
	if err != nil {
		t.Fatalf("build the join request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := (&http.Client{Timeout: 10 * time.Second}).Do(request)
	if err != nil {
		t.Fatalf("join the host node: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read the join response: %v", err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("join status = %d, want 200, body %s", response.StatusCode, body)
	}
	var joined struct {
		Node       string `json:"node"`
		Credential string `json:"credential"`
	}
	if err := json.Unmarshal(body, &joined); err != nil {
		t.Fatalf("decode the join response: %v", err)
	}
	if joined.Node != "gateway" || joined.Credential == "" {
		t.Fatalf("join response = %+v, want gateway and a credential", joined)
	}

	url := "ws" + strings.TrimPrefix("http://"+g.Addr, "http") + "/v1/agent/connect"
	dialer := *websocket.DefaultDialer
	dialer.Subprotocols = []string{agentproto.SubprotocolV1}
	ws, _, err := dialer.Dial(url, http.Header{"Authorization": {"Bearer " + joined.Credential}})
	if err != nil {
		t.Fatalf("connect the host node: %v", err)
	}
	defer func() { _ = ws.Close() }()
	hello, err := agentproto.Encode(&agentproto.Hello{
		Type: agentproto.TypeHello, Protocol: agentproto.ProtocolV1, AgentVersion: "1.0.0",
		OS: "linux", Arch: "amd64", Contracts: []string{"hanse.handler/v1"},
		MaxConcurrentRuns: 4,
	})
	if err != nil {
		t.Fatalf("encode hello: %v", err)
	}
	_ = ws.SetWriteDeadline(time.Now().Add(3 * time.Second))
	if err := ws.WriteMessage(websocket.TextMessage, hello); err != nil {
		t.Fatalf("write hello: %v", err)
	}
	_ = ws.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, welcomeData, err := ws.ReadMessage()
	if err != nil {
		t.Fatalf("read welcome: %v", err)
	}
	message, err := agentproto.Decode(welcomeData)
	if err != nil {
		t.Fatalf("decode welcome: %v", err)
	}
	if _, ok := message.(*agentproto.Welcome); !ok {
		t.Fatalf("first message is %T, want welcome", message)
	}
}

// writeSelfSignedCertificate writes a certificate and key for host into
// dir, for the TLS tests only.
func writeSelfSignedCertificate(t *testing.T, dir, host string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("create %s: %v", dir, err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: host},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		IPAddresses:           []net.IP{net.ParseIP(host)},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(filepath.Join(dir, "cert.pem"), certPEM, 0o600); err != nil {
		t.Fatalf("write certificate: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "key.pem"), keyPEM, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
}
