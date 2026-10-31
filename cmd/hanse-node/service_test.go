package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/achmadss/hanse/internal/service"
)

// testInstallPaths are the paths that the tests install into.
type testInstallPaths struct {
	config string
	state  string
}

// newTestPaths returns a configuration path and a state directory inside
// one temporary directory.
func newTestPaths(t *testing.T) testInstallPaths {
	t.Helper()
	dir := t.TempDir()
	return testInstallPaths{
		config: filepath.Join(dir, "agent.yaml"),
		state:  filepath.Join(dir, "state"),
	}
}

// testArgs builds the arguments of install or uninstall. --user is empty,
// as it is when an operator names no account: install then uses the
// documented default on POSIX and the virtual service account on Windows.
func testArgs(paths testInstallPaths) serviceArgs {
	return serviceArgs{configPath: paths.config, stateDir: paths.state}
}

// serviceFake records the calls of an install or uninstall. It is the
// whole operating system of these tests, so none of them needs root.
type serviceFake struct {
	calls         []string
	account       service.Account
	administrator bool
	failOn        string
}

// record appends one call.
func (f *serviceFake) record(format string, args ...any) error {
	call := fmt.Sprintf(format, args...)
	f.calls = append(f.calls, call)
	if f.failOn != "" && strings.Contains(call, f.failOn) {
		return errors.New("the test host refuses this call")
	}
	return nil
}

// Account returns the account that the test configured.
func (f *serviceFake) Account(string) (service.Account, error) { return f.account, nil }

// Administrator returns the rights that the test configured.
func (f *serviceFake) Administrator() (bool, error) { return f.administrator, nil }

// MkdirAll records the directory.
func (f *serviceFake) MkdirAll(path string, _ os.FileMode, _ *service.Owner) error {
	return f.record("mkdir %s", path)
}

// WriteFile records the file and whether an existing one is kept.
func (f *serviceFake) WriteFile(path string, data []byte, _ os.FileMode, _ *service.Owner, createOnly bool) error {
	return f.record("write %s createOnly=%t bytes=%d", path, createOnly, len(data))
}

// Chown records the file whose owner changes.
func (f *serviceFake) Chown(path string, _ os.FileMode, _ *service.Owner) error {
	return f.record("chown %s", path)
}

// RemoveFile records the removed file.
func (f *serviceFake) RemoveFile(path string) error { return f.record("remove %s", path) }

// Run records a service manager command.
func (f *serviceFake) Run(args []string) error { return f.record("run %s", strings.Join(args, " ")) }

// RegisterService records the service definition.
func (f *serviceFake) RegisterService(spec service.Spec) error {
	return f.record("register %s", spec.Name)
}

// StartService records the started service.
func (f *serviceFake) StartService(name string) error { return f.record("start %s", name) }

// StopService records the stopped service.
func (f *serviceFake) StopService(name string) error { return f.record("stop %s", name) }

// DeleteService records the deleted service.
func (f *serviceFake) DeleteService(name string) error { return f.record("delete %s", name) }

// AddGroupMember records the added membership.
func (f *serviceFake) AddGroupMember(group, member string) error {
	return f.record("add-member %s %s", group, member)
}

// RemoveGroupMember records the removed membership.
func (f *serviceFake) RemoveGroupMember(group, member string) error {
	return f.record("remove-member %s %s", group, member)
}

// Grant records the access that the plan grants.
func (f *serviceFake) Grant(path string, access service.Access) error {
	return f.record("grant %s %d", path, access)
}

// callsText joins the recorded calls for a failure message.
func (f *serviceFake) callsText() string { return strings.Join(f.calls, "\n") }

// TestInstallRefusesWithoutRights covers the refusal on a process that
// cannot register a service.
func TestInstallRefusesWithoutRights(t *testing.T) {
	paths := newTestPaths(t)
	host := &serviceFake{account: testNodeAccount(), administrator: false}
	var stdout, stderr bytes.Buffer
	code := installService(host, testArgs(paths), &stdout, &stderr)
	if code != exitFail {
		t.Fatalf("exit code = %d, want %d", code, exitFail)
	}
	want := "must run as root"
	if runtime.GOOS == "windows" {
		want = "needs an elevated prompt"
	}
	if !strings.Contains(stderr.String(), want) {
		t.Errorf("stderr = %q, want it to contain %q", stderr.String(), want)
	}
	if len(host.calls) != 0 {
		t.Fatalf("a refused install changed the system: %v", host.calls)
	}
}

// TestUninstallRefusesWithoutRights covers the same refusal for uninstall.
func TestUninstallRefusesWithoutRights(t *testing.T) {
	paths := newTestPaths(t)
	host := &serviceFake{administrator: false}
	var stdout, stderr bytes.Buffer
	if code := uninstallService(host, testArgs(paths), &stdout, &stderr); code != exitFail {
		t.Fatalf("exit code = %d, want %d", code, exitFail)
	}
	if len(host.calls) != 0 {
		t.Fatalf("a refused uninstall changed the system: %v", host.calls)
	}
}

