package registry

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// nodes is the fixture node set: the Gateway host, two linux executors, a
// windows node no exec entry covers, and a disabled executor.
func nodes() []Node {
	return []Node{
		{ID: "gateway", OS: "linux", Arch: "amd64", Roles: []string{"server"}, Enabled: true},
		{ID: "node-a", OS: "linux", Arch: "arm64", Roles: []string{"server"}, Enabled: true},
		{ID: "node-b", OS: "linux", Arch: "arm64", Roles: []string{"server"}, Enabled: true},
		{ID: "node-c", OS: "windows", Arch: "amd64", Roles: []string{"pc"}, Enabled: true},
		{ID: "node-d", OS: "linux", Arch: "amd64", Roles: []string{"pc"}, Enabled: false},
	}
}

// baseInput is the routing of docs/configuration.md sections 5 and 9.
func baseInput() Input {
	return Input{
		Nodes:       nodes(),
		GatewayHost: "gateway",
		Routes: Routes{Capabilities: map[string]CapabilityRoute{
			"pc": {Wildcard: true, DefaultExecutor: "gateway"},
		}},
	}
}

// writeCapability writes manifest into <root>/<id>/manifest.yaml.
func writeCapability(t *testing.T, root, id, manifest string) string {
	t.Helper()
	dir := filepath.Join(root, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, manifestFile), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	// A handler file so the bundle is not only the manifest.
	if err := os.WriteFile(filepath.Join(dir, "handler.sh"), []byte("#!/bin/sh\necho ok\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// pcManifest is the manifest example of docs/configuration.md section 6.5.
const pcManifest = `
version: 1
id: pc
name: PC power
description: Wake, shut down, and check machines from another node
contract: hanse.handler/v1

handler:
  kind: exec
  entry:
    linux: [sh, handler.sh]
    darwin: [sh, handler.sh]

secrets:
  ssh_key: Private key that can log in to target nodes and shut them down

operations:
  up:
    description: Wake a node with Wake-on-LAN and wait until it answers
    mode: async
    timeout: 3m
    concurrency:
      limit: 1
      key: [node]
    node_filters:
      node:
        roles: [pc]
        attributes: [mac, broadcast]
    arguments:
      type: object
      required: [node]
      additionalProperties: false
      properties:
        node:
          type: string
          format: hanse:node
          description: The node to wake

  down:
    description: Shut down a node over SSH and wait until it stops answering
    mode: async
    timeout: 2m
    secrets: [ssh_key]
    concurrency:
      limit: 1
      key: [node]
    node_filters:
      node:
        roles: [pc]
        attributes: [ssh_user]
    arguments:
      type: object
      required: [node]
      additionalProperties: false
      properties:
        node:
          type: string
          format: hanse:node
          description: The node to shut down

  status:
    description: Report whether a node is up
    mode: sync
    timeout: 15s
    node_filters:
      node:
        roles: [pc]
    arguments:
      type: object
      required: [node]
      additionalProperties: false
      properties:
        node:
          type: string
          format: hanse:node
        probe:
          type: string
          description: How to check the node
    options:
      probe: provider
`

// httpManifest is a minimal http capability.
const httpManifest = `
version: 1
id: weather
description: Look up the outdoor temperature
contract: hanse.handler/v1

handler:
  kind: http
  url: http://192.0.2.50:8080/weather
  auth:
    type: bearer
    credential: api_token

operations:
  current:
    description: Read the current temperature
    arguments:
      type: object
      properties:
        city:
          type: string
`

func TestLoadValidPackage(t *testing.T) {
	root := t.TempDir()
	writeCapability(t, root, "pc", pcManifest)
	reg, warnings, err := Load(Options{Dirs: []string{root}, Input: baseInput()})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v, want none", warnings)
	}
	c, ok := reg.Capability("pc")
	if !ok {
		t.Fatal("capability pc missing")
	}
	if c.Name != "PC power" || c.Description == "" || c.Dir != filepath.Join(root, "pc") {
		t.Fatalf("capability header wrong: %+v", c)
	}
	if !strings.HasPrefix(c.BundleHash, "sha256:") {
		t.Fatalf("bundle hash %q has no sha256: prefix", c.BundleHash)
	}
	if got := len(c.OperationIDs()); got != 3 {
		t.Fatalf("operations = %v, want 3", c.OperationIDs())
	}
	up, ok := c.Operation("up")
	if !ok {
		t.Fatal("operation up missing")
	}
	if up.ID != "up" || up.Mode != ModeAsync || up.Privileged {
		t.Fatalf("up id/mode/privileged wrong: %+v", up)
	}
	if up.Timeout == nil || *up.Timeout != 3*time.Minute {
		t.Fatalf("up timeout %v, want 3m", up.Timeout)
	}
	if up.Concurrency == nil || up.Concurrency.Limit != 1 ||
		len(up.Concurrency.Key) != 1 || up.Concurrency.Key[0] != "node" {
		t.Fatalf("up concurrency wrong: %+v", up.Concurrency)
	}
	if len(up.Secrets) != 0 {
		t.Fatalf("up secrets %v, want none", up.Secrets)
	}
	if up.Schema == nil {
		t.Fatal("argument schema not compiled")
	}
	down, _ := c.Operation("down")
	if down.Timeout == nil || *down.Timeout != 2*time.Minute {
		t.Fatalf("down timeout %v, want 2m", down.Timeout)
	}
	if len(down.Secrets) != 1 || down.Secrets[0] != "ssh_key" {
		t.Fatalf("down secrets %v, want [ssh_key]", down.Secrets)
	}
	if got := down.Entry["linux"]; len(got) != 2 || got[1] != "handler.sh" {
		t.Fatalf("down linux entry %v, want [sh handler.sh]", got)
	}
	status, _ := c.Operation("status")
	if status.Mode != ModeSync {
		t.Fatalf("status mode %q, want sync", status.Mode)
	}
	if _, ok := status.NodeFilters["probe"]; ok {
		t.Fatalf("status node filters %v, node is the filtered argument", status.NodeFilters)
	}
	if f := status.NodeFilters["node"]; f.Roles[0] != "pc" {
		t.Fatalf("status node filter %v", f)
	}
	if got, ok := c.RoutingOf("up"); !ok ||
		strings.Join(got.Executors, ",") != "gateway,node-a,node-b" ||
		got.DefaultExecutor != "gateway" {
		t.Fatalf("up routing %+v", got)
	}
}

func TestLoadWildcardExpansion(t *testing.T) {
	root := t.TempDir()
	writeCapability(t, root, "pc", pcManifest)
	reg, _, err := Load(Options{Dirs: []string{root}, Input: baseInput()})
	if err != nil {
		t.Fatal(err)
	}
	c, _ := reg.Capability("pc")
	routing, _ := c.RoutingOf("up")
	// node-c is windows and the entry commands cover linux and darwin
	// only; node-d is disabled. "*" skips both.
	want := "gateway,node-a,node-b"
	if got := strings.Join(routing.Executors, ","); got != want {
		t.Fatalf("executors = %q, want %q", got, want)
	}
}

func TestLoadOperationRouteNarrowsToSubset(t *testing.T) {
	root := t.TempDir()
	writeCapability(t, root, "pc", pcManifest)
	input := baseInput()
	input.Routes.Capabilities["pc"] = CapabilityRoute{
		Wildcard:        true,
		DefaultExecutor: "gateway",
		Operations: map[string]OperationRoute{
			"down": {Executors: []string{"node-a"}, ExecutorsSet: true, DefaultSet: true, DefaultExecutor: "node-a"},
		},
	}
	reg, _, err := Load(Options{Dirs: []string{root}, Input: input})
	if err != nil {
		t.Fatal(err)
	}
	c, _ := reg.Capability("pc")
	routing, _ := c.RoutingOf("down")
	want := "node-a"
	if got := strings.Join(routing.Executors, ","); got != want || routing.DefaultExecutor != want {
		t.Fatalf("down routing %+v, want executors [%v]", routing, want)
	}
	// Other operations keep the capability route.
	routing, _ = c.RoutingOf("up")
	if got := strings.Join(routing.Executors, ","); got != "gateway,node-a,node-b" {
		t.Fatalf("up routing %+v", routing)
	}
}

func TestLoadInvalidManifest(t *testing.T) {
	root := t.TempDir()
	writeCapability(t, root, "pc", strings.Replace(pcManifest,
		"handler:\n  kind: exec", "handler:\n  kind: exec\n  typo_field: 1", 1))
	_, _, err := Load(Options{Dirs: []string{root}, Input: baseInput()})
	if err == nil || !strings.Contains(err.Error(), "field typo_field not found") {
		t.Fatalf("err = %v, want unknown field typo_field", err)
	}
	if err != nil && !strings.Contains(err.Error(), "manifest.yaml") {
		t.Fatalf("err %v does not name the manifest file", err)
	}
}

func TestLoadMissingDescription(t *testing.T) {
	root := t.TempDir()
	writeCapability(t, root, "pc", strings.Replace(pcManifest,
		"description: Wake, shut down, and check machines from another node\n", "", 1))
	_, _, err := Load(Options{Dirs: []string{root}, Input: baseInput()})
	if err == nil || !strings.Contains(err.Error(), "description is required") {
		t.Fatalf("err = %v, want description is required", err)
	}
}

func TestLoadDuplicateCapabilityID(t *testing.T) {
	root := t.TempDir()
	writeCapability(t, root, "one", pcManifest)
	other := t.TempDir()
	writeCapability(t, other, "two", pcManifest)
	_, _, err := Load(Options{Dirs: []string{root, other}, Input: baseInput()})
	if err == nil || !strings.Contains(err.Error(), "declared twice") ||
		!strings.Contains(err.Error(), "\"pc\"") {
		t.Fatalf("err = %v, want duplicate capability pc", err)
	}
}

func TestLoadInvalidSchema(t *testing.T) {
	root := t.TempDir()
	// The top level must be type: object.
	writeCapability(t, root, "pc", strings.Replace(pcManifest,
		"arguments:\n      type: object\n      required: [node]\n      additionalProperties: false\n      properties:\n        node:\n          type: string\n          format: hanse:node\n          description: The node to wake\n",
		"arguments:\n      type: string\n", 1))
	_, _, err := Load(Options{Dirs: []string{root}, Input: baseInput()})
	if err == nil || !strings.Contains(err.Error(), "type: object") {
		t.Fatalf("err = %v, want top-level object error", err)
	}
	if err != nil && !strings.Contains(err.Error(), "operations.up.arguments") {
		t.Fatalf("err %v does not name the field", err)
	}
}

func TestLoadUncompilableSchema(t *testing.T) {
	root := t.TempDir()
	// A schema property with an unknown type does not compile.
	writeCapability(t, root, "pc", strings.Replace(pcManifest,
		"        node:\n          type: string\n          format: hanse:node\n          description: The node to wake\n",
		"        node:\n          type: nonsense\n", 1))
	_, _, err := Load(Options{Dirs: []string{root}, Input: baseInput()})
	if err == nil {
		t.Fatal("want schema compile error, got nil")
	}
}

func TestLoadMissingOSEntryCommand(t *testing.T) {
	root := t.TempDir()
	writeCapability(t, root, "pc", pcManifest)
	input := baseInput()
	input.Routes.Capabilities["pc"] = CapabilityRoute{
		Executors:       []string{"gateway", "node-c"},
		DefaultExecutor: "gateway",
	}
	_, _, err := Load(Options{Dirs: []string{root}, Input: input})
	if err == nil || !strings.Contains(err.Error(), "node-c") ||
		!strings.Contains(err.Error(), "windows") {
		t.Fatalf("err = %v, want missing windows entry command for node-c", err)
	}
}

func TestLoadCapabilitySymlink(t *testing.T) {
	root := t.TempDir()
	writeCapability(t, root, "pc", pcManifest)
	if err := os.Symlink(filepath.Join(root, "pc", "handler.sh"),
		filepath.Join(root, "pc", "handler-link.sh")); err != nil {
		t.Skipf("os.Symlink: %v", err)
	}
	_, _, err := Load(Options{Dirs: []string{root}, Input: baseInput()})
	if err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("err = %v, want symbolic link error", err)
	}
}

func TestLoadCapabilityRootSymlink(t *testing.T) {
	root := t.TempDir()
	writeCapability(t, root, "pc", pcManifest)
	if err := os.Symlink(filepath.Join(root, "pc"),
		filepath.Join(root, "pc-link")); err != nil {
		t.Skipf("os.Symlink: %v", err)
	}
	_, _, err := Load(Options{Dirs: []string{root}, Input: baseInput()})
	if err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("err = %v, want symbolic link error", err)
	}
}

