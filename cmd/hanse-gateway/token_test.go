package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// tokenDir returns the tokens file path of a fixture.
func tokenFixture(t *testing.T) (configPath, tokensFile string) {
	t.Helper()
	dir := configFixture(t, "")
	return filepath.Join(dir, "gateway.yaml"), filepath.Join(dir, "state", "tokens.json")
}

// fixturePath returns just the config path of a fresh fixture.
func tokenFixtureG(t *testing.T) string {
	t.Helper()
	p, _ := tokenFixture(t)
	return p
}

func TestTokenAddPrintsTokenOnce(t *testing.T) {
	configPath, tokensFile := tokenFixture(t)
	var stdout, stderr bytes.Buffer
	code := run([]string{"token", "add", "ci", "--config", configPath}, &stdout, &stderr)
	if code != exitOK {
		t.Fatalf("exit = %d, stderr: %s", code, stderr.String())
	}
	stdoutLines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(stdoutLines) != 1 {
		t.Fatalf("stdout = %q, want exactly one line holding the token", stdout.String())
	}
	token := stdoutLines[0]
	if !strings.HasPrefix(token, "hst_") {
		t.Errorf("token %q misses the hst_ prefix", token)
	}
	data, err := os.ReadFile(tokensFile)
	if err != nil {
		t.Fatalf("read the tokens file: %v", err)
	}
	if strings.Contains(string(data), token) {
		t.Errorf("the tokens file holds the plain token")
	}
	if stderr.String() != "" {
		t.Errorf("stderr = %q, want nothing: no log may carry the token", stderr.String())
	}
}

func TestTokenAddTwiceFails(t *testing.T) {
	configPath, _ := tokenFixture(t)
	var stdout, stderr bytes.Buffer
	if run([]string{"token", "add", "ci", "--config", configPath}, &stdout, &stderr) != exitOK {
		t.Fatal("first add failed")
	}
	var secondStdout, secondStderr bytes.Buffer
	code := run([]string{"token", "add", "ci", "--config", configPath}, &secondStdout, &secondStderr)
	if code != exitFail {
		t.Fatalf("exit = %d, want %d", code, exitFail)
	}
	if !strings.Contains(secondStderr.String(), "already exists") {
		t.Errorf("stderr = %q, want the duplicate-name problem", secondStderr.String())
	}
}

func TestTokenListAndRevoke(t *testing.T) {
	configPath, _ := tokenFixture(t)
	var stdout, stderr bytes.Buffer
	if run([]string{"token", "add", "ci", "--allow", "pc.up", "--config", configPath}, &stdout, &stderr) != exitOK {
		t.Fatal("add failed")
	}
	stdout.Reset()
	stderr.Reset()
	code := run([]string{"token", "list", "--config", configPath}, &stdout, &stderr)
	if code != exitOK {
		t.Fatalf("list exit = %d, stderr: %s", code, stderr.String())
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("list = %q, want a header and one row", stdout.String())
	}
	if lines[0] != "name\tcreated\tallow" {
		t.Errorf("header = %q", lines[0])
	}
	if !strings.Contains(lines[1], "ci") || !strings.Contains(lines[1], "pc.up") {
		t.Errorf("row = %q, want the token name and its allow entry", lines[1])
	}
	if strings.Contains(stdout.String(), "hst_") {
		t.Errorf("token list must not print a plain token")
	}

	stdout.Reset()
	stderr.Reset()
	code = run([]string{"token", "revoke", "ci", "--config", configPath}, &stdout, &stderr)
	if code != exitOK {
		t.Fatalf("revoke exit = %d, stderr: %s", code, stderr.String())
	}
	stdout.Reset()
	code = run([]string{"token", "list", "--config", configPath}, &stdout, &stderr)
	if code != exitOK {
		t.Fatalf("list exit = %d", code)
	}
	if strings.Contains(stdout.String(), "ci\t") {
		t.Errorf("the revoked token is still listed: %q", stdout.String())
	}

	stderr.Reset()
	code = run([]string{"token", "revoke", "ci", "--config", configPath}, &stdout, &stderr)
	if code != exitFail {
		t.Errorf("second revoke exit = %d, want %d", code, exitFail)
	}
	if !strings.Contains(stderr.String(), "does not exist") {
		t.Errorf("stderr = %q, want the unknown-name problem", stderr.String())
	}
}

func TestTokenErrors(t *testing.T) {
	configPath, _ := tokenFixture(t)
	cases := [][]string{
		{"token"},                                      // no subcommand
		{"token", "dance"},                             // unknown subcommand
		{"token", "add", "--config", configPath},       // no name
		{"token", "add", "CI", "--config", configPath}, // invalid name
		{"token", "add", "x", "--allow", "ALL", "--config", configPath},               // bad allow entry
		{"token", "revoke", "--config", configPath},                                   // no name
		{"token", "list", "extra", "--config", configPath},                            // too many names
		{"token", "add", "x", "--allow=", "--config", configPath},                     // empty allow value
		{"token", "add", "x", "--allow", "", "--config", configPath},                  // empty allow value
		{"token", "add", "x", "--allow=pc.up", "--allow", "", "--config", configPath}, // empty again
	}
	for _, args := range cases {
		var stdout, stderr bytes.Buffer
		code := run(append([]string{}, args...), &stdout, &stderr)
		if code == exitOK {
			t.Errorf("run(%v) = exitOK, want a failure", args)
		}
	}
}