// TestInstallAppliesThePlan covers a complete install of an enrolled node:
// the plan runs, the default agent.yaml keeps any existing file, and the
// service is started because the node has a credential.
func TestInstallAppliesThePlan(t *testing.T) {
	paths := newTestPaths(t)
	writeTestCredential(t, paths.state, testCredential, "https://gateway.example.com")
	host := &serviceFake{account: testNodeAccount(), administrator: true}
	var stdout, stderr bytes.Buffer
	if code := installService(host, testArgs(paths), &stdout, &stderr); code != exitOK {
		t.Fatalf("exit code = %d, want %d: %s", code, exitOK, stderr.String())
	}
	if !strings.Contains(host.callsText(), "write "+paths.config+" createOnly=true") {
		t.Errorf("calls:\n%s\nwant the default agent.yaml, written only when absent", host.callsText())
	}
	credential := filepath.Join(paths.state, credentialFileName)
	if !strings.Contains(host.callsText(), "chown "+credential) &&
		!strings.Contains(host.callsText(), "grant "+credential) {
		t.Errorf("calls:\n%s\nwant the credential handed to the service account", host.callsText())
	}
	if !strings.Contains(stdout.String(), "it is running now") {
		t.Errorf("stdout = %q, want the running service reported", stdout.String())
	}
}

// TestInstallBeforeEnrollment covers an install on a node without a
// credential: nothing starts, and the operator is told what to do next.
func TestInstallBeforeEnrollment(t *testing.T) {
	paths := newTestPaths(t)
	host := &serviceFake{account: testNodeAccount(), administrator: true}
	var stdout, stderr bytes.Buffer
	if code := installService(host, testArgs(paths), &stdout, &stderr); code != exitOK {
		t.Fatalf("exit code = %d, want %d: %s", code, exitOK, stderr.String())
	}
	if !strings.Contains(stdout.String(), "has not enrolled yet") {
		t.Errorf("stdout = %q, want the enrollment step", stdout.String())
	}
	if strings.Contains(stdout.String(), "it is running now") {
		t.Fatal("the service was started before enrollment")
	}
}

// TestInstallNamesTheServiceAndAccount covers the last lines of a
// successful install: an operator who runs it on a real node must see what
// was registered and for whom.
func TestInstallNamesTheServiceAndAccount(t *testing.T) {
	paths := newTestPaths(t)
	writeTestCredential(t, paths.state, testCredential, "https://gateway.example.com")
	host := &serviceFake{account: testNodeAccount(), administrator: true}
	var stdout, stderr bytes.Buffer
	if code := installService(host, testArgs(paths), &stdout, &stderr); code != exitOK {
		t.Fatalf("exit code = %d, want %d: %s", code, exitOK, stderr.String())
	}
	summary := "installed " + planServicePath() + " for " + planAccount()
	if !strings.Contains(stdout.String(), summary) {
		t.Errorf("stdout = %q, want %q", stdout.String(), summary)
	}
}

// TestUninstallRunsTwice covers a second uninstall on a node where nothing
// is installed any more: every service step finds nothing to do, and the
// command still succeeds.
func TestUninstallRunsTwice(t *testing.T) {
	paths := newTestPaths(t)
	for run := 1; run <= 2; run++ {
		// The second run meets a node whose service, group membership, and
		// unit file are already gone, so every service step fails.
		host := &serviceFake{administrator: true, failOn: "hanse"}
		var stdout, stderr bytes.Buffer
		if code := uninstallService(host, testArgs(paths), &stdout, &stderr); code != exitOK {
			t.Fatalf("uninstall %d: exit code = %d, want %d: %s", run, code, exitOK, stderr.String())
		}
		if !strings.Contains(stdout.String(), "removed "+planServicePath()) {
			t.Errorf("uninstall %d: stdout = %q, want the removal reported", run, stdout.String())
		}
	}
}

// TestUninstallAppliesThePlan covers the removal and the files it keeps.
func TestUninstallAppliesThePlan(t *testing.T) {
	paths := newTestPaths(t)
	host := &serviceFake{administrator: true}
	var stdout, stderr bytes.Buffer
	if code := uninstallService(host, testArgs(paths), &stdout, &stderr); code != exitOK {
		t.Fatalf("exit code = %d, want %d: %s", code, exitOK, stderr.String())
	}
	for _, call := range host.calls {
		if strings.Contains(call, paths.config) || strings.Contains(call, paths.state) {
			t.Errorf("uninstall touched %q", call)
		}
	}
	if !strings.Contains(stdout.String(), "removed "+planServicePath()) {
		t.Errorf("stdout = %q, want the removal reported", stdout.String())
	}
}