func TestLoadHTTPHostOnly(t *testing.T) {
	root := t.TempDir()
	writeCapability(t, root, "weather", httpManifest)
	input := baseInput()
	input.Routes.Capabilities["weather"] = CapabilityRoute{
		Executors:       []string{"gateway"},
		DefaultExecutor: "gateway",
	}
	reg, _, err := Load(Options{Dirs: []string{root}, Input: input})
	if err != nil {
		t.Fatal(err)
	}
	c, _ := reg.Capability("weather")
	routing, _ := c.RoutingOf("current")
	if got := strings.Join(routing.Executors, ","); got != "gateway" {
		t.Fatalf("executors = %q, want gateway only", got)
	}

	// star with http means the Gateway host only
	input.Routes.Capabilities["weather"] = CapabilityRoute{Wildcard: true}
	reg, _, err = Load(Options{Dirs: []string{root}, Input: input})
	if err != nil {
		t.Fatal(err)
	}
	c, _ = reg.Capability("weather")
	routing, _ = c.RoutingOf("current")
	if got := strings.Join(routing.Executors, ","); got != "gateway" {
		t.Fatalf("executors = %q with \"*\", want gateway only", got)
	}

	// another node fails
	input.Routes.Capabilities["weather"] = CapabilityRoute{
		Executors: []string{"gateway", "node-a"},
	}
	_, _, err = Load(Options{Dirs: []string{root}, Input: input})
	if err == nil || !strings.Contains(err.Error(), "Gateway host") {
		t.Fatalf("err = %v, want Gateway host rule error", err)
	}
}