// TestTokenConfigForms shows the three forms the review asked for: the
// default path, --config before `token`, and --config after it.
func TestTokenConfigForms(t *testing.T) {
	t.Run("default path", func(t *testing.T) {
		// A run with no --config at all must resolve the OS default,
		// which is not this fixture: it fails as "cannot read the
		// default file", not with an empty path's odd message.
		var stdout, stderr bytes.Buffer
		code := run([]string{"token", "add", "ci"}, &stdout, &stderr)
		if code != exitFail {
			t.Errorf("exit = %d, want %d", code, exitFail)
		}
		if strings.Contains(stderr.String(), "open : no such file") {
			t.Errorf("stderr = %q, want a real default path, not an empty one", stderr.String())
		}
	})
	t.Run("outer --config", func(t *testing.T) {
		configPath, _ := tokenFixture(t)
		var stdout, stderr bytes.Buffer
		code := run([]string{"--config", configPath, "token", "add", "ci"}, &stdout, &stderr)
		if code != exitOK {
			t.Fatalf("exit = %d, stderr: %s", code, stderr.String())
		}
		if !strings.HasPrefix(strings.TrimSpace(stdout.String()), "hst_") {
			t.Errorf("stdout = %q, want a token", stdout.String())
		}
	})
	t.Run("inner --config", func(t *testing.T) {
		configPath, _ := tokenFixture(t)
		var stdout, stderr bytes.Buffer
		if run([]string{"token", "add", "ci", "--config", configPath}, &stdout, &stderr) != exitOK {
			t.Fatal("add failed")
		}
		stdout.Reset()
		code := run([]string{"--config", configPath, "token", "revoke", "ci"}, &stdout, &stderr)
		if code != exitOK {
			t.Fatalf("exit = %d, stderr: %s", code, stderr.String())
		}
		if !strings.Contains(stdout.String(), "revoked ci") {
			t.Errorf("stdout = %q, want the revoke confirmation", stdout.String())
		}
	})
}

func TestTokenAddWithAllowStoresOnlyAllow(t *testing.T) {
	configPath, tokensFile := tokenFixture(t)
	var stdout, stderr bytes.Buffer
	if run([]string{"token", "add", "ci", "--allow", "pc.up,monitor", "--config", configPath}, &stdout, &stderr) != exitOK {
		t.Fatal("add failed")
	}
	data, err := os.ReadFile(tokensFile)
	if err != nil {
		t.Fatalf("read the tokens file: %v", err)
	}
	var f struct {
		Version int `json:"version"`
		Tokens  []struct {
			Name  string   `json:"name"`
			Hash  string   `json:"hash"`
			Allow []string `json:"allow"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatalf("parse the tokens file: %v (%q)", err, data)
	}
	if f.Version != 1 || len(f.Tokens) != 1 {
		t.Fatalf("file = %q", data)
	}
	r := f.Tokens[0]
	if r.Name != "ci" || len(r.Allow) != 2 || r.Allow[0] != "pc.up" || r.Allow[1] != "monitor" {
		t.Errorf("record = %+v", r)
	}
	if len(r.Hash) != 64 {
		t.Errorf("hash length = %d, want 64 hex characters of a SHA-256", len(r.Hash))
	}
}

func TestTokenAddWithBadConfigFails(t *testing.T) {
	dir := configFixture(t, "timeuouts:\n  operation: 5m\n")
	var stdout, stderr bytes.Buffer
	code := run([]string{"token", "add", "ci", "--config", filepath.Join(dir, "gateway.yaml")}, &stdout, &stderr)
	if code != exitFail {
		t.Fatalf("exit = %d, want %d", code, exitFail)
	}
	if !strings.Contains(stderr.String(), "field timeuouts not found") {
		t.Errorf("stderr = %q, want the configuration problem", stderr.String())
	}
}

func TestTokenAddWritesTokensFileNonWindowsPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode bits do not apply on Windows")
	}
	configPath, tokensFile := tokenFixture(t)
	var stdout, stderr bytes.Buffer
	if run([]string{"token", "add", "ci", "--config", configPath}, &stdout, &stderr) != exitOK {
		t.Fatal("add failed")
	}
	info, err := os.Stat(tokensFile)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("the tokens file mode = %s, want 0600", mode)
	}
}

func TestTokenUsageWithoutSubcommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"token", "add"}, &stdout, &stderr)
	if code != exitUse {
		t.Errorf("exit = %d, want %d", code, exitUse)
	}
	if !strings.Contains(stderr.String(), "usage: hanse-gateway token add") {
		t.Errorf("stderr = %q, want the subcommand usage", stderr.String())
	}
}

func TestTokenInvalidNameExitsOne(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"token", "add", "invalid name", "--config", tokenFixtureG(t)}, &stdout, &stderr)
	if code != exitFail {
		t.Errorf("exit = %d, want %d", code, exitFail)
	}
	if !strings.Contains(stderr.String(), "starting with a letter") {
		t.Errorf("stderr = %q, want the invalid-name problem", stderr.String())
	}
}
