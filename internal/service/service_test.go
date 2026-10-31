package service

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/achmadss/hanse/internal/config"
)

// Paths and values of the placeholder node that the tests describe.
const (
	testBinary  = "/usr/local/bin/hanse-node"
	testConfig  = "/etc/hanse-node/agent.yaml"
	testState   = "/var/lib/hanse-node"
	testGateway = "https://gateway.example.com"
)

// testAccount is the account that runs the agent in the tests.
func testAccount() Account {
	return Account{Name: "hanse-node", UID: 1500, GID: 1500, Group: "hanse-node"}
}

// testOptions is a complete Linux install.
func testOptions(action Action) Options {
	return Options{
		OS:         "linux",
		Action:     action,
		User:       "hanse-node",
		Binary:     testBinary,
		ConfigPath: testConfig,
		StateDir:   testState,
		GatewayURL: testGateway,
		Start:      true,
	}
}

// windowsOptions is a complete Windows install.
func windowsOptions(action Action) Options {
	return Options{
		OS:         "windows",
		Action:     action,
		Binary:     `C:\Program Files\Hanse\hanse-node.exe`,
		ConfigPath: `C:\ProgramData\Hanse\node\agent.yaml`,
		StateDir:   `C:\ProgramData\Hanse\node\state`,
		GatewayURL: testGateway,
		Start:      true,
	}
}

// kinds returns the step kinds in order, so a test reads the shape of a
// plan in one line.
func kinds(plan Plan) []StepKind {
	found := make([]StepKind, 0, len(plan.Steps))
	for _, step := range plan.Steps {
		found = append(found, step.Kind)
	}
	return found
}

// step finds the only step of one kind and fails when there is none or
// more than one.
func step(t *testing.T, plan Plan, kind StepKind) Step {
	t.Helper()
	var found []Step
	for _, candidate := range plan.Steps {
		if candidate.Kind == kind {
			found = append(found, candidate)
		}
	}
	if len(found) != 1 {
		t.Fatalf("steps of kind %q = %d, want exactly 1 in %v", kind, len(found), kinds(plan))
	}
	return found[0]
}

// stepAt finds the step that acts on one target.
func stepAt(t *testing.T, plan Plan, target string) Step {
	t.Helper()
	for _, candidate := range plan.Steps {
		if candidate.Target == target {
			return candidate
		}
	}
	t.Fatalf("no step acts on %q in %v", target, kinds(plan))
	return Step{}
}

// runSteps returns the commands that a plan runs, in order.
func runSteps(plan Plan) []string {
	var commands []string
	for _, candidate := range plan.Steps {
		if candidate.Kind == StepRun {
			commands = append(commands, strings.Join(candidate.Args, " "))
		}
	}
	return commands
}

// TestBuildRefusesUnsupportedOS covers a platform without a service
// manager definition.
func TestBuildRefusesUnsupportedOS(t *testing.T) {
	options := testOptions(Install)
	options.OS = "plan9"
	if _, err := Build(options, testAccount()); err == nil {
		t.Fatal("an unknown operating system was accepted")
	}
}

// TestBuildRefusesRelativePaths covers a plan that would write outside the
// absolute paths the maintenance commands expect.
func TestBuildRefusesRelativePaths(t *testing.T) {
	for _, field := range []string{"binary", "config", "state"} {
		options := testOptions(Install)
		switch field {
		case "binary":
			options.Binary = "hanse-node"
		case "config":
			options.ConfigPath = "agent.yaml"
		default:
			options.StateDir = "state"
		}
		if _, err := Build(options, testAccount()); err == nil {
			t.Errorf("a relative %s path was accepted", field)
		}
	}
}

// TestBuildRefusesWindowsUser covers the account flag on Windows, where
// the service account is fixed by the service manager.
func TestBuildRefusesWindowsUser(t *testing.T) {
	options := windowsOptions(Install)
	options.User = "someone"
	if _, err := Build(options, Account{}); err == nil {
		t.Fatal("a Windows install accepted another account")
	}
	options.User = WindowsAccount
	if _, err := Build(options, Account{}); err != nil {
		t.Fatalf("Build: %v", err)
	}
}

// TestBuildNeedsThePosixAccount covers a POSIX install that names no
// account, which would leave systemd without a user to run.
func TestBuildNeedsThePosixAccount(t *testing.T) {
	if _, err := Build(testOptions(Install), Account{Name: "hanse-node"}); err == nil {
		t.Fatal("an install without the account's primary group was accepted")
	}
}