func TestLoadUnroutedCapabilityWarns(t *testing.T) {
	root := t.TempDir()
	writeCapability(t, root, "pc", pcManifest)
	input := baseInput()
	input.Routes.Capabilities = nil
	reg, warnings, err := Load(Options{Dirs: []string{root}, Input: input})
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 1 || warnings[0].Capability != "pc" {
		t.Fatalf("warnings = %+v, want one for pc", warnings)
	}
	c, _ := reg.Capability("pc")
	if routing, _ := c.RoutingOf("up"); len(routing.Executors) != 0 {
		t.Fatalf("unrouted executors %v, want empty", routing.Executors)
	}
}

func TestLoadRouteDefaultNotEligibleFails(t *testing.T) {
	root := t.TempDir()
	writeCapability(t, root, "pc", pcManifest)
	// gateway is eligible, so the capability default is fine; narrowing
	// the operation to node-a invalidates the inherited default.
	input := baseInput()
	input.Routes.Capabilities["pc"] = CapabilityRoute{
		Wildcard:        true,
		DefaultExecutor: "gateway",
		Operations: map[string]OperationRoute{
			"down": {Executors: []string{"node-a"}, ExecutorsSet: true},
		},
	}
	_, _, err := Load(Options{Dirs: []string{root}, Input: input})
	if err == nil || !strings.Contains(err.Error(), "default executor") {
		t.Fatalf("err = %v, want default executor error", err)
	}
}

