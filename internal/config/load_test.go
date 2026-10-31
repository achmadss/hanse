package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// entry is one file to write into a test tree.
type entry struct {
	content string
	mode    os.FileMode // 0 means 0644
}

// base templates -------------------------------------------------------------

const gwBase = `version: 1
node: gateway
public_url: http://127.0.0.1:8443
listen: 127.0.0.1:8443
paths:
  nodes: nodes.yaml
  routes: routes.yaml
  capabilities:
    - %DIR%/capabilities
  credentials_dir: %DIR%/credentials
  state_dir: %DIR%/state
`

const nodesBase = `version: 1
nodes:
  gateway:
    os: linux
    arch: amd64
  node-a:
    os: linux
    arch: amd64
  node-b:
    os: linux
    arch: amd64
  node-c:
    os: linux
    arch: amd64
    enabled: false
`

const routesBase = `version: 1
capabilities:
  demo:
    executors: "*"
    default_executor: gateway
  pc:
    executors: [node-a, node-b]
    default_executor: node-a
`

// setup writes the tree into a fresh temporary directory and replaces
// %DIR% in every file with that directory.
func setup(t *testing.T, files map[string]entry, dirs map[string]os.FileMode) string {
	t.Helper()
	dir := t.TempDir()
	for name, e := range files {
		path, content := filepath.Join(dir, name), strings.ReplaceAll(e.content, "%DIR%", dir)
		mode := e.mode
		if mode == 0 {
			mode = 0o644
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	for name, mode := range dirs {
		path := filepath.Join(dir, strings.ReplaceAll(name, "%DIR%", dir))
		if err := os.MkdirAll(path, mode); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// setMode re-applies an explicit mode, because WriteFile only honors it
// when the file is new and the process umask can tighten it.
func setMode(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

// assertProblems checks got against want: every want matches exactly one
// problem by substring, and no problem goes unmatched.
func assertProblems(t *testing.T, got Problems, want []string) {
	t.Helper()
	matched := make([]bool, len(got))
	for _, w := range want {
		found := false
		for i, p := range got {
			if !matched[i] && strings.Contains(p.Error(), w) {
				matched[i] = true
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing problem matching %q\ngot:\n%s", w, dumpProblems(got))
		}
	}
	for i, m := range matched {
		if !m {
			t.Errorf("unexpected problem: %s", got[i].Error())
		}
	}
	if len(want) != len(got) {
		t.Errorf("problem count = %d, want %d\ngot:\n%s", len(got), len(want), dumpProblems(got))
	}
}

func dumpProblems(ps Problems) string {
	var b strings.Builder
	for _, p := range ps {
		b.WriteString("  " + p.Error() + "\n")
	}
	return b.String()
}

// baseFiles is the valid base of every scenario; a scenario overrides
// named entries or deletes one.
func baseFiles() map[string]entry {
	return map[string]entry{
		"gateway.yaml": {content: gwBase},
		"nodes.yaml":   {content: nodesBase},
		"routes.yaml":  {content: routesBase},
		"tls/cert.pem": {content: "not a real certificate\n"},
		"tls/key.pem":  {content: "not a real key\n", mode: 0o600},
	}
}

func baseDirs() map[string]os.FileMode {
	return map[string]os.FileMode{
		"credentials":  0o700,
		"state":        0o750,
		"capabilities": 0o755,
	}
}

func baseGatewayFiles() map[string]os.FileMode {
	return map[string]os.FileMode{
		"tls": 0o700,
	}
}

// withGateway adds a snippet at the end of gateway.yaml.
func withGateway(snippet string) map[string]entry {
	out := baseFiles()
	out["gateway.yaml"] = entry{content: out["gateway.yaml"].content + snippet}
	return out
}

// run loads %DIR%/gateway.yaml and returns the problems found.
func runLoad(t *testing.T, dir string) Problems {
	t.Helper()
	_, problems, err := Load(filepath.Join(dir, "gateway.yaml"))
	if err != nil {
		t.Fatalf("Load: unexpected hard error: %v", err)
	}
	return problems
}

// loadValidBaseline is used by every valid-scenario test that only needs
// the base configuration.
func loadValidBaseline(t *testing.T) string {
	dir := setup(t, baseFiles(), mergeDirs(baseDirs(), baseGatewayFiles()))
	assertProblems(t, runLoad(t, dir), nil)
	return dir
}

func mergeDirs(a, b map[string]os.FileMode) map[string]os.FileMode {
	out := make(map[string]os.FileMode, len(a)+len(b))
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

func TestLoadValidBase(t *testing.T) {
	dir := loadValidBaseline(t)

	// Spot-check the filled defaults and resolved paths.
	cfg, _, err := Load(filepath.Join(dir, "gateway.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if *cfg.Gateway.Listen != "127.0.0.1:8443" {
		t.Errorf("listen default = %q, want 127.0.0.1:8443", *cfg.Gateway.Listen)
	}
	if cfg.Gateway.Timeouts.Operation == nil || cfg.Gateway.Timeouts.Operation.Value.String() != "5m0s" {
		t.Errorf("timeouts.operation default = %v, want 5m", cfg.Gateway.Timeouts.Operation)
	}
	if cfg.Gateway.History.Retention == nil || cfg.Gateway.History.Retention.Value != 30*24*3600e9 {
		t.Errorf("history.retention default = %v, want 30d", cfg.Gateway.History.Retention)
	}
	if cfg.Gateway.Agents.HeartbeatInterval == nil || cfg.Gateway.Agents.HeartbeatInterval.Value != 15e9 {
		t.Errorf("heartbeat_interval default = %v, want 15s", cfg.Gateway.Agents.HeartbeatInterval)
	}
	if cfg.Gateway.Paths.CredentialsDir != filepath.Join(dir, "credentials") {
		t.Errorf("credentials_dir = %q, want %q", cfg.Gateway.Paths.CredentialsDir, filepath.Join(dir, "credentials"))
	}
	if cfg.Gateway.Paths.Nodes != filepath.Join(dir, "nodes.yaml") {
		t.Errorf("paths.nodes = %q, want %q", cfg.Gateway.Paths.Nodes, filepath.Join(dir, "nodes.yaml"))
	}
	if cfg.Gateway.Auth.TokensFile != filepath.Join(dir, "state", "tokens.json") {
		t.Errorf("tokens_file default = %q, want %q", cfg.Gateway.Auth.TokensFile, filepath.Join(dir, "state", "tokens.json"))
	}
	if cfg.Gateway.Log.Level != "info" || cfg.Gateway.Log.Format != "text" {
		t.Errorf("log defaults = %q/%q, want info/text", cfg.Gateway.Log.Level, cfg.Gateway.Log.Format)
	}
	if cfg.Gateway.Limits.ResponseSize == nil || cfg.Gateway.Limits.ResponseSize.Value != 1<<20 {
		t.Errorf("response_size default = %v, want 1MiB", cfg.Gateway.Limits.ResponseSize)
	}
	if cfg.Gateway.Limits.LogLine == nil || cfg.Gateway.Limits.LogLine.Value != 16<<10 {
		t.Errorf("log_line default = %v, want 16KiB", cfg.Gateway.Limits.LogLine)
	}
	if cfg.Gateway.Limits.LogTotal == nil || cfg.Gateway.Limits.LogTotal.Value != 10<<20 {
		t.Errorf("log_total default = %v, want 10MiB", cfg.Gateway.Limits.LogTotal)
	}
	if cfg.Gateway.Limits.Summary == nil || cfg.Gateway.Limits.Summary.Value != 1<<10 {
		t.Errorf("summary default = %v, want 1KiB", cfg.Gateway.Limits.Summary)
	}
	if cfg.Gateway.Limits.ErrorMessage == nil || cfg.Gateway.Limits.ErrorMessage.Value != 4<<10 {
		t.Errorf("error_message default = %v, want 4KiB", cfg.Gateway.Limits.ErrorMessage)
	}
}

// TestLoadValidationRules is the scenario table. Every validation rule
// has at least one row, plus a valid row proving no problems on the base
// tree.
func TestLoadValidationRules(t *testing.T) {
	tests := []struct {
		name string
		// files rewrites the file tree; nil keeps the base tree.
		files func() map[string]entry
		dirs  func() map[string]os.FileMode
		after func(t *testing.T, dir string)
		// winSkip marks a scenario that only exercises the POSIX mode
		// checks, which are skipped on Windows.
		winSkip bool
		// check inspects the loaded configuration after a scenario with no
		// problems.
		check func(t *testing.T, cfg *Config)
		want  []string
	}{
		{
			name: "valid base loads with no problems",
			want: nil,
		},
		{
			name: "null capability is empty routing, not a crash",
			files: func() map[string]entry {
				f := baseFiles()
				f["routes.yaml"] = entry{content: "version: 1\ncapabilities:\n  demo:\n"}
				return f
			},
			want: nil,
		},
		{
			name: "null operation is empty routing, not a crash",
			files: func() map[string]entry {
				f := baseFiles()
				f["routes.yaml"] = entry{content: routesBase +
					"\n    operations:\n      run:\n"}
				return f
			},
			want: nil,
		},
		{
			name: "unknown key in gateway.yaml",
			files: func() map[string]entry {
				return withGateway("timeuouts:\n  operation: 5m\n")
			},
			want: []string{"field timeuouts not found in type config.GatewayConfig"},
		},
		{
			name: "unknown key in nodes.yaml",
			files: func() map[string]entry {
				f := baseFiles()
				f["nodes.yaml"] = entry{content: nodesBase + "  node-e:\n    oss: linux\n"}
				return f
			},
			want: []string{
				"field oss not found in type config.NodeConfig",
				"nodes.node-e.os: is required",
				"nodes.node-e.arch: is required",
			},
		},
		{
			name: "unknown key in routes.yaml",
			files: func() map[string]entry {
				f := baseFiles()
				f["routes.yaml"] = entry{content: routesBase + "\nextra_key: {}\n"}
				return f
			},
			want: []string{"field extra_key not found in type config.RoutesConfig"},
		},
		{
			name: "version missing in gateway.yaml",
			files: func() map[string]entry {
				f := baseFiles()
				f["gateway.yaml"] = entry{content: strings.Replace(gwBase, "version: 1\n", "", 1)}
				return f
			},
			want: []string{"gateway.yaml: version: must be 1"},
		},
		{
			name: "version 2 in gateway.yaml is a problem",
			files: func() map[string]entry {
				f := baseFiles()
				f["gateway.yaml"] = entry{content: strings.Replace(gwBase, "version: 1\n", "version: 2\n", 1)}
				return f
			},
			want: []string{"gateway.yaml: version: must be 1"},
		},
		{
			name: "version 2 in nodes.yaml is a problem",
			files: func() map[string]entry {
				f := baseFiles()
				f["nodes.yaml"] = entry{content: strings.Replace(nodesBase, "version: 1\n", "version: 2\n", 1)}
				return f
			},
			want: []string{"nodes.yaml: version: must be 1"},
		},
		{
			name: "version 2 in routes.yaml is a problem",
			files: func() map[string]entry {
				f := baseFiles()
				f["routes.yaml"] = entry{content: strings.Replace(routesBase, "version: 1\n", "version: 2\n", 1)}
				return f
			},
			want: []string{"routes.yaml: version: must be 1"},
		},
		{
			name: "bad duration unit",
			files: func() map[string]entry {
				return withGateway("timeouts:\n  operation: 5x\n")
			},
			want: []string{"gateway.yaml: file: duration \"5x\" needs one of the units"},
		},
		{
			name: "bad size unit",
			files: func() map[string]entry {
				return withGateway("limits:\n  response_size: 1KB\n")
			},
			want: []string{"gateway.yaml: file: size \"1KB\" needs one of the units"},
		},
		{
			name: "invalid node ID in nodes.yaml",
			files: func() map[string]entry {
				f := baseFiles()
				f["nodes.yaml"] = entry{content: nodesBase + "  Node-X:\n    os: linux\n    arch: amd64\n"}
				return f
			},
			want: []string{`nodes.yaml: nodes.Node-X: is not a valid node ID`},
		},
		{
			name: "duplicate node ID is rejected",
			files: func() map[string]entry {
				f := baseFiles()
				f["nodes.yaml"] = entry{content: nodesBase + "  node-a:\n    os: linux\n    arch: amd64\n"}
				return f
			},
			want: []string{"nodes.yaml: file: "},
		},
		{
			name: "missing os in a node",
			files: func() map[string]entry {
				f := baseFiles()
				f["nodes.yaml"] = entry{content: nodesBase + "  node-e:\n    arch: amd64\n"}
				return f
			},
			want: []string{"nodes.yaml: nodes.node-e.os: is required"},
		},
		{
			name: "invalid os in a node",
			files: func() map[string]entry {
				f := baseFiles()
				f["nodes.yaml"] = entry{content: nodesBase + "  node-e:\n    os: plan9\n    arch: amd64\n"}
				return f
			},
			want: []string{"nodes.yaml: nodes.node-e.os: must be one of linux, darwin, or windows"},
		},
		{
			name: "invalid arch in a node",
			files: func() map[string]entry {
				f := baseFiles()
				f["nodes.yaml"] = entry{content: nodesBase + "  node-e:\n    os: linux\n    arch: 386\n"}
				return f
			},
			want: []string{"nodes.yaml: nodes.node-e.arch: must be amd64 or arm64"},
		},
		{
			name: "capability default executor not in executors",
			files: func() map[string]entry {
				f := baseFiles()
				f["routes.yaml"] = entry{content: strings.Replace(routesBase,
					"executors: [node-a, node-b]\n    default_executor: node-a",
					"executors: [node-a, node-b]\n    default_executor: node-c", 1)}
				return f
			},
			want: []string{
				`routes.yaml: capabilities.pc.default_executor: node "node-c" is not in executors`,
			},
		},
		{
			name: "default executor not in star executors is unknown",
			files: func() map[string]entry {
				f := baseFiles()
				f["routes.yaml"] = entry{content: routesBase + "\n  ghost:\n    executors: \"*\"\n    default_executor: nobody\n"}
				return f
			},
			want: []string{
				`routes.yaml: capabilities.ghost.default_executor: node "nobody" does not exist`,
			},
		},
		{
			name: "default executor is a disabled node under star",
			files: func() map[string]entry {
				f := baseFiles()
				f["routes.yaml"] = entry{content: routesBase + "\n  sleep:\n    executors: \"*\"\n    default_executor: node-c\n"}
				return f
			},
			want: []string{
				`routes.yaml: capabilities.sleep.default_executor: node "node-c" is disabled, so it is not in "*"`,
			},
		},
		{
			name: "operation default executor not in operation executors",
			files: func() map[string]entry {
				f := baseFiles()
				f["routes.yaml"] = entry{content: routesBase +
					"\n    operations:\n      down:\n        executors: [node-b]\n        default_executor: node-a\n"}
				return f
			},
			want: []string{
				`routes.yaml: capabilities.pc.operations.down.default_executor: node "node-a" is not in executors`,
			},
		},
		{
			name: "operation default executor not in capability executors via star op",
			files: func() map[string]entry {
				f := baseFiles()
				f["routes.yaml"] = entry{content: routesBase +
					"\n    operations:\n      down:\n        executors: \"*\"\n        default_executor: nobody\n"}
				return f
			},
			want: []string{
				`routes.yaml: capabilities.pc.operations.down.default_executor: node "nobody" is not in executors`,
			},
		},
		{
			name: "operation executors not a subset of capability executors",
			files: func() map[string]entry {
				f := baseFiles()
				f["routes.yaml"] = entry{content: routesBase +
					"\n    operations:\n      down:\n        executors: [node-b, user]\n"}
				return f
			},
			want: []string{
				`routes.yaml: capabilities.pc.operations.down.executors: is not a subset of the capability's executors: node "user" is not in the capability's list`,
				`routes.yaml: capabilities.pc.operations.down.executors: node "user" does not exist`,
			},
		},
		{
			name: "operation executors naming an unknown node",
			files: func() map[string]entry {
				f := baseFiles()
				f["routes.yaml"] = entry{content: routesBase +
					"\n    operations:\n      status:\n        executors: [photon]\n"}
				return f
			},
			want: []string{
				`routes.yaml: capabilities.pc.operations.status.executors: is not a subset of the capability's executors: node "photon" is not in the capability's list`,
				`routes.yaml: capabilities.pc.operations.status.executors: node "photon" does not exist`,
			},
		},
		{
			name: "unknown node in capability executors",
			files: func() map[string]entry {
				f := baseFiles()
				f["routes.yaml"] = entry{content: routesBase +
					"\n  rep:\n    executors: [user]\n"}
				return f
			},
			want: []string{
				`routes.yaml: capabilities.rep.executors: node "user" does not exist`,
			},
		},
		{
			name: "star mixed into an executor list",
			files: func() map[string]entry {
				f := baseFiles()
				f["routes.yaml"] = entry{content: routesBase +
					"\n  mix:\n    executors: [node-a, \"*\"]\n"}
				return f
			},
			want: []string{
				`routes.yaml: capabilities.mix.executors: must list node IDs; "*" alone is the form for all nodes`,
				`routes.yaml: capabilities.mix.executors: node "*" does not exist`,
			},
		},
		{
			name: "invalid node ID inside an executor list",
			files: func() map[string]entry {
				f := baseFiles()
				f["routes.yaml"] = entry{content: routesBase + "\n  bad:\n    executors: [NOPE]\n"}
				return f
			},
			want: []string{
				`routes.yaml: capabilities.bad.executors: contains an invalid node ID "NOPE"`,
				`routes.yaml: capabilities.bad.executors: node "NOPE" does not exist`,
			},
		},
		{
			name: "invalid capability ID in routes.yaml",
			files: func() map[string]entry {
				f := baseFiles()
				f["routes.yaml"] = entry{content: routesBase + "\n  PC-UP:\n    executors: [node-a]\n"}
				return f
			},
			want: []string{
				`routes.yaml: capabilities.PC-UP: is not a valid capability ID`,
			},
		},
		{
			name: "invalid operation ID in routes.yaml",
			files: func() map[string]entry {
				f := baseFiles()
				f["routes.yaml"] = entry{content: routesBase +
					"\n    operations:\n      SHUT DOWN:\n        executors: [node-a]\n"}
				return f
			},
			want: []string{
				`routes.yaml: capabilities.pc.operations.SHUT DOWN: is not a valid operation ID`,
			},
		},
		{
			name: "gateway node is required",
			files: func() map[string]entry {
				f := baseFiles()
				f["gateway.yaml"] = entry{content: strings.Replace(gwBase, "node: gateway\n", "", 1)}
				return f
			},
			want: []string{"gateway.yaml: node: is required"},
		},
		{
			name: "embedded_agent defaults to true",
			files: func() map[string]entry {
				return baseFiles()
			},
			want: nil,
			check: func(t *testing.T, cfg *Config) {
				if !cfg.Gateway.EmbeddedAgentEnabled() {
					t.Error("embedded_agent = false by default, want true")
				}
			},
		},
		{
			name: "embedded_agent false is valid",
			files: func() map[string]entry {
				f := baseFiles()
				f["gateway.yaml"] = entry{content: gwBase + "embedded_agent: false\n"}
				return f
			},
			want: nil,
			check: func(t *testing.T, cfg *Config) {
				if cfg.Gateway.EmbeddedAgentEnabled() {
					t.Error("embedded_agent = true, want false")
				}
			},
		},
		{
			name: "embedded_agent must be a boolean",
			files: func() map[string]entry {
				f := baseFiles()
				f["gateway.yaml"] = entry{content: gwBase + "embedded_agent: 3\n"}
				return f
			},
			want: []string{"gateway.yaml: file: line 12: cannot unmarshal !!int `3` into bool"},
		},
		{
			name: "gateway node names an unknown node",
			files: func() map[string]entry {
				f := baseFiles()
				f["gateway.yaml"] = entry{content: strings.Replace(gwBase, "node: gateway", "node: ghost", 1)}
				return f
			},
			want: []string{`gateway.yaml: node: node "ghost" does not exist in the nodes file`},
		},
		{
			name: "gateway node is disabled",
			files: func() map[string]entry {
				f := baseFiles()
				f["gateway.yaml"] = entry{content: strings.Replace(gwBase, "node: gateway", "node: node-c", 1)}
				return f
			},
			want: []string{`gateway.yaml: node: node "node-c" is disabled, so it cannot be the Gateway host`},
		},
		{
			name: "gateway node is not a valid ID",
			files: func() map[string]entry {
				f := baseFiles()
				f["gateway.yaml"] = entry{content: strings.Replace(gwBase, "node: gateway", "node: Gateway", 1)}
				return f
			},
			want: []string{`gateway.yaml: node: must be a valid node ID`},
		},
		{
			name: "plain http public_url on a non-loopback host",
			files: func() map[string]entry {
				f := baseFiles()
				f["gateway.yaml"] = entry{content: strings.Replace(gwBase, "http://127.0.0.1:8443", "http://gw.example.test:8443", 1)}
				return f
			},
			want: []string{"gateway.yaml: public_url: must use https, unless its host is a loopback address"},
		},
		{
			name: "https on a non-loopback host is fine",
			files: func() map[string]entry {
				f := baseFiles()
				f["gateway.yaml"] = entry{content: strings.Replace(gwBase, "public_url: http://127.0.0.1:8443", "public_url: https://gw.example.test", 1)}
				return f
			},
			want: nil,
		},
		{
			name: "a scheme that is not http or https is a problem, even on loopback",
			files: func() map[string]entry {
				f := baseFiles()
				f["gateway.yaml"] = entry{content: strings.Replace(gwBase, "public_url: http://127.0.0.1:8443", "public_url: ftp://127.0.0.1", 1)}
				return f
			},
			want: []string{"gateway.yaml: public_url: must use https, unless its host is a loopback address"},
		},
		{
			name: "a scheme that is not http or https is a problem on any host",
			files: func() map[string]entry {
				f := baseFiles()
				f["gateway.yaml"] = entry{content: strings.Replace(gwBase, "public_url: http://127.0.0.1:8443", "public_url: ftp://gw.example.test", 1)}
				return f
			},
			want: []string{"gateway.yaml: public_url: must use https, unless its host is a loopback address"},
		},
		{
			name: "public_url is required",
			files: func() map[string]entry {
				f := baseFiles()
				f["gateway.yaml"] = entry{content: strings.Replace(gwBase, "public_url: http://127.0.0.1:8443\n", "", 1)}
				return f
			},
			want: []string{"gateway.yaml: public_url: is required"},
		},
		{
			name: "public_url is not a URL",
			files: func() map[string]entry {
				f := baseFiles()
				f["gateway.yaml"] = entry{content: strings.Replace(gwBase, "public_url: http://127.0.0.1:8443", "public_url: \":\"", 1)}
				return f
			},
			want: []string{"gateway.yaml: public_url: is not a valid URL"},
		},
		{
			name: "listen on a non-loopback address without TLS",
			files: func() map[string]entry {
				f := baseFiles()
				f["gateway.yaml"] = entry{content: strings.Replace(gwBase, "listen: 127.0.0.1:8443", "listen: 0.0.0.0:8443", 1)}
				return f
			},
			want: []string{"gateway.yaml: listen: must be a loopback address, because there is no TLS"},
		},
		{
			name: "non-loopback listen with TLS configured is fine",
			files: func() map[string]entry {
				f := baseFiles()
				f["gateway.yaml"] = entry{content: gwBase + "tls:\n  cert_file: %DIR%/tls/cert.pem\n  key_file: %DIR%/tls/key.pem\n"}
				return f
			},
			want: nil,
		},
		{
			name: "non-loopback listen with allow_insecure_http is fine",
			files: func() map[string]entry {
				f := baseFiles()
				f["gateway.yaml"] = entry{content: strings.Replace(gwBase, "listen: 127.0.0.1:8443", "listen: 0.0.0.0:8443", 1) +
					"allow_insecure_http: true\n"}
				return f
			},
			want: nil,
		},
		{
			name: "TLS with only one side set",
			files: func() map[string]entry {
				f := baseFiles()
				f["gateway.yaml"] = entry{content: gwBase + "tls:\n  cert_file: %DIR%/tls/cert.pem\n"}
				return f
			},
			want: []string{"gateway.yaml: tls: set both tls.cert_file and tls.key_file, or neither"},
		},
		{
			name: "missing TLS key file is a problem",
			files: func() map[string]entry {
				f := baseFiles()
				delete(f, "tls/key.pem")
				f["gateway.yaml"] = entry{content: gwBase + "tls:\n  cert_file: %DIR%/tls/cert.pem\n  key_file: %DIR%/tls/key.pem\n"}
				return f
			},
			want: []string{"gateway.yaml: tls.key_file: cannot read the file"},
		},
		{
			name: "TLS key readable by others is a problem",
			files: func() map[string]entry {
				f := baseFiles()
				f["gateway.yaml"] = entry{content: gwBase + "tls:\n  cert_file: %DIR%/tls/cert.pem\n  key_file: %DIR%/tls/key.pem\n"}
				return f
			},
			after: func(t *testing.T, dir string) {
				setMode(t, filepath.Join(dir, "tls", "key.pem"), 0o644)
			},
			winSkip: true,
			want:    []string{"tls/key.pem is readable by users other than its owner"},
		},
		{
			name: "missing capabilities list",
			files: func() map[string]entry {
				f := baseFiles()
				f["gateway.yaml"] = entry{content: strings.Replace(gwBase, "  capabilities:\n    - %DIR%/capabilities\n", "", 1)}
				return f
			},
			want: []string{"gateway.yaml: paths.capabilities: is required: at least one directory"},
		},
		{
			name: "missing credentials_dir key",
			files: func() map[string]entry {
				f := baseFiles()
				f["gateway.yaml"] = entry{content: strings.Replace(gwBase, "  credentials_dir: %DIR%/credentials\n", "", 1)}
				return f
			},
			want: []string{"gateway.yaml: paths.credentials_dir: is required"},
		},
		{
			name: "missing state_dir key",
			files: func() map[string]entry {
				f := baseFiles()
				f["gateway.yaml"] = entry{content: strings.Replace(gwBase, "  state_dir: %DIR%/state\n", "", 1)}
				return f
			},
			want: []string{"gateway.yaml: paths.state_dir: is required"},
		},
		{
			name: "credentials directory does not exist",
			files: func() map[string]entry {
				return baseFiles()
			},
			after: func(t *testing.T, dir string) {
				if err := os.RemoveAll(filepath.Join(dir, "credentials")); err != nil {
					t.Fatal(err)
				}
			},
			want: []string{"gateway.yaml: paths.credentials_dir: cannot read the directory: "},
		},
		{
			name: "credentials directory readable by others is a problem",
			files: func() map[string]entry {
				return baseFiles()
			},
			after: func(t *testing.T, dir string) {
				setMode(t, filepath.Join(dir, "credentials"), 0o754)
			},
			winSkip: true,
			want:    []string{"credentials is readable by users other than its owner"},
		},
		{
			name: "secret file readable by others is a problem",
			files: func() map[string]entry {
				f := baseFiles()
				return f
			},
			after: func(t *testing.T, dir string) {
				if err := os.MkdirAll(filepath.Join(dir, "credentials", "backup"), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "credentials", "backup", "api_token"), []byte("placeholder value, never logged"), 0o600); err != nil {
					t.Fatal(err)
				}
				setMode(t, filepath.Join(dir, "credentials", "backup", "api_token"), 0o644)
			},
			winSkip: true,
			want:    []string{"backup/api_token is readable by users other than its owner"},
		},
		{
			name: "safe secret file passes",
			files: func() map[string]entry {
				return baseFiles()
			},
			after: func(t *testing.T, dir string) {
				if err := os.MkdirAll(filepath.Join(dir, "credentials", "backup"), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "credentials", "backup", "api_token"), []byte("placeholder value, never logged"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			want: nil,
		},
		{
			name: "heartbeat_timeout below twice heartbeat_interval",
			files: func() map[string]entry {
				return withGateway("agents:\n  heartbeat_interval: 15s\n  heartbeat_timeout: 25s\n")
			},
			want: []string{"gateway.yaml: agents.heartbeat_timeout: must be at least twice agents.heartbeat_interval"},
		},
		{
			name: "bad log level",
			files: func() map[string]entry {
				return withGateway("log:\n  level: loud\n")
			},
			want: []string{"gateway.yaml: log.level: must be one of debug, info, warn, or error"},
		},
		{
			name: "bad log format",
			files: func() map[string]entry {
				return withGateway("log:\n  level: info\n  format: xml\n")
			},
			want: []string{"gateway.yaml: log.format: must be text or json"},
		},
		{
			name: "relative node and route paths inside a subdirectory resolve from the gateway file",
			files: func() map[string]entry {
				f := baseFiles()
				f["sub/nodes.yaml"] = entry{content: nodesBase}
				f["sub/routes.yaml"] = entry{content: routesBase}
				f["gateway.yaml"] = entry{content: strings.ReplaceAll(gwBase,
					"  nodes: nodes.yaml\n  routes: routes.yaml",
					"  nodes: sub/nodes.yaml\n  routes: sub/routes.yaml")}
				delete(f, "nodes.yaml")
				delete(f, "routes.yaml")
				return f
			},
			want: nil,
		},
		{
			name: "missing nodes.yaml file",
			files: func() map[string]entry {
				f := baseFiles()
				delete(f, "nodes.yaml")
				return f
			},
			want: []string{"nodes.yaml: file: cannot read the file: open"},
		},
		{
			name: "missing routes.yaml file and gateway keeping its own checks",
			files: func() map[string]entry {
				f := baseFiles()
				delete(f, "routes.yaml")
				return f
			},
			want: []string{"routes.yaml: file: cannot read the file: open"},
		},
		{
			name: "every problem shows up, not only the first",
			files: func() map[string]entry {
				f := baseFiles()
				delete(f, "routes.yaml")
				f["nodes.yaml"] = entry{content: nodesBase + "  node-e:\n    os: plan9\n    arch: 386\n"}
				return f
			},
			want: []string{
				"nodes.yaml: nodes.node-e.os: must be one of linux, darwin, or windows",
				"nodes.yaml: nodes.node-e.arch: must be amd64 or arm64",
				"routes.yaml: file: cannot read the file: open",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.winSkip && runtime.GOOS == "windows" {
				t.Skip("the POSIX mode checks do not run on Windows")
			}
			files := baseFiles()
			if tc.files != nil {
				files = tc.files()
			}
			dirs := mergeDirs(baseDirs(), baseGatewayFiles())
			if tc.dirs != nil {
				dirs = mergeDirs(baseDirs(), tc.dirs())
			}
			dir := setup(t, files, dirs)
			if tc.after != nil {
				tc.after(t, dir)
			}
			cfg, problems, err := Load(filepath.Join(dir, "gateway.yaml"))
			if err != nil {
				t.Fatalf("Load: unexpected hard error: %v", err)
			}
			assertProblems(t, problems, tc.want)
			if tc.check != nil && len(problems) == 0 {
				tc.check(t, cfg)
			}
		})
	}
}

// TestLoadDefaultPathsMissing checks the missing-outmost-file case: when
// gateway.yaml itself is unreadable, the loader reports exactly that.
func TestLoadMissingGatewayFile(t *testing.T) {
	dir := t.TempDir()
	problems := runLoad(t, dir)
	assertProblems(t, problems, []string{
		filepath.Join(dir, "gateway.yaml") + ": file: cannot read the file",
	})
}

func TestLimitsDefaults(t *testing.T) {
	files := baseFiles()
	files["gateway.yaml"] = entry{content: gwBase}
	dir := setup(t, files, mergeDirs(baseDirs(), baseGatewayFiles()))
	cfg, _, err := Load(filepath.Join(dir, "gateway.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Gateway.Limits.ResponseSize == nil || cfg.Gateway.Limits.ResponseSize.Value != 1<<20 {
		t.Errorf("response_size default = %v, want 1MiB", cfg.Gateway.Limits.ResponseSize)
	}
}

// TestResolveTimeoutsAgainstDefaults ties the loader to the timeout
// resolution the already-existing Timeouts code implements.
func TestLoadTimeoutsOverride(t *testing.T) {
	files := baseFiles()
	files["gateway.yaml"] = entry{content: gwBase + "timeouts:\n  operation: 2m\n  options: 3s\n  grace_period: 4s\n"}
	dir := setup(t, files, mergeDirs(baseDirs(), baseGatewayFiles()))
	cfg, _, err := Load(filepath.Join(dir, "gateway.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Gateway.Timeouts.Operation.Value != 2*60e9 {
		t.Errorf("operation timeout = %v, want 2m", cfg.Gateway.Timeouts.Operation.Value)
	}
	if cfg.Gateway.Timeouts.Options.Value != 3e9 {
		t.Errorf("options timeout = %v, want 3s", cfg.Gateway.Timeouts.Options.Value)
	}
}
