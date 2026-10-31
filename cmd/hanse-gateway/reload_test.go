package main

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// reloadRoutes is routes.yaml with the echo capability routed to the
// Gateway host node, which runs it in the embedded agent.
const reloadRoutes = "version: 1\ncapabilities:\n  echo:\n    executors: [gateway]\n    default_executor: gateway\n"

// writeReloadCapability writes the echo capability package into dir's
// capability root. Its handler creates marker, so a test can prove the
// operation ran.
func writeReloadCapability(t *testing.T, dir, marker string) string {
	t.Helper()
	capDir := filepath.Join(dir, "capabilities", "echo")
	if err := os.MkdirAll(capDir, 0o755); err != nil {
		t.Fatalf("create the capability directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(capDir, "manifest.yaml"), []byte(echoManifest), 0o644); err != nil {
		t.Fatalf("write the manifest: %v", err)
	}
	handler := "#!/bin/sh\ntouch '" + marker + "'\n" +
		"printf '%s\\n' '{\"contract\":\"hanse.handler/v1\",\"status\":\"ok\",\"result\":{\"ok\":true}}'\n"
	if err := os.WriteFile(filepath.Join(capDir, "handler.sh"), []byte(handler), 0o644); err != nil {
		t.Fatalf("write the handler: %v", err)
	}
	return capDir
}

// doJSON performs one request against the test Gateway and returns the
// status and the body.
func doJSON(t *testing.T, g *gateway, token, method, path, body string) (int, []byte) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	request, err := http.NewRequest(method, "http://"+g.Addr+path, reader)
	if err != nil {
		t.Fatalf("build %s %s: %v", method, path, err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := (&http.Client{Timeout: 30 * time.Second}).Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read %s %s: %v", method, path, err)
	}
	return response.StatusCode, data
}

// runEcho runs echo.say and requires the operation to complete.
func runEcho(t *testing.T, g *gateway, token string) {
	t.Helper()
	status, body := doJSON(t, g, token, http.MethodPost, "/v1/operations",
		`{"capability":"echo","operation":"say","arguments":{"message":"hello"}}`)
	if status != http.StatusOK {
		t.Fatalf("submit echo.say = %d, want 200: %s", status, body)
	}
	var out struct {
		Status string `json:"status"`
		Error  *struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode the operation: %v", err)
	}
	if out.Status != "completed" {
		t.Fatalf("echo.say status = %q (error %+v), want completed: %s", out.Status, out.Error, body)
	}
}

// TestReloadInstallsCapabilityAndRoute is the issue's acceptance test: it
// installs a capability package, adds its route to routes.yaml, reloads the
// Gateway, and runs it, all without a restart.
func TestReloadInstallsCapabilityAndRoute(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fixture handler needs a POSIX shell")
	}
	dir := t.TempDir()
	writeConfigTree(t, dir, "public_url: http://127.0.0.1:8443\n")
	g, token := startGatewayWithToken(t, dir)

	marker := filepath.Join(dir, "ran")
	writeReloadCapability(t, dir, marker)
	if err := os.WriteFile(filepath.Join(dir, "routes.yaml"), []byte(reloadRoutes), 0o644); err != nil {
		t.Fatalf("add the route: %v", err)
	}

	status, body := doJSON(t, g, token, http.MethodPost, "/v1/capabilities/reload", "")
	if status != http.StatusOK {
		t.Fatalf("POST /v1/capabilities/reload = %d, want 200: %s", status, body)
	}
	var reloaded struct {
		Capabilities []struct {
			ID string `json:"id"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(body, &reloaded); err != nil {
		t.Fatalf("decode the reload: %v", err)
	}
	found := false
	for _, c := range reloaded.Capabilities {
		if c.ID == "echo" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the reload did not load echo: %s", body)
	}

	runEcho(t, g, token)
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("the reloaded capability did not run: %v", err)
	}
}

// TestReloadKeepsRoutingWhenRoutesAreInvalid requires an invalid routes.yaml
// to fail the reload and to leave the running routing in place.
func TestReloadKeepsRoutingWhenRoutesAreInvalid(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fixture handler needs a POSIX shell")
	}
	dir := t.TempDir()
	writeConfigTree(t, dir, "public_url: http://127.0.0.1:8443\n")
	if err := os.WriteFile(filepath.Join(dir, "routes.yaml"), []byte(reloadRoutes), 0o644); err != nil {
		t.Fatalf("write the route: %v", err)
	}
	marker := filepath.Join(dir, "ran")
	writeReloadCapability(t, dir, marker)
	g, token := startGatewayWithToken(t, dir)
	runEcho(t, g, token)

	// A route to a node that nodes.yaml does not have is one problem of
	// the new file; the reload must report it and change nothing.
	invalid := "version: 1\ncapabilities:\n  echo:\n    executors: [node-missing]\n    default_executor: node-missing\n"
	if err := os.WriteFile(filepath.Join(dir, "routes.yaml"), []byte(invalid), 0o644); err != nil {
		t.Fatalf("write the invalid route: %v", err)
	}
	status, body := doJSON(t, g, token, http.MethodPost, "/v1/capabilities/reload", "")
	if status != http.StatusInternalServerError {
		t.Fatalf("reload with an invalid routes.yaml = %d, want 500: %s", status, body)
	}
	if code := codeOfBody(t, body); code != "internal" {
		t.Errorf("reload error code = %q, want internal: %s", code, body)
	}
	if !strings.Contains(string(body), "node-missing") {
		t.Errorf("the reload did not report the problem: %s", body)
	}

	if err := os.Remove(marker); err != nil {
		t.Fatalf("remove the marker: %v", err)
	}
	runEcho(t, g, token)
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("the failed reload dropped the running route: %v", err)
	}
}

// TestReloadAppliesNodesFile changes nodes.yaml and requires the reload to
// serve the new node set, with the Gateway host node still online.
func TestReloadAppliesNodesFile(t *testing.T) {
	dir := t.TempDir()
	writeConfigTree(t, dir, "public_url: http://127.0.0.1:8443\n")
	g, token := startGatewayWithToken(t, dir)

	nodes := "version: 1\nnodes:\n  gateway:\n    os: linux\n    arch: amd64\n" +
		"  node-a:\n    os: linux\n    arch: amd64\n"
	if err := os.WriteFile(filepath.Join(dir, "nodes.yaml"), []byte(nodes), 0o644); err != nil {
		t.Fatalf("add the node: %v", err)
	}
	status, body := doJSON(t, g, token, http.MethodPost, "/v1/capabilities/reload", "")
	if status != http.StatusOK {
		t.Fatalf("POST /v1/capabilities/reload = %d, want 200: %s", status, body)
	}

	status, body = doJSON(t, g, token, http.MethodGet, "/v1/nodes/node-a", "")
	if status != http.StatusOK {
		t.Fatalf("GET /v1/nodes/node-a = %d, want 200: %s", status, body)
	}
	status, body = doJSON(t, g, token, http.MethodGet, "/v1/nodes/gateway", "")
	if status != http.StatusOK {
		t.Fatalf("GET /v1/nodes/gateway = %d, want 200: %s", status, body)
	}
	var host struct {
		Node struct {
			Status string `json:"status"`
		} `json:"node"`
	}
	if err := json.Unmarshal(body, &host); err != nil {
		t.Fatalf("decode the host node: %v", err)
	}
	if host.Node.Status != "online" {
		t.Errorf("the Gateway host is %q after the reload, want online", host.Node.Status)
	}
}