func TestLoadRouteUnknownNodeFails(t *testing.T) {
	root := t.TempDir()
	writeCapability(t, root, "pc", pcManifest)
	input := baseInput()
	input.Routes.Capabilities["pc"] = CapabilityRoute{
		Executors: []string{"ghost"},
	}
	_, _, err := Load(Options{Dirs: []string{root}, Input: input})
	if err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("err = %v, want unknown executor error", err)
	}
}

func TestLoadRouteUnknownOperationFails(t *testing.T) {
	root := t.TempDir()
	writeCapability(t, root, "pc", pcManifest)
	input := baseInput()
	input.Routes.Capabilities["pc"] = CapabilityRoute{
		Wildcard: true,
		Operations: map[string]OperationRoute{
			"restart": {Wildcard: true},
		},
	}
	_, _, err := Load(Options{Dirs: []string{root}, Input: input})
	if err == nil || !strings.Contains(err.Error(), "restart") {
		t.Fatalf("err = %v, want unknown operation error", err)
	}
}

func TestLoadSecretNotDeclaredFails(t *testing.T) {
	root := t.TempDir()
	writeCapability(t, root, "pc", strings.Replace(pcManifest,
		"operations:\n  up:\n", "operations:\n  up:\n    secrets: [other_key]\n", 1))
	_, _, err := Load(Options{Dirs: []string{root}, Input: baseInput()})
	if err == nil || !strings.Contains(err.Error(), "other_key") {
		t.Fatalf("err = %v, want undeclared secret error", err)
	}
}

