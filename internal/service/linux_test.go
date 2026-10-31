package service

import (
	"strings"
	"testing"
)

// TestLinuxInstallPlan covers the systemd install: the unit runs as the
// chosen account, restarts on failure, and the hardening leaves sudo and
// privileged handlers able to work.
func TestLinuxInstallPlan(t *testing.T) {
	plan, err := Build(testOptions(Install), testAccount())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if plan.Service != LinuxUnitPath {
		t.Errorf("service = %q, want %q", plan.Service, LinuxUnitPath)
	}
	if plan.Account != "hanse-node" {
		t.Errorf("account = %q, want the service account", plan.Account)
	}
	want := []StepKind{
		StepMkdir, StepWriteFile, StepMkdir, StepChown,
		StepWriteFile, StepRun, StepRun, StepRun,
	}
	if got := kinds(plan); len(got) != len(want) {
		t.Fatalf("steps = %v, want %v", got, want)
	} else {
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("steps = %v, want %v", got, want)
			}
		}
	}

	unit := stepAt(t, plan, LinuxUnitPath)
	if unit.Mode != 0o644 {
		t.Errorf("unit mode = %04o, want 0644", unit.Mode)
	}
	for _, line := range []string{
		"User=hanse-node\n",
		"Group=hanse-node\n",
		"ExecStart=" + testBinary + " run --config " + testConfig + "\n",
		"Restart=on-failure\n",
		"RestartSec=2s\n",
		"KillMode=mixed\n",
		"WantedBy=multi-user.target\n",
	} {
		if !strings.Contains(unit.Content, line) {
			t.Errorf("unit is missing %q:\n%s", line, unit.Content)
		}
	}
	for _, forbidden := range []string{"NoNewPrivileges", "CapabilityBoundingSet", "ProtectSystem"} {
		if strings.Contains(unit.Content, forbidden) {
			t.Errorf("unit sets %s, which breaks sudo and privileged handlers", forbidden)
		}
	}

	commands := runSteps(plan)
	wantCommands := []string{
		"systemctl daemon-reload",
		"systemctl enable hanse-node.service",
		"systemctl restart hanse-node.service",
	}
	if len(commands) != len(wantCommands) {
		t.Fatalf("commands = %v, want %v", commands, wantCommands)
	}
	for i := range wantCommands {
		if commands[i] != wantCommands[i] {
			t.Fatalf("commands = %v, want %v", commands, wantCommands)
		}
	}
}

// TestLinuxInstallSecuresTheFiles covers the permissions of the two files
// that decide what the agent may read and who may change it: agent.yaml is
// root's with the service account's group, and the credential belongs to
// the service account.
func TestLinuxInstallSecuresTheFiles(t *testing.T) {
	plan, err := Build(testOptions(Install), testAccount())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	configFile := stepAt(t, plan, testConfig)
	credential := stepAt(t, plan, testState+"/credential")
	if configFile.Kind != StepWriteFile {
		t.Fatalf("agent.yaml step = %q, want a write", configFile.Kind)
	}
	if configFile.Mode != 0o640 {
		t.Errorf("agent.yaml mode = %04o, want 0640", configFile.Mode)
	}
	if configFile.Owner == nil || configFile.Owner.UID != 0 || configFile.Owner.GID != 1500 {
		t.Errorf("agent.yaml owner = %v, want 0:1500", configFile.Owner)
	}
	if !configFile.CreateOnly {
		t.Error("agent.yaml would be replaced on a second install")
	}
	if credential.Kind != StepChown {
		t.Fatalf("credential step = %q, want a chown", credential.Kind)
	}
	if credential.Mode != 0o600 {
		t.Errorf("credential mode = %04o, want 0600", credential.Mode)
	}
	if credential.Owner == nil || credential.Owner.UID != 1500 {
		t.Errorf("credential owner = %v, want the service account", credential.Owner)
	}
	if !credential.IgnoreFailure {
		t.Error("a node that has not enrolled yet would stop the install")
	}
}

// TestLinuxInstallDoesNotStartBeforeEnrollment covers an install on a node
// without a credential: the unit is enabled but not started, so it does not
// crash-loop before the operator enrolls the node.
func TestLinuxInstallDoesNotStartBeforeEnrollment(t *testing.T) {
	options := testOptions(Install)
	options.Start = false
	plan, err := Build(options, testAccount())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	for _, command := range runSteps(plan) {
		if strings.Contains(command, "restart") || strings.Contains(command, "start") {
			t.Errorf("command %q starts the agent before enrollment", command)
		}
	}
}

// TestLinuxUninstallPlan covers the removal that keeps the node's
// configuration and credential.
func TestLinuxUninstallPlan(t *testing.T) {
	plan, err := Build(Options{OS: "linux", Action: Uninstall}, Account{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	want := []StepKind{StepRun, StepRemoveFile, StepRun}
	got := kinds(plan)
	if len(got) != len(want) {
		t.Fatalf("steps = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("steps = %v, want %v", got, want)
		}
	}
	remove := step(t, plan, StepRemoveFile)
	if remove.Target != LinuxUnitPath {
		t.Errorf("removed %q, want %q", remove.Target, LinuxUnitPath)
	}
	if !remove.IgnoreFailure {
		t.Error("removing an absent unit would fail")
	}
	commands := runSteps(plan)
	if commands[0] != "systemctl disable --now hanse-node.service" {
		t.Errorf("first command = %q, want the unit stopped and disabled", commands[0])
	}
	if commands[1] != "systemctl daemon-reload" {
		t.Errorf("second command = %q, want the manager reloaded", commands[1])
	}
}

// TestLinuxUnitQuotesAPathWithSpaces covers a program path that needs
// quoting in an ExecStart line.
func TestLinuxUnitQuotesAPathWithSpaces(t *testing.T) {
	options := testOptions(Install)
	options.Binary = "/opt/hanse node/hanse-node"
	unit := linuxUnit(options, testAccount())
	if !strings.Contains(unit, `ExecStart="/opt/hanse node/hanse-node" run`) {
		t.Errorf("unit does not quote the program path:\n%s", unit)
	}
}
