//go:build windows

package runner

import (
	"context"
	"os"
	"strings"
	"testing"
)

// TestRunEnvironmentWindowsBaseline covers the Windows part of
// docs/handler-contract.md section 4.2: a handler receives the system
// variables that Windows programs expect, with the values of the agent's own
// environment.
func TestRunEnvironmentWindowsBaseline(t *testing.T) {
	helperWantMode(t, "env")
	logs := &logCollector{}
	if _, err := newTestRunner(t).Run(context.Background(), helperSpec(t, "env", logs.log)); err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, name := range windowsBaselineVars {
		want := os.Getenv(name)
		if want == "" {
			// ProgramFiles(x86) exists only in a 64-bit session, and
			// PSModulePath belongs to the PowerShell that started the
			// agent, so a service or a non-PowerShell parent may not
			// hold it. Every other baseline variable belongs to every
			// Windows session, so the agent must hold it to pass it on.
			if name == "ProgramFiles(x86)" || name == "PSModulePath" {
				continue
			}
			t.Fatalf("the test process has no %s to pass on to the handler", name)
		}
		if line := "ENV " + name + "=" + want; !logs.contains(line) {
			t.Errorf("the handler did not receive %q: %v", line, logs.all())
		}
	}
	if want := os.Getenv("USERPROFILE"); want != "" && !logs.contains("ENV USERPROFILE="+want) {
		t.Errorf("the handler did not receive the user profile %q: %v", want, logs.all())
	}
}

// TestRunEnvironmentSkipsAMissingWindowsVariable proves that the runner adds
// a baseline variable only when the agent holds it. An empty value would tell
// a Windows program that the directory exists and is empty.
func TestRunEnvironmentSkipsAMissingWindowsVariable(t *testing.T) {
	helperWantMode(t, "env")
	t.Setenv("windir", "")
	logs := &logCollector{}
	if _, err := newTestRunner(t).Run(context.Background(), helperSpec(t, "env", logs.log)); err != nil {
		t.Fatalf("run: %v", err)
	}
	reported := false
	for _, line := range logs.all() {
		if !strings.HasPrefix(line, "ENV windir=") {
			continue
		}
		reported = true
		if line != "ENV windir=" {
			t.Errorf("the runner passed a windir value that the agent does not hold: %q", line)
		}
	}
	if !reported {
		t.Fatalf("the handler did not report windir: %v", logs.all())
	}
}
