package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// gatewayFixture writes a valid gateway.yaml tree whose first capability
// directory is <dir>/capabilities. Every host value is a placeholder.
func gatewayFixture(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"gateway.yaml": `version: 1
node: gateway
public_url: https://gateway.example.com
paths:
  nodes: nodes.yaml
  routes: routes.yaml
  capabilities:
    - capabilities
  credentials_dir: credentials
  state_dir: state
`,
		"nodes.yaml": `version: 1
nodes:
  gateway:
    name: Gateway host
    os: linux
    arch: amd64
    roles: [server]
    address: 192.0.2.10
`,
		"routes.yaml": `version: 1
capabilities: {}
`,
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	if err := os.MkdirAll(filepath.Join(dir, "credentials"), 0o700); err != nil {
		t.Fatalf("create credentials directory: %v", err)
	}
	caps := filepath.Join(dir, "capabilities")
	if err := os.MkdirAll(caps, 0o755); err != nil {
		t.Fatalf("create capability directory: %v", err)
	}
	return filepath.Join(dir, "gateway.yaml"), caps
}

// cliPackageManifest is a minimal valid package manifest.
const cliPackageManifest = `version: 1
id: demo
name: Demo
description: Remove the demonstration file from a node
contract: hanse.handler/v1

handler:
  kind: exec
  entry:
    linux: [sh, handler.sh]
    darwin: [sh, handler.sh]

operations:
  clean:
    description: Remove the demonstration file
    arguments:
      type: object
      additionalProperties: false
      properties:
        path:
          type: string
`

// writeCLIPackage writes one package directory.
func writeCLIPackage(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "demo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create package: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(cliPackageManifest), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "handler.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write handler: %v", err)
	}
	return dir
}

// runCapabilities runs one capabilities subcommand with the test writers.
func runCapabilities(t *testing.T, args []string, stdinIsTerminal bool, stdin string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), args, Options{
		Stdin:           strings.NewReader(stdin),
		Stdout:          &stdout,
		Stderr:          &stderr,
		StdinIsTerminal: stdinIsTerminal,
		// No CLI login: the reload is a warning only.
		ConfigPath: filepath.Join(t.TempDir(), "missing-cli-config.json"),
	})
	return code, stdout.String(), stderr.String()
}

// TestCapabilitiesInstallAndRemove installs a local package, reports the
// JSON result, warns that the Gateway was not reloaded, then removes it.
func TestCapabilitiesInstallAndRemove(t *testing.T) {
	gatewayConfig, capsDir := gatewayFixture(t)
	pkgDir := writeCLIPackage(t)

	code, stdout, stderr := runCapabilities(t, []string{
		"capabilities", "install", pkgDir,
		"--config", gatewayConfig, "--yes", "--format", "json",
	}, false, "")
	if code != 0 {
		t.Fatalf("install exit code = %d, stderr: %s", code, stderr)
	}
	var installed struct {
		Capability string `json:"capability"`
		Path       string `json:"path"`
		Reloaded   bool   `json:"reloaded"`
		Warnings   []any  `json:"warnings"`
	}
	if err := json.Unmarshal([]byte(stdout), &installed); err != nil {
		t.Fatalf("install output is not JSON: %v\n%s", err, stdout)
	}
	if installed.Capability != "demo" {
		t.Errorf("installed capability = %q, want demo", installed.Capability)
	}
	if installed.Path != filepath.Join(capsDir, "demo") {
		t.Errorf("installed path = %q", installed.Path)
	}
	if installed.Reloaded {
		t.Error("installed reloaded = true without a CLI login")
	}
	if installed.Warnings == nil {
		t.Error("installed warnings = null, want an empty list")
	}
	if !strings.Contains(stderr, "takes effect after a reload or a restart") {
		t.Errorf("install stderr = %q, want the reload warning", stderr)
	}
	if _, err := os.Stat(filepath.Join(capsDir, "demo", "manifest.yaml")); err != nil {
		t.Fatalf("installed manifest: %v", err)
	}

	code, stdout, stderr = runCapabilities(t, []string{
		"capabilities", "remove", "demo",
		"--config", gatewayConfig, "--yes", "--format", "json",
	}, false, "")
	if code != 0 {
		t.Fatalf("remove exit code = %d, stderr: %s", code, stderr)
	}
	var removed struct {
		Capability string `json:"capability"`
		Path       string `json:"path"`
	}
	if err := json.Unmarshal([]byte(stdout), &removed); err != nil {
		t.Fatalf("remove output is not JSON: %v\n%s", err, stdout)
	}
	if removed.Capability != "demo" || removed.Path != filepath.Join(capsDir, "demo") {
		t.Errorf("remove output = %+v", removed)
	}
	if _, err := os.Stat(filepath.Join(capsDir, "demo")); !os.IsNotExist(err) {
		t.Errorf("capability directory still exists after remove: %v", err)
	}
}

// TestCapabilitiesInstallNeedsConfirmation checks that a non-interactive
// run refuses without --yes and does not install anything.
func TestCapabilitiesInstallNeedsConfirmation(t *testing.T) {
	gatewayConfig, capsDir := gatewayFixture(t)
	pkgDir := writeCLIPackage(t)

	code, _, stderr := runCapabilities(t, []string{
		"capabilities", "install", pkgDir, "--config", gatewayConfig,
	}, false, "y\n")
	if code != 2 {
		t.Fatalf("exit code = %d, want 2; stderr: %s", code, stderr)
	}
	if !strings.Contains(stderr, "--yes") {
		t.Errorf("stderr = %q, want it to mention --yes", stderr)
	}
	if _, err := os.Stat(filepath.Join(capsDir, "demo")); !os.IsNotExist(err) {
		t.Errorf("the package was installed without a confirmation: %v", err)
	}
}

// TestCapabilitiesInstallConfirmationOnTerminal confirms with "y" and
// installs.
func TestCapabilitiesInstallConfirmationOnTerminal(t *testing.T) {
	gatewayConfig, capsDir := gatewayFixture(t)
	pkgDir := writeCLIPackage(t)

	code, stdout, stderr := runCapabilities(t, []string{
		"capabilities", "install", pkgDir, "--config", gatewayConfig,
	}, true, "y\n")
	if code != 0 {
		t.Fatalf("exit code = %d, stderr: %s", code, stderr)
	}
	if !strings.Contains(stdout, "OPERATION") || !strings.Contains(stdout, "clean") {
		t.Errorf("preview did not list the operations: %s", stdout)
	}
	if _, err := os.Stat(filepath.Join(capsDir, "demo", "manifest.yaml")); err != nil {
		t.Fatalf("installed manifest: %v", err)
	}
}

// TestCapabilitiesInstallDuplicate checks that a second install of the same
// ID points at the remove command.
func TestCapabilitiesInstallDuplicate(t *testing.T) {
	gatewayConfig, _ := gatewayFixture(t)
	pkgDir := writeCLIPackage(t)

	if code, _, stderr := runCapabilities(t, []string{
		"capabilities", "install", pkgDir, "--config", gatewayConfig, "--yes",
	}, false, ""); code != 0 {
		t.Fatalf("first install exit code = %d, stderr: %s", code, stderr)
	}
	code, _, stderr := runCapabilities(t, []string{
		"capabilities", "install", pkgDir, "--config", gatewayConfig, "--yes",
	}, false, "")
	if code == 0 {
		t.Fatal("second install succeeded")
	}
	if !strings.Contains(stderr, "hanse capabilities remove demo") {
		t.Errorf("stderr = %q, want the remove suggestion", stderr)
	}
}