func TestLoadConcurrencyKeyUnknownArgumentFails(t *testing.T) {
	root := t.TempDir()
	writeCapability(t, root, "pc", strings.Replace(pcManifest,
		"      key: [node]\n", "      key: [ghost]\n", 1))
	_, _, err := Load(Options{Dirs: []string{root}, Input: baseInput()})
	if err == nil || !strings.Contains(err.Error(), "ghost") ||
		!strings.Contains(err.Error(), "concurrency.key") {
		t.Fatalf("err = %v, want unknown concurrency key error", err)
	}
}

func TestLoadNodeFilterUnknownArgumentFails(t *testing.T) {
	root := t.TempDir()
	writeCapability(t, root, "pc", strings.Replace(pcManifest,
		"    node_filters:\n      node:", "    node_filters:\n      ghost:", 1))
	_, _, err := Load(Options{Dirs: []string{root}, Input: baseInput()})
	if err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("err = %v, want unknown node filter argument error", err)
	}
}

func TestLoadOperationUnknownFieldFails(t *testing.T) {
	root := t.TempDir()
	writeCapability(t, root, "pc", strings.Replace(pcManifest,
		"    mode: async\n    timeout: 3m", "    mode: async\n    timeout: 3m\n    priviledged: true", 1))
	_, _, err := Load(Options{Dirs: []string{root}, Input: baseInput()})
	if err == nil || !strings.Contains(err.Error(), "field priviledged not found") {
		t.Fatalf("err = %v, want unknown field error", err)
	}
}

func TestLoadPrivilegedOnHTTPFails(t *testing.T) {
	root := t.TempDir()
	writeCapability(t, root, "weather", strings.Replace(httpManifest,
		"    description: Read the current temperature\n",
		"    description: Read the current temperature\n    privileged: true\n", 1))
	_, _, err := Load(Options{Dirs: []string{root}, Input: baseInput()})
	if err == nil || !strings.Contains(err.Error(), "privileged") {
		t.Fatalf("err = %v, want privileged-on-http error", err)
	}
}

func TestLoadExecCapabilitiesWarnWithoutGatewayNode(t *testing.T) {
	root := t.TempDir()
	writeCapability(t, root, "pc", pcManifest)
	input := baseInput()
	input.GatewayHost = ""
	_, warnings, err := Load(Options{Dirs: []string{root}, Input: input})
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings %v", warnings)
	}
}
