package service

import (
	"strings"
	"testing"
)

// TestWindowsInstallPlan covers the service install: a real Windows
// service that starts automatically, runs as the virtual account, gains
// administrator rights through the local Administrators group by its
// well-known SID, and restarts after a crash.
func TestWindowsInstallPlan(t *testing.T) {
	plan, err := Build(windowsOptions(Install), Account{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	want := []StepKind{
		StepMkdir, StepRegisterService, StepAddGroupMember,
		StepWriteFile, StepGrant, StepMkdir, StepGrant, StepGrant, StepStartService,
	}
	got := kinds(plan)
	if len(got) != len(want) {
		t.Fatalf("steps = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("steps = %v, want %v", got, want)
		}
	}

	register := step(t, plan, StepRegisterService)
	if register.Service == nil {
		t.Fatal("the register step carries no definition")
	}
	spec := *register.Service
	if spec.Name != WindowsName {
		t.Errorf("service name = %q, want %q", spec.Name, WindowsName)
	}
	if spec.Account != `NT SERVICE\hanse-node` {
		t.Errorf("account = %q, want the virtual service account", spec.Account)
	}
	if len(spec.Recovery) != 3 {
		t.Errorf("recovery actions = %d, want 3 restart delays", len(spec.Recovery))
	}
	if spec.RecoveryReset.Hours() != 24 {
		t.Errorf("recovery reset = %s, want 24h", spec.RecoveryReset)
	}
	if got, want := spec.CommandLine(),
		`"C:\Program Files\Hanse\hanse-node.exe" run --config C:\ProgramData\Hanse\node\agent.yaml`; got != want {
		t.Errorf("command = %q, want %q", got, want)
	}

	member := step(t, plan, StepAddGroupMember)
	if member.Group != "S-1-5-32-544" {
		t.Errorf("group = %q, want the Administrators SID", member.Group)
	}
	if member.Member != `NT SERVICE\hanse-node` {
		t.Errorf("member = %q, want the service account", member.Member)
	}

	// The service exists before the files name its account, because the
	// virtual account has no SID until the manager creates the service.
	if memberIndex := indexOf(plan, StepAddGroupMember); !existsBefore(plan, StepRegisterService, memberIndex) {
		t.Error("the group membership is added before the service exists")
	}

	// The configuration stays writable by the administrator only.
	configFile := stepAt(t, plan, `C:\ProgramData\Hanse\node\agent.yaml`)
	if configFile.Kind != StepWriteFile || !configFile.CreateOnly {
		t.Fatalf("agent.yaml step = %v, want a create-only write", configFile)
	}
	if configFile.Mode != 0o600 {
		t.Errorf("agent.yaml mode = %04o, want 0600", configFile.Mode)
	}
	stateFile := stepAt(t, plan, `C:\ProgramData\Hanse\node\state`)
	if stateFile.Kind != StepMkdir {
		t.Errorf("state step = %q, want a mkdir", stateFile.Kind)
	}
	if !strings.Contains(strings.Join(grants(plan), "\n"),
		`grant C:\ProgramData\Hanse\node\state modify`) {
		t.Errorf("grants = %v, want the state directory for the service account", grants(plan))
	}
}

// TestWindowsInstallBeforeEnrollment covers an install on a node without a
// credential: the service is registered but not started.
func TestWindowsInstallBeforeEnrollment(t *testing.T) {
	options := windowsOptions(Install)
	options.Start = false
	plan, err := Build(options, Account{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	for _, candidate := range plan.Steps {
		if candidate.Kind == StepStartService {
			t.Fatal("the service is started before enrollment")
		}
	}
}

// TestWindowsUninstallPlan covers the removal that also undoes the group
// membership. The membership goes first, while the virtual account still
// exists.
func TestWindowsUninstallPlan(t *testing.T) {
	plan, err := Build(windowsOptions(Uninstall), Account{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	want := []StepKind{StepStopService, StepRemoveGroupMember, StepDeleteService}
	got := kinds(plan)
	if len(got) != len(want) {
		t.Fatalf("steps = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("steps = %v, want %v", got, want)
		}
	}
	for _, candidate := range plan.Steps {
		if !candidate.IgnoreFailure {
			t.Errorf("step %q fails on a node that never installed", candidate.Description)
		}
	}
	member := step(t, plan, StepRemoveGroupMember)
	if member.Group != AdministratorsSID || member.Member != WindowsAccount {
		t.Errorf("removed %s from %s, want the service account from the Administrators SID",
			member.Member, member.Group)
	}
}

// grants returns the recorded grant calls of a plan, without running it.
func grants(plan Plan) []string {
	var found []string
	for _, candidate := range plan.Steps {
		if candidate.Kind == StepGrant {
			access := "read"
			if candidate.Access == AccessModify {
				access = "modify"
			}
			found = append(found, "grant "+candidate.Target+" "+access)
		}
	}
	return found
}

// indexOf returns the position of the only step of one kind.
func indexOf(plan Plan, kind StepKind) int {
	for i, candidate := range plan.Steps {
		if candidate.Kind == kind {
			return i
		}
	}
	return -1
}

// existsBefore reports whether the step of that kind came before index.
func existsBefore(plan Plan, kind StepKind, index int) bool {
	at := indexOf(plan, kind)
	return at >= 0 && at < index
}
