package service

import (
	"strings"
	"testing"
)

// TestDarwinInstallPlan covers the launchd daemon: it lives in the daemons
// directory, runs as the chosen account, restarts on a crash, and is
// reloaded before its definition changes.
func TestDarwinInstallPlan(t *testing.T) {
	options := testOptions(Install)
	options.OS = "darwin"
	options.ConfigPath = "/usr/local/etc/hanse-node/agent.yaml"
	plan, err := Build(options, testAccount())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if plan.Service != DarwinPlistPath {
		t.Errorf("service = %q, want %q", plan.Service, DarwinPlistPath)
	}
	if plan.Account != "hanse-node" {
		t.Errorf("account = %q, want the service account", plan.Account)
	}
	plist := stepAt(t, plan, DarwinPlistPath)
	if plist.Kind != StepWriteFile {
		t.Fatalf("plist step = %q, want a write", plist.Kind)
	}
	if plist.Mode != 0o644 {
		t.Errorf("plist mode = %04o, want 0644", plist.Mode)
	}
	for _, fragment := range []string{
		"<string>" + DarwinLabel + "</string>",
		"<string>" + testBinary + "</string>",
		"<string>run</string>",
		"<string>" + options.ConfigPath + "</string>",
		"<key>UserName</key>\n\t<string>hanse-node</string>",
		"<key>RunAtLoad</key>\n\t<true/>",
		"<key>SuccessfulExit</key>\n\t\t<false/>",
	} {
		if !strings.Contains(plist.Content, fragment) {
			t.Errorf("plist is missing %q:\n%s", fragment, plist.Content)
		}
	}
	if !strings.HasPrefix(DarwinPlistPath, "/Library/LaunchDaemons/") {
		t.Fatalf("plist path = %q, want the daemons directory", DarwinPlistPath)
	}

	// The old definition is unloaded before the new plist replaces it,
	// because launchd keeps the definition it loaded.
	var unload, write, bootstrap, enable, start int
	for i, candidate := range plan.Steps {
		switch {
		case candidate.Kind == StepRun && len(candidate.Args) > 1 && candidate.Args[1] == "bootout":
			unload = i
		case candidate.Target == DarwinPlistPath:
			write = i
		case candidate.Kind == StepRun && len(candidate.Args) > 1 && candidate.Args[1] == "bootstrap":
			bootstrap = i
		case candidate.Kind == StepRun && len(candidate.Args) > 1 && candidate.Args[1] == "enable":
			enable = i
		case candidate.Kind == StepRun && len(candidate.Args) > 1 && candidate.Args[1] == "kickstart":
			start = i
		}
	}
	if unload >= write || write >= bootstrap || bootstrap >= enable || enable >= start {
		t.Fatalf("step order unload=%d write=%d bootstrap=%d enable=%d start=%d is wrong",
			unload, write, bootstrap, enable, start)
	}
	if got := strings.Join(plan.Steps[bootstrap].Args, " "); got != "launchctl bootstrap system "+DarwinPlistPath {
		t.Errorf("bootstrap = %q", got)
	}
}

// TestDarwinUninstallPlan covers the removal that keeps the node's
// configuration and credential.
func TestDarwinUninstallPlan(t *testing.T) {
	options := testOptions(Uninstall)
	options.OS = "darwin"
	plan, err := Build(options, Account{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got := kinds(plan); len(got) != 2 || got[0] != StepRun || got[1] != StepRemoveFile {
		t.Fatalf("steps = %v, want unload and remove", got)
	}
	if plan.Steps[0].Args[1] != "bootout" || !plan.Steps[0].IgnoreFailure {
		t.Errorf("first step = %v, want a tolerated unload", plan.Steps[0])
	}
	if plan.Steps[1].Target != DarwinPlistPath {
		t.Errorf("removed %q, want %q", plan.Steps[1].Target, DarwinPlistPath)
	}
}

// TestDarwinPlistEscapesAPath covers a program path with an XML character,
// which would otherwise produce a plist that launchd refuses.
func TestDarwinPlistEscapesAPath(t *testing.T) {
	options := testOptions(Install)
	options.OS = "darwin"
	options.Binary = "/opt/a&b/hanse-node"
	plist := darwinPlist(options, testAccount())
	if !strings.Contains(plist, "<string>/opt/a&amp;b/hanse-node</string>") {
		t.Errorf("plist does not escape the path:\n%s", plist)
	}
}
