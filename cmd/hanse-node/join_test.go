package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/achmadss/hanse/internal/config"
)

// testCredential is a placeholder credential of the documented shape.
const testCredential = "hnc_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

// testJoinToken is a placeholder join token of the documented shape.
const testJoinToken = "hjt_BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"

// joinServer serves one join endpoint. The handler requires the documented
// request and answers with node-a's credential.
func joinServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/agent/join" {
			t.Errorf("request = %s %s, want POST /v1/agent/join", r.Method, r.URL.Path)
		}
		var in joinInput
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			t.Errorf("decode the join request: %v", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if in.Protocol != agentProtocolV1 {
			t.Errorf("protocol = %q, want %q", in.Protocol, agentProtocolV1)
		}
		if in.JoinToken != testJoinToken {
			t.Errorf("join token = %q, want the presented token", in.JoinToken)
		}
		if in.OS != runtime.GOOS || in.Arch != runtime.GOARCH {
			t.Errorf("os/arch = %s/%s, want %s/%s", in.OS, in.Arch, runtime.GOOS, runtime.GOARCH)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"node":"node-a","credential":"` + testCredential + `"}`))
	}))
}

// TestJoinStoresCredential enrolls against a local server and requires the
// credential file with mode 0600 inside a 0700 directory, holding the node
// ID and no secret in the output.
func TestJoinStoresCredential(t *testing.T) {
	server := joinServer(t)
	defer server.Close()
	stateDir := filepath.Join(t.TempDir(), "state")

	var stdout, stderr bytes.Buffer
	code := run([]string{"join", server.URL, testJoinToken, "--state-dir", stateDir}, &stdout, &stderr)
	if code != exitOK {
		t.Fatalf("exit = %d, stderr: %s", code, stderr.String())
	}
	if stderr.String() != "" {
		t.Errorf("stderr = %q, want nothing: no log may carry a credential", stderr.String())
	}
	path := filepath.Join(stateDir, credentialFileName)
	if !strings.Contains(stdout.String(), "node-a") || !strings.Contains(stdout.String(), path) {
		t.Errorf("stdout = %q, want the node and the credential path", stdout.String())
	}
	for _, secret := range []string{testJoinToken, testCredential} {
		if strings.Contains(stdout.String(), secret) {
			t.Errorf("stdout holds %q: the value must never be printed", secret)
		}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the credential file: %v", err)
	}
	var stored credentialFile
	if err := json.Unmarshal(data, &stored); err != nil {
		t.Fatalf("decode the credential file: %v", err)
	}
	if stored.Version != credentialFileVersion {
		t.Errorf("version = %d, want %d", stored.Version, credentialFileVersion)
	}
	if stored.Node != "node-a" || stored.Credential != testCredential || stored.GatewayURL != server.URL {
		t.Errorf("stored credential = %+v, want node-a, the credential, and the Gateway URL", stored)
	}
	if runtime.GOOS != "windows" {
		checkMode(t, path, 0o600)
		checkMode(t, stateDir, 0o700)
	}
}

// checkMode requires the permission bits of path to be exactly want.
func checkMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Errorf("mode of %s = %s, want %s", path, got, want)
	}
}

// TestJoinRefused writes no credential when the Gateway refuses the token
// and reports the Gateway's code.
func TestJoinRefused(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":"join_token_invalid","message":"join token invalid"}`))
	}))
	defer server.Close()
	stateDir := t.TempDir()

	var stdout, stderr bytes.Buffer
	code := run([]string{"join", server.URL, testJoinToken, "--state-dir", stateDir}, &stdout, &stderr)
	if code != exitFail {
		t.Fatalf("exit = %d, want %d", code, exitFail)
	}
	if !strings.Contains(stderr.String(), "join_token_invalid") {
		t.Errorf("stderr = %q, want the Gateway's code", stderr.String())
	}
	if strings.Contains(stderr.String(), testJoinToken) {
		t.Error("stderr holds the join token")
	}
	if _, err := os.Stat(filepath.Join(stateDir, credentialFileName)); !os.IsNotExist(err) {
		t.Errorf("a credential file exists after a refused join: %v", err)
	}
}

