package main

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// configFixture writes a valid minimal gateway.yaml tree, the same shape
// internal/config's tests use, and returns its directory.
func configFixture(t *testing.T, extra string) string {
	t.Helper()
	dir := t.TempDir()
	gw := "version: 1\nnode: gateway\npublic_url: http://127.0.0.1:8443\nlisten: 127.0.0.1:8443\npaths:\n" +
		"  nodes: nodes.yaml\n  routes: routes.yaml\n  capabilities:\n    - " + dir + "/capabilities\n" +
		"  credentials_dir: " + dir + "/credentials\n  state_dir: " + dir + "/state\n" + extra
	nodes := "version: 1\nnodes:\n  gateway:\n    os: linux\n    arch: amd64\n"
	routes := "version: 1\ncapabilities:\n  demo:\n    executors: \"*\"\n    default_executor: gateway\n"
	files := map[string]string{"gateway.yaml": gw, "nodes.yaml": nodes, "routes.yaml": routes}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, d := range []string{"capabilities", "credentials", "state"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestRunCheckConfigValidExitsZero(t *testing.T) {
	dir := configFixture(t, "")
	var stdout, stderr bytes.Buffer
	code := run([]string{"--check-config", "--config", filepath.Join(dir, "gateway.yaml")}, &stdout, &stderr)
	if code != exitOK {
		t.Fatalf("exit = %d, want %d\nstdout: %s\nstderr: %s", code, exitOK, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "configuration is valid") {
		t.Errorf("stdout = %q, want it to report a valid configuration", stdout.String())
	}
}

// TestRunCheckConfigWithoutNode requires `node` to be optional: a Gateway
// that runs no embedded agent validates like any other.
func TestRunCheckConfigWithoutNode(t *testing.T) {
	dir := t.TempDir()
	writeNoNodeTree(t, dir, filepath.Join(dir, "ran-in-process"))
	var stdout, stderr bytes.Buffer
	code := run([]string{"--check-config", "--config", filepath.Join(dir, "gateway.yaml")}, &stdout, &stderr)
	if code != exitOK {
		t.Fatalf("exit = %d, want %d\nstdout: %s\nstderr: %s", code, exitOK, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "configuration is valid") {
		t.Errorf("stdout = %q, want it to report a valid configuration", stdout.String())
	}
}

// TestRunCheckConfigUnknownNode keeps the reference check: an optional
// `node` that names no node in nodes.yaml is still a problem.
func TestRunCheckConfigUnknownNode(t *testing.T) {
	dir := configFixture(t, "")
	path := filepath.Join(dir, "gateway.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	changed := strings.Replace(string(data), "node: gateway", "node: ghost", 1)
	if err := os.WriteFile(path, []byte(changed), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := run([]string{"--check-config", "--config", path}, &stdout, &stderr)
	if code != exitFail {
		t.Fatalf("exit = %d, want %d", code, exitFail)
	}
	if !strings.Contains(stderr.String(), `node "ghost" does not exist`) {
		t.Errorf("stderr = %q, want the unknown-node problem", stderr.String())
	}
}

func TestRunCheckConfigProblemsExitOne(t *testing.T) {
	dir := configFixture(t, "timeuouts:\n  operation: 5m\n")
	var stdout, stderr bytes.Buffer
	code := run([]string{"--check-config", "--config", filepath.Join(dir, "gateway.yaml")}, &stdout, &stderr)
	if code != exitFail {
		t.Fatalf("exit = %d, want %d\nstderr: %s", code, exitFail, stderr.String())
	}
	if stdout.String() != "" {
		t.Errorf("stdout = %q, want nothing", stdout.String())
	}
	if !strings.Contains(stderr.String(), "field timeuouts not found") {
		t.Errorf("stderr = %q, want the unknown-field problem", stderr.String())
	}
	if !strings.Contains(stderr.String(), filepath.Join(dir, "gateway.yaml")) {
		t.Errorf("stderr = %q, want every problem to name the file path", stderr.String())
	}
}

func TestRunCheckConfigMissingFileExitsOne(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"--check-config", "--config", filepath.Join(t.TempDir(), "gateway.yaml")}, &stdout, &stderr)
	if code != exitFail {
		t.Fatalf("exit = %d, want %d\nstderr: %s", code, exitFail, stderr.String())
	}
	if !strings.Contains(stderr.String(), "cannot read the file") {
		t.Errorf("stderr = %q, want the unreadable-file problem", stderr.String())
	}
}

func TestRunCheckConfigNoCelebrationForServer(t *testing.T) {
	// --check-config never starts a server. The output must be exactly
	// one line naming the file and its validity.
	dir := configFixture(t, "")
	var stdout, stderr bytes.Buffer
	code := run([]string{"--check-config", "--config", filepath.Join(dir, "gateway.yaml")}, &stdout, &stderr)
	if code != exitOK {
		t.Fatalf("exit = %d, want %d", code, exitOK)
	}
	if len(strings.Split(strings.TrimSpace(stdout.String()), "\n")) != 1 {
		t.Errorf("stdout = %q, want exactly one line", stdout.String())
	}
}

func TestRunWithoutConfigMissingExitsOne(t *testing.T) {
	// Without a subcommand the program runs the server. A missing
	// configuration file stops it before it opens a listener.
	var stdout, stderr bytes.Buffer
	code := run([]string{"--config", filepath.Join(t.TempDir(), "gateway.yaml")}, &stdout, &stderr)
	if code != exitFail {
		t.Fatalf("exit = %d, want %d", code, exitFail)
	}
	if !strings.Contains(stderr.String(), "cannot read the file") {
		t.Errorf("stderr = %q, want the unreadable-file problem", stderr.String())
	}
}

func TestRunUnknownArgumentPrintsUsage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"nonsense"}, &stdout, &stderr)
	if code != exitUse {
		t.Fatalf("exit = %d, want %d", code, exitUse)
	}
	if !strings.Contains(stderr.String(), "usage: hanse-gateway") {
		t.Errorf("stderr = %q, want the usage line", stderr.String())
	}
}

func TestRunVersion(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"--version"}, &stdout, &stderr)
	if code != exitOK {
		t.Fatalf("exit = %d, want %d", code, exitOK)
	}
	if !strings.HasPrefix(stdout.String(), "hanse-gateway ") {
		t.Errorf("stdout = %q, want the version line", stdout.String())
	}
}

func TestDefaultConfigPathPerOS(t *testing.T) {
	// The documented default location per platform (docs/configuration.md
	// §1.1); this test runs on one platform, so only that one is exact.
	switch {
	case isWindows():
		if want := `C:\ProgramData\Hanse\gateway\gateway.yaml`; defaultConfigPath() != want {
			t.Errorf("path = %q, want %q", defaultConfigPath(), want)
		}
	case isDarwin():
		if want := "/usr/local/etc/hanse/gateway.yaml"; defaultConfigPath() != want {
			t.Errorf("path = %q, want %q", defaultConfigPath(), want)
		}
	default:
		if want := "/etc/hanse/gateway.yaml"; defaultConfigPath() != want {
			t.Errorf("path = %q, want %q", defaultConfigPath(), want)
		}
	}
}

func isWindows() bool { return runtime.GOOS == "windows" }

func isDarwin() bool { return runtime.GOOS == "darwin" }
