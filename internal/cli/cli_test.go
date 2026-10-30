package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/achmadss/hanse/internal/version"
)

// update rewrites the golden files instead of comparing against them.
var update = flag.Bool("update", false, "rewrite the testdata golden files")

// The fixture bodies below follow docs/architecture.md section 11.1 and
// internal/api's response shapes. Every value is a placeholder: 192.0.2.0/24
// and 00:00:5e:00:53:xx are reserved for documentation.

const fixtureVersionBody = `{
  "gateway_version": "dev",
  "api_version": "v1",
  "handler_contracts": [
    "hanse.handler/v1"
  ],
  "agent_protocols": [
    "hanse.agent/v1"
  ]
}`

const fixtureCapabilitiesBody = `{
  "capabilities": [
    {
      "id": "demo",
      "name": "Demo",
      "description": "Remove the demonstration file from a node",
      "contract": "hanse.handler/v1",
      "handler": {
        "kind": "exec"
      },
      "operations": [
        {
          "id": "clean",
          "description": "Remove the demonstration file",
          "mode": "sync",
          "privileged": false
        }
      ]
    },
    {
      "id": "pc",
      "name": "PC power",
      "description": "Wake, shut down, and check machines from another node",
      "contract": "hanse.handler/v1",
      "handler": {
        "kind": "exec"
      },
      "operations": [
        {
          "id": "down",
          "description": "Shut down a node over SSH and wait until it stops answering",
          "mode": "async",
          "privileged": false
        },
        {
          "id": "status",
          "description": "Report whether a node is up",
          "mode": "sync",
          "privileged": false
        },
        {
          "id": "up",
          "description": "Wake a node with Wake-on-LAN and wait until it answers",
          "mode": "async",
          "privileged": false
        }
      ]
    }
  ]
}`

const fixtureCapabilityBody = `{
  "capability": {
    "id": "pc",
    "name": "PC power",
    "description": "Wake, shut down, and check machines from another node",
    "contract": "hanse.handler/v1",
    "handler": {
      "kind": "exec"
    },
    "operations": [
      {
        "id": "down",
        "description": "Shut down a node over SSH and wait until it stops answering",
        "mode": "async",
        "privileged": false
      },
      {
        "id": "status",
        "description": "Report whether a node is up",
        "mode": "sync",
        "privileged": false
      },
      {
        "id": "up",
        "description": "Wake a node with Wake-on-LAN and wait until it answers",
        "mode": "async",
        "privileged": false
      }
    ]
  }
}`

const fixtureNodesBody = `{
  "nodes": [
    {
      "id": "gateway",
      "os": "linux",
      "arch": "amd64",
      "roles": [
        "server"
      ],
      "attributes": {},
      "enabled": true,
      "status": "online"
    },
    {
      "id": "node-a",
      "os": "linux",
      "arch": "arm64",
      "roles": [
        "server"
      ],
      "attributes": {},
      "enabled": true,
      "status": "offline"
    },
    {
      "id": "node-c",
      "os": "windows",
      "arch": "amd64",
      "roles": [
        "pc"
      ],
      "attributes": {
        "broadcast": "192.0.2.255",
        "mac": "00:00:5e:00:53:01",
        "ssh_user": "hanse"
      },
      "enabled": true,
      "status": "offline"
    }
  ]
}`

const fixtureNodeBody = `{
  "node": {
    "id": "node-c",
    "os": "windows",
    "arch": "amd64",
    "roles": [
      "pc"
    ],
    "attributes": {
      "broadcast": "192.0.2.255",
      "mac": "00:00:5e:00:53:01",
      "ssh_user": "hanse"
    },
    "enabled": true,
    "status": "offline"
  }
}`

const fixtureNodeCapabilitiesBody = `{
  "node": "node-a",
  "capabilities": [
    {
      "id": "demo",
      "operations": [
        "clean"
      ]
    },
    {
      "id": "pc",
      "operations": [
        "down",
        "status",
        "up"
      ]
    }
  ]
}`

const fixtureSchemaBody = `{
  "additionalProperties": false,
  "properties": {
    "node": {
      "description": "The node to wake",
      "format": "hanse:node",
      "type": "string"
    },
    "wait": {
      "description": "Wait until the node answers",
      "type": "boolean"
    }
  },
  "required": [
    "node"
  ],
  "type": "object"
}`

const fixtureOptionsBody = `{
  "options": [
    {
      "value": "node-c"
    }
  ]
}`

// testToken is the bearer token the fixture server accepts.
const testToken = "test-token"