// TestJoinRejectsPlainHTTP requires plain HTTP for a non-loopback host to
// fail before any request is sent (docs/agent-protocol.md §1).
func TestJoinRejectsPlainHTTP(t *testing.T) {
	stateDir := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := run([]string{"join", "http://192.0.2.10:8443", testJoinToken, "--state-dir", stateDir}, &stdout, &stderr)
	if code != exitFail {
		t.Fatalf("exit = %d, want %d", code, exitFail)
	}
	if !strings.Contains(stderr.String(), "must use https") {
		t.Errorf("stderr = %q, want the https rule", stderr.String())
	}
}

// TestJoinRefusesRedirect proves that a redirect never carries the join
// token to another location, which may be plain http or another host.
func TestJoinRefusesRedirect(t *testing.T) {
	var reached atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"node":"node-a","credential":"` + testCredential + `"}`))
	}))
	defer target.Close()
	moved := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/v1/agent/join", http.StatusTemporaryRedirect)
	}))
	defer moved.Close()
	stateDir := t.TempDir()

	var stdout, stderr bytes.Buffer
	code := run([]string{"join", moved.URL, testJoinToken, "--state-dir", stateDir}, &stdout, &stderr)
	if code != exitFail {
		t.Fatalf("exit = %d, want %d", code, exitFail)
	}
	if got := reached.Load(); got != 0 {
		t.Errorf("the redirect target got %d request(s), want none: the token must not follow a redirect", got)
	}
	if !strings.Contains(stderr.String(), "307") {
		t.Errorf("stderr = %q, want the redirect status", stderr.String())
	}
	if strings.Contains(stderr.String(), testJoinToken) {
		t.Error("stderr holds the join token")
	}
	if _, err := os.Stat(filepath.Join(stateDir, credentialFileName)); !os.IsNotExist(err) {
		t.Errorf("a credential file exists after a refused join: %v", err)
	}
}

// TestJoinUsage requires the join command with no arguments to print its
// usage and exit with the usage code.
func TestJoinUsage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"join"}, &stdout, &stderr)
	if code != exitUse {
		t.Fatalf("exit = %d, want %d", code, exitUse)
	}
	if !strings.Contains(stderr.String(), "hanse-node join") {
		t.Errorf("stderr = %q, want the join usage", stderr.String())
	}
}

// TestRunVersion keeps the version flag working after the subcommand split.
func TestRunVersion(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"--version"}, &stdout, &stderr)
	if code != exitOK {
		t.Fatalf("exit = %d, want %d", code, exitOK)
	}
	if !strings.HasPrefix(stdout.String(), "hanse-node ") {
		t.Errorf("stdout = %q, want the version line", stdout.String())
	}
}

// TestJoinWritesGatewayURL proves that join stores gateway_url in the
// agent.yaml named by --config and keeps every other value.
func TestJoinWritesGatewayURL(t *testing.T) {
	server := joinServer(t)
	defer server.Close()
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	configPath := filepath.Join(dir, "agent.yaml")
	if err := os.WriteFile(configPath, []byte("version: 1\nstate_dir: /var/lib/hanse-node\nmax_concurrent_runs: 8\n"), 0o600); err != nil {
		t.Fatalf("write agent.yaml: %v", err)
	}
	var stdout, stderr bytes.Buffer
	code := run([]string{"join", server.URL, testJoinToken, "--state-dir", stateDir, "--config", configPath}, &stdout, &stderr)
	if code != exitOK {
		t.Fatalf("exit code %d, stderr %s", code, stderr.String())
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read agent.yaml: %v", err)
	}
	text := string(data)
	if !strings.Contains(text, "gateway_url: "+server.URL) {
		t.Errorf("agent.yaml does not name the Gateway URL:\n%s", text)
	}
	if !strings.Contains(text, "max_concurrent_runs: 8") {
		t.Errorf("join dropped another setting:\n%s", text)
	}
	if strings.Contains(stdout.String(), testCredential) || strings.Contains(stderr.String(), testCredential) {
		t.Error("the credential reached the output")
	}
}

// TestJoinCreatesMissingConfigFile proves that a join with --config and no
// file creates one the loader accepts.
func TestJoinCreatesMissingConfigFile(t *testing.T) {
	server := joinServer(t)
	defer server.Close()
	dir := t.TempDir()
	configPath := filepath.Join(dir, "nested", "agent.yaml")
	var stdout, stderr bytes.Buffer
	code := run([]string{"join", server.URL, testJoinToken, "--state-dir", filepath.Join(dir, "state"), "--config", configPath}, &stdout, &stderr)
	if code != exitOK {
		t.Fatalf("exit code %d, stderr %s", code, stderr.String())
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read agent.yaml: %v", err)
	}
	if !strings.Contains(string(data), "version: 1") || !strings.Contains(string(data), "gateway_url: "+server.URL) {
		t.Errorf("new agent.yaml is incomplete:\n%s", data)
	}
	if !strings.Contains(string(data), "state_dir: "+filepath.Join(dir, "state")) {
		t.Errorf("new agent.yaml does not name the state directory:\n%s", data)
	}
	if runtime.GOOS != "windows" {
		checkMode(t, configPath, 0o600)
	}
}

// TestJoinUsesTheConfiguredStateDir proves that join without --state-dir
// stores the credential where agent.yaml says, not in the default directory
// (issue #71).
func TestJoinUsesTheConfiguredStateDir(t *testing.T) {
	server := joinServer(t)
	defer server.Close()
	dir := t.TempDir()
	configPath := filepath.Join(dir, "agent.yaml")
	if err := os.WriteFile(configPath, []byte("version: 1\nstate_dir: ./node-state\n"), 0o600); err != nil {
		t.Fatalf("write agent.yaml: %v", err)
	}
	var stdout, stderr bytes.Buffer
	code := run([]string{"join", server.URL, testJoinToken, "--config", configPath}, &stdout, &stderr)
	if code != exitOK {
		t.Fatalf("exit code %d, stderr %s", code, stderr.String())
	}
	stateDir := filepath.Join(dir, "node-state")
	if _, err := os.Stat(filepath.Join(stateDir, credentialFileName)); err != nil {
		t.Fatalf("the credential is not in the configured state directory: %v", err)
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read agent.yaml: %v", err)
	}
	if !strings.Contains(string(data), "state_dir: ./node-state") {
		t.Errorf("join rewrote a state_dir it did not name:\n%s", data)
	}
}

// TestJoinWritesTheStateDirAsAnAbsolutePath proves that --state-dir is
// recorded beside gateway_url as an absolute path, and that the existing
// fields and comments survive.
func TestJoinWritesTheStateDirAsAnAbsolutePath(t *testing.T) {
	server := joinServer(t)
	defer server.Close()
	dir := t.TempDir()
	configPath := filepath.Join(dir, "agent.yaml")
	body := "# keep this comment\nversion: 1\nmax_concurrent_runs: 8\n"
	if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
		t.Fatalf("write agent.yaml: %v", err)
	}
	// A relative --state-dir resolves against the working directory.
	t.Chdir(dir)
	var stdout, stderr bytes.Buffer
	code := run([]string{"join", server.URL, testJoinToken, "--state-dir", "./node", "--config", configPath}, &stdout, &stderr)
	if code != exitOK {
		t.Fatalf("exit code %d, stderr %s", code, stderr.String())
	}
	stateDir, ok, err := config.AgentStateDir(configPath)
	if err != nil {
		t.Fatalf("read state_dir: %v", err)
	}
	if !ok || !filepath.IsAbs(stateDir) {
		t.Fatalf("state_dir = %q, want an absolute path", stateDir)
	}
	if want := filepath.Join(dir, "node"); stateDir != want {
		t.Errorf("state_dir = %q, want %q", stateDir, want)
	}
	if _, err := os.Stat(filepath.Join(stateDir, credentialFileName)); err != nil {
		t.Fatalf("the credential is not in the named state directory: %v", err)
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read agent.yaml: %v", err)
	}
	text := string(data)
	if !strings.Contains(text, "# keep this comment") {
		t.Errorf("join dropped a comment:\n%s", text)
	}
	if !strings.Contains(text, "max_concurrent_runs: 8") {
		t.Errorf("join dropped another setting:\n%s", text)
	}
	if !strings.Contains(text, "state_dir: "+stateDir) {
		t.Errorf("agent.yaml does not name the absolute state directory:\n%s", text)
	}
}
