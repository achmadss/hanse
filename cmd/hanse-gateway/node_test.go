package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/achmadss/hanse/internal/enroll"
	herrors "github.com/achmadss/hanse/internal/errors"
)

// nodeFixture writes the configuration fixture with node-a and node-b in
// nodes.yaml, so the node subcommands have real IDs to work with.
func nodeFixture(t *testing.T) (configPath, stateDir string) {
	t.Helper()
	dir := configFixture(t, "")
	nodes := `version: 1
nodes:
  gateway:
    os: linux
    arch: amd64
  node-a:
    os: linux
    arch: arm64
  node-b:
    os: linux
    arch: arm64
`
	if err := os.WriteFile(filepath.Join(dir, "nodes.yaml"), []byte(nodes), 0o644); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "gateway.yaml"), filepath.Join(dir, "state")
}

// joinTokenPattern matches one printed join token.
var joinTokenPattern = regexp.MustCompile(`hjt_[A-Za-z0-9_-]{43}`)

// TestNodeEnrollPrintsTokenOnce requires the command to print the token
// with the join command, exactly once, and to keep only its hash.
func TestNodeEnrollPrintsTokenOnce(t *testing.T) {
	configPath, stateDir := nodeFixture(t)
	var stdout, stderr bytes.Buffer
	code := run([]string{"node", "enroll", "node-a", "--config", configPath}, &stdout, &stderr)
	if code != exitOK {
		t.Fatalf("exit = %d, stderr: %s", code, stderr.String())
	}
	if stderr.String() != "" {
		t.Errorf("stderr = %q, want nothing: no log may carry the token", stderr.String())
	}
	matches := joinTokenPattern.FindAllString(stdout.String(), -1)
	if len(matches) != 1 {
		t.Fatalf("stdout = %q, want exactly one join token", stdout.String())
	}
	token := matches[0]
	for _, want := range []string{
		"join token for node-a",
		"one use",
		"expires ",
		"hanse-node join http://127.0.0.1:8443 " + token,
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("stdout = %q, want it to hold %q", stdout.String(), want)
		}
	}
	// The store holds only the hash, as the tokens file does for API tokens.
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		t.Fatalf("read the state directory: %v", err)
	}
	for _, entry := range entries {
		data, err := os.ReadFile(filepath.Join(stateDir, entry.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", entry.Name(), err)
		}
		if strings.Contains(string(data), token) {
			t.Errorf("%s holds the plain join token", entry.Name())
		}
	}
}

// TestNodeEnrollUnknownNode requires a node that nodes.yaml does not hold
// to fail instead of minting a token.
func TestNodeEnrollUnknownNode(t *testing.T) {
	configPath, _ := nodeFixture(t)
	var stdout, stderr bytes.Buffer
	code := run([]string{"node", "enroll", "node-z", "--config", configPath}, &stdout, &stderr)
	if code != exitFail {
		t.Fatalf("exit = %d, want %d", code, exitFail)
	}
	if stdout.String() != "" {
		t.Errorf("stdout = %q, want nothing: no token may exist for an unknown node", stdout.String())
	}
	if !strings.Contains(stderr.String(), `node "node-z" is not in`) {
		t.Errorf("stderr = %q, want the unknown-node problem", stderr.String())
	}
}

// TestNodeEnrollRefusesGatewayHost requires a token for the node that
// gateway.yaml's `node` names to be refused: that node already runs its
// handlers in the embedded agent.
func TestNodeEnrollRefusesGatewayHost(t *testing.T) {
	configPath, _ := nodeFixture(t)
	var stdout, stderr bytes.Buffer
	code := run([]string{"node", "enroll", "gateway", "--config", configPath}, &stdout, &stderr)
	if code != exitFail {
		t.Fatalf("exit = %d, want %d", code, exitFail)
	}
	if stdout.String() != "" {
		t.Errorf("stdout = %q, want nothing: no token may exist for the Gateway host", stdout.String())
	}
	for _, want := range []string{`node "gateway" is the Gateway host`, "embedded agent", "Remove `node`"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr = %q, want it to hold %q", stderr.String(), want)
		}
	}
}

// TestNodeRevokeInvalidatesCredential enrolls a node through the store,
// revokes it with the command, and requires the credential to fail
// afterwards.
func TestNodeRevokeInvalidatesCredential(t *testing.T) {
	configPath, stateDir := nodeFixture(t)
	service, err := enroll.Open(enroll.Options{StateDir: stateDir})
	if err != nil {
		t.Fatalf("open the enrollment store: %v", err)
	}
	token, err := service.IssueJoinToken("node-a")
	if err != nil {
		t.Fatalf("issue a join token: %v", err)
	}
	out, err := service.Join(token.Plain, nil)
	if err != nil {
		t.Fatalf("join node-a: %v", err)
	}
	if err := service.Close(); err != nil {
		t.Fatalf("close the enrollment store: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"node", "revoke", "node-a", "--config", configPath}, &stdout, &stderr)
	if code != exitOK {
		t.Fatalf("exit = %d, stderr: %s", code, stderr.String())
	}
	if got, want := stdout.String(), "revoked the node credential of node-a\n"; got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
	if strings.Contains(stdout.String(), out.Credential) {
		t.Error("stdout holds the credential")
	}

	service, err = enroll.Open(enroll.Options{StateDir: stateDir})
	if err != nil {
		t.Fatalf("reopen the enrollment store: %v", err)
	}
	defer func() { _ = service.Close() }()
	if _, err := service.Verify(out.Credential); !errors.Is(err, herrors.AuthenticationFailed) {
		t.Errorf("verify after revoke = %v, want authentication_failed", err)
	}
}

// TestNodeRevokeUnknownNode requires a mistyped ID to fail instead of
// reporting a revoke that did nothing.
func TestNodeRevokeUnknownNode(t *testing.T) {
	configPath, _ := nodeFixture(t)
	var stdout, stderr bytes.Buffer
	code := run([]string{"node", "revoke", "node-z", "--config", configPath}, &stdout, &stderr)
	if code != exitFail {
		t.Fatalf("exit = %d, want %d", code, exitFail)
	}
	if !strings.Contains(stderr.String(), `node "node-z" is not in`) {
		t.Errorf("stderr = %q, want the unknown-node problem", stderr.String())
	}
}

// TestNodeUsage requires a bare `node` command to print both subcommands
// and exit with the usage code.
func TestNodeUsage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"node"}, &stdout, &stderr)
	if code != exitUse {
		t.Fatalf("exit = %d, want %d", code, exitUse)
	}
	for _, want := range []string{"node enroll", "node revoke"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr = %q, want it to hold %q", stderr.String(), want)
		}
	}
}

// TestNodeEnrollRejectsUnknownFlag requires a flag that enroll does not
// take to fail, as every other command does.
func TestNodeEnrollRejectsUnknownFlag(t *testing.T) {
	configPath, _ := nodeFixture(t)
	var stdout, stderr bytes.Buffer
	code := run([]string{"node", "enroll", "node-a", "--allow", "pc.up", "--config", configPath}, &stdout, &stderr)
	if code != exitUse {
		t.Fatalf("exit = %d, want %d", code, exitUse)
	}
	if !strings.Contains(stderr.String(), "unknown flag") {
		t.Errorf("stderr = %q, want the unknown-flag problem", stderr.String())
	}
}
