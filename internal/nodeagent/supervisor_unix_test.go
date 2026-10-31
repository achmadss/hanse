//go:build unix

package nodeagent

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/achmadss/hanse/internal/runner"
)

// supervisorControlFile writes the handler environment for the supervisor.
func supervisorControlFile(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "supervisor.json")
	data, err := json.Marshal(runner.SupervisorControl{
		Dir: dir,
		Env: []string{"PATH=" + os.Getenv("PATH"), "SUPERVISOR_TEST_VALUE=placeholder"},
	})
	if err != nil {
		t.Fatalf("encode control file: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write control file: %v", err)
	}
	return path
}

// TestSuperviseRunsTheHandler covers the happy path: the request on stdin
// reaches the handler, its output comes back, and its exit code is
// mirrored.
func TestSuperviseRunsTheHandler(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := SuperviseHandler(SuperviseSpec{
		Entry:       []string{"sh", "-c", `read line; printf 'got %s' "$line"; printf '%s' "$SUPERVISOR_TEST_VALUE" >&2`},
		ControlFile: supervisorControlFile(t, t.TempDir()),
		Stdin:       strings.NewReader("hello\n"),
		Stdout:      &stdout,
		Stderr:      &stderr,
		Grace:       time.Second,
	})
	if code != 0 {
		t.Fatalf("exit code %d, want 0: %s", code, stderr.String())
	}
	if got := stdout.String(); got != "got hello" {
		t.Errorf("stdout %q, want the handler's answer", got)
	}
	if got := stderr.String(); got != "placeholder" {
		t.Errorf("stderr %q, want the control file's environment", got)
	}
}

// TestSuperviseMirrorsTheExitCode covers a handler that fails.
func TestSuperviseMirrorsTheExitCode(t *testing.T) {
	code := SuperviseHandler(SuperviseSpec{
		Entry:       []string{"sh", "-c", "exit 3"},
		ControlFile: supervisorControlFile(t, t.TempDir()),
		Grace:       time.Second,
	})
	if code != 3 {
		t.Errorf("exit code %d, want 3", code)
	}
}

// TestSuperviseStopsOnTheCancelFile covers the stop request of a
// privileged run: the agent cannot signal a root process, so the cancel
// file drives SIGTERM and the grace-period kill.
func TestSuperviseStopsOnTheCancelFile(t *testing.T) {
	dir := t.TempDir()
	cancelFile := filepath.Join(dir, "cancel")
	done := make(chan int, 1)
	go func() {
		done <- SuperviseHandler(SuperviseSpec{
			Entry:       []string{"sh", "-c", "sleep 30"},
			ControlFile: supervisorControlFile(t, dir),
			CancelFile:  cancelFile,
			Grace:       200 * time.Millisecond,
			Poll:        20 * time.Millisecond,
		})
	}()
	time.Sleep(150 * time.Millisecond)
	if err := os.WriteFile(cancelFile, nil, 0o600); err != nil {
		t.Fatalf("write cancel file: %v", err)
	}
	select {
	case code := <-done:
		if code == 0 {
			t.Error("the stopped handler reported success")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the supervisor did not stop the handler")
	}
}

// TestSuperviseKillsTheHandlerWhenTheAgentDies covers the parent watch:
// the handler must not outlive the agent.
func TestSuperviseKillsTheHandlerWhenTheAgentDies(t *testing.T) {
	var agentGone atomic.Bool
	done := make(chan int, 1)
	go func() {
		done <- SuperviseHandler(SuperviseSpec{
			Entry:       []string{"sh", "-c", "sleep 30"},
			ControlFile: supervisorControlFile(t, t.TempDir()),
			Grace:       200 * time.Millisecond,
			Poll:        20 * time.Millisecond,
			Parent: func() int {
				if agentGone.Load() {
					return 1
				}
				return 4242
			},
		})
	}()
	time.Sleep(150 * time.Millisecond)
	agentGone.Store(true)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the supervisor kept the handler alive after the agent exited")
	}
}

// TestSuperviseKillsTheHandlerWhenTheAgentPIDDies covers the watch on the
// agent process that the runner passes: the handler must not outlive the
// agent even when sudo runs the supervisor as its own child.
func TestSuperviseKillsTheHandlerWhenTheAgentPIDDies(t *testing.T) {
	agent := exec.Command("sh", "-c", "sleep 1")
	if err := agent.Start(); err != nil {
		t.Fatalf("start the agent stand-in: %v", err)
	}
	done := make(chan int, 1)
	go func() {
		done <- SuperviseHandler(SuperviseSpec{
			Entry:       []string{"sh", "-c", "sleep 30"},
			ControlFile: supervisorControlFile(t, t.TempDir()),
			Grace:       200 * time.Millisecond,
			Poll:        20 * time.Millisecond,
			AgentPID:    agent.Process.Pid,
		})
	}()
	start := time.Now()
	_ = agent.Wait()
	select {
	case <-done:
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Errorf("the supervisor stopped after %s", elapsed)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the supervisor kept the handler alive after the agent exited")
	}
}