// TestUninstallNeedsNoPaths covers uninstall, which acts on the service
// only and keeps every file.
func TestUninstallNeedsNoPaths(t *testing.T) {
	for _, goos := range []string{"linux", "darwin", "windows"} {
		plan, err := Build(Options{OS: goos, Action: Uninstall}, Account{})
		if err != nil {
			t.Fatalf("%s uninstall: %v", goos, err)
		}
		if len(plan.Steps) == 0 {
			t.Fatalf("%s uninstall has no steps", goos)
		}
		for _, candidate := range plan.Steps {
			if candidate.Kind == StepWriteFile {
				t.Fatalf("%s uninstall writes %s", goos, candidate.Target)
			}
		}
	}
}

// TestAgentYAMLLoads checks that the default file is a valid agent.yaml,
// so an install never leaves a node that the agent refuses to start.
func TestAgentYAMLLoads(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.yaml")
	options := testOptions(Install)
	options.ConfigPath = path
	options.StateDir = filepath.Join(dir, "state")
	if err := os.WriteFile(path, []byte(agentYAML(options)), 0o600); err != nil {
		t.Fatalf("write agent.yaml: %v", err)
	}
	cfg, problems, err := config.LoadAgent(path)
	if err != nil {
		t.Fatalf("LoadAgent: %v", err)
	}
	if len(problems) > 0 {
		t.Fatalf("problems = %v, want none", problems)
	}
	if cfg.GatewayURL != testGateway {
		t.Errorf("gateway_url = %q, want %q", cfg.GatewayURL, testGateway)
	}
	if cfg.StateDir != options.StateDir {
		t.Errorf("state_dir = %q, want %q", cfg.StateDir, options.StateDir)
	}
	if cfg.Elevation.Enabled {
		t.Error("elevation is enabled by default")
	}
	if cfg.MaxConcurrentRuns == nil || *cfg.MaxConcurrentRuns != 16 {
		t.Errorf("max_concurrent_runs = %v, want 16", cfg.MaxConcurrentRuns)
	}
}

// TestAgentYAMLBeforeEnrollment covers a node that has not enrolled: the
// file holds the documented defaults, and the only problem is the missing
// Gateway URL.
func TestAgentYAMLBeforeEnrollment(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.yaml")
	options := testOptions(Install)
	options.ConfigPath = path
	options.StateDir = filepath.Join(dir, "state")
	options.GatewayURL = ""
	if err := os.WriteFile(path, []byte(agentYAML(options)), 0o600); err != nil {
		t.Fatalf("write agent.yaml: %v", err)
	}
	_, problems, err := config.LoadAgent(path)
	if err != nil {
		t.Fatalf("LoadAgent: %v", err)
	}
	if len(problems) != 1 || problems[0].Field != "gateway_url" {
		t.Fatalf("problems = %v, want the missing gateway_url only", problems)
	}
	if !strings.Contains(agentYAML(options), "# gateway_url: https://gateway.example.com") {
		t.Error("the file does not show the commented gateway_url placeholder")
	}
}

// TestApplyRunsEveryStep covers the order of the calls that a plan makes.
func TestApplyRunsEveryStep(t *testing.T) {
	host := &fakeHost{account: testAccount()}
	plan, err := Build(testOptions(Install), testAccount())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	var printed []string
	if err := plan.Apply(host, func(step Step) { printed = append(printed, step.Description) }); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(host.calls) != len(plan.Steps) {
		t.Fatalf("calls = %d, want one per step (%d):\n%s",
			len(host.calls), len(plan.Steps), strings.Join(host.calls, "\n"))
	}
	if len(printed) != len(plan.Steps) {
		t.Fatalf("printed = %d, want one line per step (%d)", len(printed), len(plan.Steps))
	}
	if !strings.HasPrefix(host.calls[0], "mkdir /etc/hanse-node ") {
		t.Errorf("first call = %q, want the configuration directory", host.calls[0])
	}
	if last := host.calls[len(host.calls)-1]; last != "run systemctl restart hanse-node.service" {
		t.Errorf("last call = %q, want the service start", last)
	}
}

// TestApplyStopsAtTheFirstFailure covers a failed step: the plan stops, so
// a half-installed service is never enabled or started.
func TestApplyStopsAtTheFirstFailure(t *testing.T) {
	host := &fakeHost{account: testAccount(), failOn: "write " + testConfig}
	plan, err := Build(testOptions(Install), testAccount())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	err = plan.Apply(host, nil)
	if err == nil {
		t.Fatal("a failing step was ignored")
	}
	if !strings.Contains(err.Error(), "write the default "+testConfig) {
		t.Errorf("err = %v, want the failing step named", err)
	}
	for _, call := range host.calls {
		if strings.HasPrefix(call, "run ") {
			t.Fatalf("a command ran after the failure: %q", call)
		}
	}
}

