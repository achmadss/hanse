package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// writeAgentConfig writes one agent.yaml and returns its path.
func writeAgentConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agent.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write agent.yaml: %v", err)
	}
	return path
}

// TestLoadAgentDefaults covers the defaults of docs/configuration.md
// section 7.1.
func TestLoadAgentDefaults(t *testing.T) {
	path := writeAgentConfig(t, "version: 1\ngateway_url: https://gateway.example.com\n")
	cfg, problems, err := LoadAgent(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(problems) != 0 {
		t.Fatalf("problems: %+v", problems)
	}
	if cfg.StateDir != filepath.FromSlash(defaultAgentStateDir()) {
		t.Errorf("state dir %q", cfg.StateDir)
	}
	if *cfg.MaxConcurrentRuns != 16 {
		t.Errorf("max_concurrent_runs %d, want 16", *cfg.MaxConcurrentRuns)
	}
	if cfg.BufferLimit.Value != 10<<20 {
		t.Errorf("buffer_limit %d, want 10MiB", cfg.BufferLimit.Value)
	}
	if cfg.Log.Level != "info" || cfg.Log.Format != "text" {
		t.Errorf("log %q/%q, want info/text", cfg.Log.Level, cfg.Log.Format)
	}
	if *cfg.Elevation.Lockout.Attempts != 5 {
		t.Errorf("lockout attempts %d, want 5", *cfg.Elevation.Lockout.Attempts)
	}
	if cfg.Elevation.Lockout.Window.Value != 10*time.Minute {
		t.Errorf("lockout window %s, want 10m", cfg.Elevation.Lockout.Window.Value)
	}
	if cfg.Elevation.Lockout.Duration.Value != 10*time.Minute {
		t.Errorf("lockout duration %s, want 10m", cfg.Elevation.Lockout.Duration.Value)
	}
}

// TestLoadAgentExample loads the shipped example, with its default state
// directory replaced so the test never touches /var/lib.
func TestLoadAgentExample(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "config", "agent.example.yaml"))
	if err != nil {
		t.Fatalf("read example: %v", err)
	}
	body := strings.Replace(string(data), "state_dir: /var/lib/hanse-node", "state_dir: ./state", 1)
	path := writeAgentConfig(t, body)
	cfg, problems, err := LoadAgent(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(problems) != 0 {
		t.Fatalf("problems: %+v", problems)
	}
	want := filepath.Join(filepath.Dir(path), "state")
	if cfg.StateDir != want {
		t.Errorf("state dir %q, want %q", cfg.StateDir, want)
	}
	if cfg.CAFile != "" {
		t.Errorf("ca_file %q, want empty", cfg.CAFile)
	}
}

// TestLoadAgentRejectsUnsafeGatewayURL covers the https-unless-loopback
// rule.
func TestLoadAgentRejectsUnsafeGatewayURL(t *testing.T) {
	cases := map[string]bool{
		"http://gateway.example.com":  true,
		"http://192.0.2.10:8443":      true,
		"https://gateway.example.com": false,
		"http://127.0.0.1:8443":       false,
		"http://localhost:8443":       false,
		"ftp://gateway.example.com":   true,
	}
	for gatewayURL, wantProblem := range cases {
		t.Run(gatewayURL, func(t *testing.T) {
			path := writeAgentConfig(t, "version: 1\ngateway_url: "+gatewayURL+"\n")
			_, problems, err := LoadAgent(path)
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if wantProblem && len(problems) == 0 {
				t.Fatalf("%s was accepted", gatewayURL)
			}
			if !wantProblem && len(problems) != 0 {
				t.Fatalf("%s was rejected: %+v", gatewayURL, problems)
			}
		})
	}
}

// TestLoadAgentRejectsUnknownKey keeps a misspelled key an error.
func TestLoadAgentRejectsUnknownKey(t *testing.T) {
	path := writeAgentConfig(t, "version: 1\ngateway_url: https://gateway.example.com\nstate_dirr: /tmp/x\n")
	_, problems, err := LoadAgent(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(problems) == 0 {
		t.Fatal("an unknown key was accepted")
	}
}

// TestLoadAgentRejectsWorldWritableFile proves the agent refuses a
// configuration file other users can write (docs/configuration.md §7).
func TestLoadAgentRejectsWorldWritableFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX modes only")
	}
	path := writeAgentConfig(t, "version: 1\ngateway_url: https://gateway.example.com\n")
	if err := os.Chmod(path, 0o666); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	_, problems, err := LoadAgent(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(problems) == 0 {
		t.Fatal("a world-writable agent.yaml was accepted")
	}
}

// TestLoadAgentRejectsBadValues covers the field ranges.
func TestLoadAgentRejectsBadValues(t *testing.T) {
	cases := map[string]string{
		"version":             "version: 2\ngateway_url: https://gateway.example.com\n",
		"max_concurrent":      "version: 1\ngateway_url: https://gateway.example.com\nmax_concurrent_runs: 0\n",
		"buffer_limit":        "version: 1\ngateway_url: https://gateway.example.com\nbuffer_limit: 0B\n",
		"log_level":           "version: 1\ngateway_url: https://gateway.example.com\nlog:\n  level: loud\n",
		"log_format":          "version: 1\ngateway_url: https://gateway.example.com\nlog:\n  format: xml\n",
		"elevation_ops":       "version: 1\ngateway_url: https://gateway.example.com\nelevation:\n  operations: [bad]\n",
		"lockout_attempts":    "version: 1\ngateway_url: https://gateway.example.com\nelevation:\n  lockout:\n    attempts: 0\n",
		"lockout_window":      "version: 1\ngateway_url: https://gateway.example.com\nelevation:\n  lockout:\n    window: 0s\n",
		"missing_gateway":     "version: 1\n",
		"env_allow_has_eq":    "version: 1\ngateway_url: https://gateway.example.com\nhandlers:\n  env_allow: [A=B]\n",
		"gateway_url_query":   "version: 1\ngateway_url: https://gateway.example.com/?x=1\n",
		"gateway_url_user":    "version: 1\ngateway_url: https://user@gateway.example.com\n",
		"gateway_url_missing": "version: 1\ngateway_url: \"\"\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			path := writeAgentConfig(t, body)
			_, problems, err := LoadAgent(path)
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if len(problems) == 0 {
				t.Fatalf("%s was accepted", name)
			}
		})
	}
}