// fixtureRoutes returns the fixture bodies by request path.
func fixtureRoutes() map[string]string {
	return map[string]string{
		"/v1/version":                       fixtureVersionBody,
		"/v1/capabilities":                  fixtureCapabilitiesBody,
		"/v1/capabilities/pc":               fixtureCapabilityBody,
		"/v1/nodes":                         fixtureNodesBody,
		"/v1/nodes/node-c":                  fixtureNodeBody,
		"/v1/nodes/node-a/capabilities":     fixtureNodeCapabilitiesBody,
		"/v1/operations/pc/up/schema":       fixtureSchemaBody,
		"/v1/operations/pc/up/options/node": fixtureOptionsBody,
	}
}

// newFixtureGateway starts a Gateway fixture. It requires the test token
// and answers unknown paths with the unknown_capability error.
func newFixtureGateway(t *testing.T) *httptest.Server {
	t.Helper()
	routes := fixtureRoutes()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Authorization") != "Bearer "+testToken {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"code":"authentication_failed","message":"the token is wrong"}`))
			return
		}
		body, ok := routes[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"code":"unknown_capability","message":"capability does not exist"}`))
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// writeTestConfig writes a CLI configuration file for a fixture Gateway.
func writeTestConfig(t *testing.T, path, gatewayURL, token string) {
	t.Helper()
	if err := saveConfig(path, config{GatewayURL: gatewayURL, Token: token}); err != nil {
		t.Fatalf("write test config: %v", err)
	}
}

// runCLI runs one invocation against a temporary configuration.
func runCLI(t *testing.T, args []string, configPath string) (stdout, stderr string, code int) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = Run(context.Background(), args, Options{
		Stdin:      strings.NewReader(""),
		Stdout:     &out,
		Stderr:     &errOut,
		ConfigPath: configPath,
	})
	return out.String(), errOut.String(), code
}

// checkGolden compares got with testdata/<name>.golden, or rewrites the
// file under -update. CRLF is normalized so a Windows checkout compares
// equal to the LF output.
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
		t.Fatalf("read %s (run go test ./internal/cli -update to create it): %v", path, err)
	}
	want = bytes.ReplaceAll(want, []byte("\r\n"), []byte("\n"))
	if !bytes.Equal(want, got) {
		t.Errorf("%s differs from the CLI output.\n--- want ---\n%s\n--- got ---\n%s", path, want, got)
	}
}

// TestGoldenJSON covers every discovery command's JSON output.
func TestGoldenJSON(t *testing.T) {
	srv := newFixtureGateway(t)
	configPath := filepath.Join(t.TempDir(), "hanse", "config.json")
	writeTestConfig(t, configPath, srv.URL, testToken)

	cases := []struct {
		golden string
		args   []string
	}{
		{"capabilities", []string{"capabilities"}},
		{"capability", []string{"capabilities", "show", "pc"}},
		{"nodes", []string{"nodes"}},
		{"node", []string{"nodes", "show", "node-c"}},
		{"node_capabilities", []string{"nodes", "capabilities", "node-a"}},
		{"schema", []string{"schema", "pc", "up"}},
		{"options", []string{"options", "pc", "up", "node"}},
	}
	for _, tc := range cases {
		t.Run(tc.golden, func(t *testing.T) {
			args := append(append([]string{}, tc.args...), "--format", "json")
			stdout, stderr, code := runCLI(t, args, configPath)
			if code != 0 {
				t.Fatalf("%v: exit code %d, stderr %s", args, code, stderr)
			}
			if stderr != "" {
				t.Errorf("%v: unexpected stderr %q", args, stderr)
			}
			if strings.Contains(stdout, "\x1b[") {
				t.Errorf("%v: JSON output contains a terminal escape", args)
			}
			checkGolden(t, tc.golden, []byte(stdout))
		})
	}
}

// TestJSONDeterministic fetches every discovery command twice and requires
// byte-identical output.
func TestJSONDeterministic(t *testing.T) {
	srv := newFixtureGateway(t)
	configPath := filepath.Join(t.TempDir(), "hanse", "config.json")
	writeTestConfig(t, configPath, srv.URL, testToken)

	commands := [][]string{
		{"capabilities"},
		{"capabilities", "show", "pc"},
		{"nodes"},
		{"nodes", "show", "node-c"},
		{"nodes", "capabilities", "node-a"},
		{"schema", "pc", "up"},
		{"options", "pc", "up", "node"},
	}
	for _, command := range commands {
		args := append(append([]string{}, command...), "--format", "json")
		first, _, firstCode := runCLI(t, args, configPath)
		second, _, secondCode := runCLI(t, args, configPath)
		if firstCode != 0 || secondCode != 0 {
			t.Fatalf("%v: exit codes %d and %d", command, firstCode, secondCode)
		}
		if first != second {
			t.Errorf("%v: two runs differ\n--- first ---\n%s\n--- second ---\n%s", command, first, second)
		}
	}
}

// TestTextOutput covers the default format.
func TestTextOutput(t *testing.T) {
	srv := newFixtureGateway(t)
	configPath := filepath.Join(t.TempDir(), "hanse", "config.json")
	writeTestConfig(t, configPath, srv.URL, testToken)

	cases := []struct {
		name string
		args []string
		want []string
	}{
		{"capabilities", []string{"capabilities"}, []string{"ID", "NAME", "HANDLER", "pc", "demo"}},
		{"capability", []string{"capabilities", "show", "pc"}, []string{"ID:", "pc", "Operations:", "up"}},
		{"nodes", []string{"nodes"}, []string{"ID", "OS", "STATUS", "node-c"}},
		{"node", []string{"nodes", "show", "node-c"}, []string{"Attributes:", "mac", "00:00:5e:00:53:01"}},
		{"node capabilities", []string{"nodes", "capabilities", "node-a"}, []string{"Node: node-a", "CAPABILITY", "pc"}},
		{"schema", []string{"schema", "pc", "up"}, []string{"\"properties\"", "\"required\""}},
		{"options", []string{"options", "pc", "up", "node"}, []string{"VALUE", "node-c"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr, code := runCLI(t, tc.args, configPath)
			if code != 0 {
				t.Fatalf("%v: exit code %d, stderr %s", tc.args, code, stderr)
			}
			if strings.Contains(stdout, "\x1b[") {
				t.Errorf("%v: text output contains a terminal escape", tc.args)
			}
			for _, want := range tc.want {
				if !strings.Contains(stdout, want) {
					t.Errorf("%v: output does not contain %q:\n%s", tc.args, want, stdout)
				}
			}
		})
	}
}

// TestVersionMismatchWarns requires a warning on stderr when the Gateway
// speaks another API version, with no effect on the output or exit code.
func TestVersionMismatchWarns(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/version" {
			_, _ = w.Write([]byte(`{"gateway_version":"dev","api_version":"v9","handler_contracts":[],"agent_protocols":[]}`))
			return
		}
		_, _ = w.Write([]byte(fixtureCapabilitiesBody))
	}))
	t.Cleanup(srv.Close)

	configPath := filepath.Join(t.TempDir(), "hanse", "config.json")
	writeTestConfig(t, configPath, srv.URL, testToken)

	stdout, stderr, code := runCLI(t, []string{"capabilities", "--format", "json"}, configPath)
	if code != 0 {
		t.Fatalf("capabilities: exit code %d, stderr %s", code, stderr)
	}
	if !strings.Contains(stderr, "v9") || !strings.Contains(stderr, "v1") {
		t.Errorf("capabilities: stderr %q, want an API version warning", stderr)
	}
	if !strings.Contains(stderr, "warning") {
		t.Errorf("capabilities: stderr %q, want a warning", stderr)
	}
	if !strings.Contains(stdout, `"capabilities"`) {
		t.Errorf("capabilities: unexpected output %q", stdout)
	}
}

// TestLoginSavesConfig requires the login command to store the URL and
// token with private permissions and to never print the token.
func TestLoginSavesConfig(t *testing.T) {
	srv := newFixtureGateway(t)
	configPath := filepath.Join(t.TempDir(), "hanse", "config.json")

	stdout, stderr, code := runCLI(t, []string{
		"login", "--url", srv.URL, "--token", testToken, "--format", "json",
	}, configPath)
	if code != 0 {
		t.Fatalf("login: exit code %d, stderr %s", code, stderr)
	}
	if strings.Contains(stdout, testToken) || strings.Contains(stderr, testToken) {
		t.Error("login: the token was printed")
	}
	var result struct {
		GatewayURL string `json:"gateway_url"`
		APIVersion string `json:"api_version"`
	}
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("login: output is not JSON: %v (%s)", err, stdout)
	}
	if result.GatewayURL != srv.URL {
		t.Errorf("login: gateway_url %q, want %q", result.GatewayURL, srv.URL)
	}
	if result.APIVersion != "v1" {
		t.Errorf("login: api_version %q, want v1", result.APIVersion)
	}

	cfg, err := loadConfig(configPath)
	if err != nil {
		t.Fatalf("load saved config: %v", err)
	}
	if cfg.GatewayURL != srv.URL || cfg.Token != testToken {
		t.Errorf("saved config: got %+v", cfg)
	}
	if runtime.GOOS != "windows" {
		checkMode(t, configPath, fileMode)
		checkMode(t, filepath.Dir(configPath), dirMode)
	}

	// The saved credentials must work for the next command.
	after, afterErr, afterCode := runCLI(t, []string{"capabilities", "--format", "json"}, configPath)
	if afterCode != 0 {
		t.Fatalf("capabilities after login: exit code %d, stderr %s", afterCode, afterErr)
	}
	if !strings.Contains(after, `"capabilities"`) {
		t.Errorf("capabilities after login: unexpected output %q", after)
	}
}

// TestLoginReadsTokenFromStdin covers a piped token.
func TestLoginReadsTokenFromStdin(t *testing.T) {
	srv := newFixtureGateway(t)
	configPath := filepath.Join(t.TempDir(), "hanse", "config.json")

	var out, errOut bytes.Buffer
	code := Run(context.Background(), []string{"login", "--url", srv.URL}, Options{
		Stdin:      strings.NewReader(testToken + "\n"),
		Stdout:     &out,
		Stderr:     &errOut,
		ConfigPath: configPath,
	})
	if code != 0 {
		t.Fatalf("login: exit code %d, stderr %s", code, errOut.String())
	}
	if strings.Contains(out.String(), testToken) || strings.Contains(errOut.String(), testToken) {
		t.Error("login: the token was printed")
	}
	cfg, err := loadConfig(configPath)
	if err != nil {
		t.Fatalf("load saved config: %v", err)
	}
	if cfg.Token != testToken {
		t.Errorf("saved token: got %q", cfg.Token)
	}
}

// TestLoginRefusesTerminalToken requires an explicit token source when the
// input is interactive, because the terminal would echo it.
func TestLoginRefusesTerminalToken(t *testing.T) {
	srv := newFixtureGateway(t)
	configPath := filepath.Join(t.TempDir(), "hanse", "config.json")

	var out, errOut bytes.Buffer
	code := Run(context.Background(), []string{"login", "--url", srv.URL}, Options{
		Stdin:           strings.NewReader(testToken),
		StdinIsTerminal: true,
		Stdout:          &out,
		Stderr:          &errOut,
		ConfigPath:      configPath,
	})
	if code != 2 {
		t.Fatalf("login: exit code %d, want 2; stderr %s", code, errOut.String())
	}
	if _, err := os.Stat(configPath); !os.IsNotExist(err) {
		t.Errorf("login: configuration file exists after a usage error")
	}
}

// TestLoginDoesNotSaveOnAuthFailure requires a rejected token to leave no
// configuration behind.
func TestLoginDoesNotSaveOnAuthFailure(t *testing.T) {
	srv := newFixtureGateway(t)
	configPath := filepath.Join(t.TempDir(), "hanse", "config.json")

	_, stderr, code := runCLI(t, []string{"login", "--url", srv.URL, "--token", "wrong-token"}, configPath)
	if code != 28 {
		t.Fatalf("login: exit code %d, want 28; stderr %s", code, stderr)
	}
	if !strings.Contains(stderr, "authentication_failed") {
		t.Errorf("login: stderr %q, want the authentication_failed code", stderr)
	}
	if _, err := os.Stat(configPath); !os.IsNotExist(err) {
		t.Errorf("login: configuration file exists after an authentication failure")
	}
}

// TestAPIErrorExitCodes maps Gateway errors to the documented exit codes.
func TestAPIErrorExitCodes(t *testing.T) {
	srv := newFixtureGateway(t)
	configPath := filepath.Join(t.TempDir(), "hanse", "config.json")

	t.Run("unknown capability", func(t *testing.T) {
		writeTestConfig(t, configPath, srv.URL, testToken)
		stdout, stderr, code := runCLI(t, []string{"capabilities", "show", "nope", "--format", "json"}, configPath)
		if code != 20 {
			t.Fatalf("exit code %d, want 20; stderr %s", code, stderr)
		}
		if stdout != "" {
			t.Errorf("stdout %q, want no output", stdout)
		}
		if !strings.Contains(stderr, "unknown_capability") {
			t.Errorf("stderr %q, want the unknown_capability code", stderr)
		}
	})

	t.Run("authentication failed", func(t *testing.T) {
		writeTestConfig(t, configPath, srv.URL, "wrong-token")
		_, stderr, code := runCLI(t, []string{"nodes"}, configPath)
		if code != 28 {
			t.Fatalf("exit code %d, want 28; stderr %s", code, stderr)
		}
		if !strings.Contains(stderr, "authentication_failed") {
			t.Errorf("stderr %q, want the authentication_failed code", stderr)
		}
	})

	t.Run("internal", func(t *testing.T) {
		broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/v1/version" {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(fixtureVersionBody))
				return
			}
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("gateway exploded"))
		}))
		t.Cleanup(broken.Close)
		writeTestConfig(t, configPath, broken.URL, testToken)
		_, stderr, code := runCLI(t, []string{"capabilities"}, configPath)
		if code != 1 {
			t.Fatalf("exit code %d, want 1; stderr %s", code, stderr)
		}
		if !strings.Contains(stderr, "internal") {
			t.Errorf("stderr %q, want the internal code", stderr)
		}
	})
}

// TestNoConfig requires a clear message and exit code 28 when the user has
// not logged in.
func TestNoConfig(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "hanse", "config.json")
	_, stderr, code := runCLI(t, []string{"capabilities"}, configPath)
	if code != 28 {
		t.Fatalf("exit code %d, want 28; stderr %s", code, stderr)
	}
	if !strings.Contains(stderr, "not logged in") || !strings.Contains(stderr, "hanse login") {
		t.Errorf("stderr %q, want a login hint", stderr)
	}
}

// TestUsageErrors covers command line mistakes and help.
func TestUsageErrors(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "hanse", "config.json")
	cases := [][]string{
		{},
		{"bogus"},
		{"schema", "pc"},
		{"options", "pc", "up"},
		{"capabilities", "--format", "yaml"},
		{"nodes", "show"},
	}
	for _, args := range cases {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			_, stderr, code := runCLI(t, args, configPath)
			if code != 2 {
				t.Fatalf("%v: exit code %d, want 2; stderr %s", args, code, stderr)
			}
			if stderr == "" {
				t.Errorf("%v: no error message", args)
			}
		})
	}

	stdout, stderr, code := runCLI(t, []string{"--help"}, configPath)
	if code != 0 {
		t.Fatalf("--help: exit code %d, want 0; stderr %s", code, stderr)
	}
	if !strings.Contains(stdout, "usage: hanse") {
		t.Errorf("--help: stdout %q, want usage", stdout)
	}
}

// TestVersionCommand covers the CLI version command.
func TestVersionCommand(t *testing.T) {
	stdout, stderr, code := runCLI(t, []string{"version", "--format", "json"}, "")
	if code != 0 {
		t.Fatalf("version: exit code %d, stderr %s", code, stderr)
	}
	want := "{\n  \"version\": \"" + version.Version + "\"\n}\n"
	if stdout != want {
		t.Errorf("version: got %q, want %q", stdout, want)
	}
}

// TestParseArgs covers flags between positionals and the "--" terminator.
func TestParseArgs(t *testing.T) {
	fs := newFlagSet("test")
	formatValue := formatFlag(fs)
	positional, err := parseArgs(fs, []string{"pc", "--format", "json", "up"})
	if err != nil {
		t.Fatalf("parseArgs: %v", err)
	}
	if *formatValue != "json" {
		t.Errorf("format %q, want json", *formatValue)
	}
	if strings.Join(positional, ",") != "pc,up" {
		t.Errorf("positional %v, want [pc up]", positional)
	}

	fs = newFlagSet("test")
	formatValue = formatFlag(fs)
	positional, err = parseArgs(fs, []string{"pc", "--", "-weird", "--format", "json"})
	if err != nil {
		t.Fatalf("parseArgs after --: %v", err)
	}
	if *formatValue != "text" {
		t.Errorf("format after -- %q, want the default", *formatValue)
	}
	if strings.Join(positional, ",") != "pc,-weird,--format,json" {
		t.Errorf("positional after -- %v, want every later argument", positional)
	}
}

// TestOneLineStripsControls requires table cells to carry no control bytes.
func TestOneLineStripsControls(t *testing.T) {
	got := oneLine("a\x1b[31mb\nc\td\x00")
	want := "a[31mb c d"
	if got != want {
		t.Errorf("oneLine() = %q, want %q", got, want)
	}
}

// checkMode requires a file or directory mode.
func checkMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Errorf("%s: mode %o, want %o", path, got, want)
	}
}