// TestApplyIgnoresAnIdempotentStep covers uninstall on a node that never
// installed: a step that finds nothing to do does not stop the plan.
func TestApplyIgnoresAnIdempotentStep(t *testing.T) {
	host := &fakeHost{failOn: "remove"}
	plan, err := Build(Options{OS: "linux", Action: Uninstall}, Account{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if err := plan.Apply(host, nil); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !strings.Contains(strings.Join(host.calls, "\n"), "run systemctl daemon-reload") {
		t.Errorf("calls = %v, want the plan to finish", host.calls)
	}
}

// TestApplyReportsAnUnknownStep covers a plan that a caller built by hand.
func TestApplyReportsAnUnknownStep(t *testing.T) {
	plan := Plan{Steps: []Step{{Kind: "nonsense", Description: "do nothing"}}}
	err := plan.Apply(&fakeHost{}, nil)
	if err == nil || !strings.Contains(err.Error(), "do nothing") {
		t.Fatalf("err = %v, want the step named", err)
	}
}

// TestCommandLineQuotesArguments covers the command line that the service
// manager stores, which must survive the space in a program files path.
func TestCommandLineQuotesArguments(t *testing.T) {
	spec := Spec{
		Exe:  `C:\Program Files\Hanse\hanse-node.exe`,
		Args: []string{"run", "--config", `C:\ProgramData\Hanse\node\agent.yaml`},
	}
	want := `"C:\Program Files\Hanse\hanse-node.exe" run --config C:\ProgramData\Hanse\node\agent.yaml`
	if got := spec.CommandLine(); got != want {
		t.Errorf("CommandLine() = %q, want %q", got, want)
	}
}

// fakeHost records the calls that a plan makes. It is the whole operating
// system of the tests: no test registers a service or writes to /etc.
type fakeHost struct {
	calls         []string
	account       Account
	accountErr    error
	administrator bool
	// failOn refuses every call that contains this text.
	failOn string
}

// record appends one call and returns the configured failure.
func (f *fakeHost) record(format string, args ...any) error {
	call := fmt.Sprintf(format, args...)
	f.calls = append(f.calls, call)
	if f.failOn != "" && strings.Contains(call, f.failOn) {
		return errors.New("the test host refuses this call")
	}
	return nil
}

// Account returns the account that the test configured.
func (f *fakeHost) Account(string) (Account, error) {
	return f.account, f.accountErr
}

// Administrator returns the rights that the test configured.
func (f *fakeHost) Administrator() (bool, error) { return f.administrator, nil }

// MkdirAll records the directory and its permissions.
func (f *fakeHost) MkdirAll(path string, mode os.FileMode, owner *Owner) error {
	return f.record("mkdir %s %04o owner=%s", path, mode, ownerText(owner))
}

// WriteFile records the file, its permissions, and its content length.
func (f *fakeHost) WriteFile(path string, data []byte, mode os.FileMode, owner *Owner, createOnly bool) error {
	return f.record("write %s %04o owner=%s createOnly=%t bytes=%d",
		path, mode, ownerText(owner), createOnly, len(data))
}

// Chown records the new mode and owner.
func (f *fakeHost) Chown(path string, mode os.FileMode, owner *Owner) error {
	return f.record("chown %s %04o owner=%s", path, mode, ownerText(owner))
}

// RemoveFile records the removed file.
func (f *fakeHost) RemoveFile(path string) error { return f.record("remove %s", path) }

// Run records the service manager command.
func (f *fakeHost) Run(args []string) error { return f.record("run %s", strings.Join(args, " ")) }

// RegisterService records the service definition.
func (f *fakeHost) RegisterService(spec Spec) error {
	return f.record("register %s account=%s recovery=%d command=%q",
		spec.Name, spec.Account, len(spec.Recovery), spec.CommandLine())
}

// StartService records the started service.
func (f *fakeHost) StartService(name string) error { return f.record("start %s", name) }

// StopService records the stopped service.
func (f *fakeHost) StopService(name string) error { return f.record("stop %s", name) }

// DeleteService records the deleted service.
func (f *fakeHost) DeleteService(name string) error { return f.record("delete %s", name) }

// AddGroupMember records the membership that the plan adds.
func (f *fakeHost) AddGroupMember(group, member string) error {
	return f.record("add-member group=%s member=%s", group, member)
}

// RemoveGroupMember records the membership that the plan removes.
func (f *fakeHost) RemoveGroupMember(group, member string) error {
	return f.record("remove-member group=%s member=%s", group, member)
}

// Grant records the access that the plan grants.
func (f *fakeHost) Grant(path string, access Access) error {
	return f.record("grant %s %s", path, accessText(access))
}

// ownerText names a POSIX owner in a recorded call.
func ownerText(owner *Owner) string {
	if owner == nil {
		return "none"
	}
	return fmt.Sprintf("%d:%d", owner.UID, owner.GID)
}

// accessText names an access right in a recorded call.
func accessText(access Access) string {
	if access == AccessModify {
		return "modify"
	}
	return "read"
}