// TestInstallFailsOnAStep covers a failing step: the command reports it
// and returns a failure.
func TestInstallFailsOnAStep(t *testing.T) {
	paths := newTestPaths(t)
	host := &serviceFake{account: testNodeAccount(), administrator: true, failOn: "write " + paths.config}
	var stdout, stderr bytes.Buffer
	if code := installService(host, testArgs(paths), &stdout, &stderr); code != exitFail {
		t.Fatalf("exit code = %d, want %d", code, exitFail)
	}
	if !strings.Contains(stderr.String(), "wrote the default "+paths.config) {
		t.Errorf("stderr = %q, want the failing step named", stderr.String())
	}
}

// TestServiceArgs covers the flags that install and uninstall accept.
func TestServiceArgs(t *testing.T) {
	var stderr bytes.Buffer
	parsed, ok := parseServiceArgs([]string{"--user", "node-runner", "--config=/tmp/agent.yaml",
		"--state-dir", "/tmp/state"}, "/etc/hanse-node/agent.yaml", "/var/lib/hanse-node", &stderr)
	if !ok {
		t.Fatalf("parse failed: %s", stderr.String())
	}
	if parsed.user != "node-runner" || parsed.configPath != "/tmp/agent.yaml" || parsed.stateDir != "/tmp/state" {
		t.Errorf("parsed = %+v, want every flag", parsed)
	}
	defaults, ok := parseServiceArgs(nil, "/etc/hanse-node/agent.yaml", "/var/lib/hanse-node", &stderr)
	if !ok {
		t.Fatalf("parse failed: %s", stderr.String())
	}
	if defaults.configPath != "/etc/hanse-node/agent.yaml" || defaults.stateDir != "/var/lib/hanse-node" {
		t.Errorf("defaults = %+v, want the documented paths", defaults)
	}
	for _, args := range [][]string{
		{"--user"},
		{"--nonsense"},
		{"extra"},
	} {
		stderr.Reset()
		if _, ok := parseServiceArgs(args, "/etc/hanse-node/agent.yaml", "/var/lib/hanse-node", &stderr); ok {
			t.Errorf("arguments %v were accepted", args)
		}
		if stderr.Len() == 0 {
			t.Errorf("arguments %v were refused without a message", args)
		}
	}
}

// TestEnrolledGatewayURL covers the Gateway URL that the default
// agent.yaml copies from an enrollment.
func TestEnrolledGatewayURL(t *testing.T) {
	paths := newTestPaths(t)
	if got := enrolledGatewayURL(paths.state); got != "" {
		t.Errorf("gateway URL = %q, want empty before enrollment", got)
	}
	writeTestCredential(t, paths.state, testCredential, "https://gateway.example.com")
	if got := enrolledGatewayURL(paths.state); got != "https://gateway.example.com" {
		t.Errorf("gateway URL = %q, want the enrolled URL", got)
	}
	if !credentialExists(paths.state) {
		t.Error("credentialExists reported false for an enrolled node")
	}
	if err := os.WriteFile(filepath.Join(paths.state, credentialFileName), []byte("not json"), 0o600); err != nil {
		t.Fatalf("write the credential: %v", err)
	}
	if got := enrolledGatewayURL(paths.state); got != "" {
		t.Errorf("gateway URL = %q, want empty for an unreadable credential", got)
	}
}

// writeTestCredential writes the credential file that `hanse-node join`
// leaves behind.
func writeTestCredential(t *testing.T, stateDir, credential, gatewayURL string) {
	t.Helper()
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatalf("create the state directory: %v", err)
	}
	data, err := json.Marshal(credentialFile{
		Version:    credentialFileVersion,
		GatewayURL: gatewayURL,
		Node:       "node-a",
		Credential: credential,
	})
	if err != nil {
		t.Fatalf("encode the credential: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, credentialFileName), data, 0o600); err != nil {
		t.Fatalf("write the credential: %v", err)
	}
}

// planServicePath is the service definition that install writes on the
// operating system the test runs on.
func planServicePath() string {
	switch runtime.GOOS {
	case "windows":
		return service.WindowsName
	case "darwin":
		return service.DarwinPlistPath
	default:
		return service.LinuxUnitPath
	}
}

// planAccount is the account that the service runs as on the operating
// system the test runs on.
func planAccount() string {
	if runtime.GOOS == "windows" {
		return service.WindowsAccount
	}
	return testNodeAccount().Name
}

// testNodeAccount is the account that runs the agent in these tests.
func testNodeAccount() service.Account {
	return service.Account{Name: "hanse-node", UID: 1500, GID: 1500, Group: "hanse-node"}
}
