package nodeagent_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	herrors "github.com/achmadss/hanse/internal/errors"
	"github.com/achmadss/hanse/internal/runtime"
)

// The privileged integration tests run only on Linux and macOS: Windows
// has no sudo path, and its elevation is covered by the runner tests.
const (
	sudoPassword      = "placeholder-sudo-password"
	wrongSudoPassword = "placeholder-wrong-password"
)

// privilegedManifest is a capability with one privileged operation.
const privilegedManifest = `version: 1
id: priv
name: Privileged demo
description: Run one handler with administrator rights
contract: hanse.handler/v1

handler:
  kind: exec
  entry:
    linux: [sh, handler.sh]
    darwin: [sh, handler.sh]

operations:
  whoami:
    description: Report whether the handler runs with administrator rights
    mode: sync
    privileged: true
    arguments:
      type: object
      additionalProperties: false
`

// privilegedHandler answers with the HANSE_PRIVILEGED value it received.
const privilegedHandler = `#!/bin/sh
cat >/dev/null
printf '{"contract":"hanse.handler/v1","status":"ok","result":{"privileged":"%s"}}\n' "$HANSE_PRIVILEGED"
`

// fakeSudo is a sudo stand-in for the tests. It records every call, reads
// the password from the first stdin line, and runs the command that
// follows `--`. It refuses `-v` and `-n`, so a test proves the agent never
// relies on a cached credential.
const fakeSudo = `#!/bin/sh
record="RECORD"
for arg in "$@"; do
  case "$arg" in
    -v|-n) printf 'forbidden %s\n' "$arg" >> "$record/argv"; exit 9 ;;
  esac
done
printf '%s\n' "$*" >> "$record/argv"
env >> "$record/env"
while [ "$1" != "--" ]; do shift; done
shift
printf 'hanse-elevation-password:' >&2
IFS= read -r password
printf '%s\n' "$password" >> "$record/stdin"
if [ "$password" = "PASSWORD" ]; then
  exec "$@"
fi
exit 1
`

// newPrivilegedStack starts a stack whose agent may elevate, with a fake
// sudo on its PATH so no real password is needed.
func newPrivilegedStack(t *testing.T) *stack {
	t.Helper()
	return newPrivilegedStackWith(t, "  enabled: true\n")
}

// newPrivilegedStackWith starts the same stack with the elevation settings
// the test writes into agent.yaml.
func newPrivilegedStackWith(t *testing.T, elevationYAML string) *stack {
	t.Helper()
	if goruntime.GOOS == "windows" {
		t.Skip("the POSIX sudo and supervisor path does not exist on Windows")
	}
	sudoDir := t.TempDir()
	recordDir := t.TempDir()
	script := strings.NewReplacer("RECORD", recordDir, "PASSWORD", sudoPassword).Replace(fakeSudo)
	if err := os.WriteFile(filepath.Join(sudoDir, "sudo"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake sudo: %v", err)
	}
	env := append(os.Environ(),
		"PATH="+sudoDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return newStackWithOptions(t, stackOptions{
		sudoRecord: recordDir,
		capabilities: []stackCapability{{
			id:       "priv",
			manifest: privilegedManifest,
			handler:  privilegedHandler,
		}},
		agentYAML: "\nelevation:\n" + elevationYAML,
		env:       env,
	})
}

// TestPrivilegedRunOutsideTheAllowList covers agent.yaml's
// `elevation.operations`: the node refuses an operation it does not list.
func TestPrivilegedRunOutsideTheAllowList(t *testing.T) {
	st := newPrivilegedStackWith(t, "  enabled: true\n  operations: [other.op]\n")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	_, err := st.callWith(ctx, t, "priv", "op_not_listed", "whoami", map[string]any{}, sudoPassword)
	if !errors.Is(err, herrors.ElevationDenied) {
		t.Fatalf("err = %v, want elevation_denied", err)
	}
}

// TestPrivilegedRunElevatesTheHandler covers the correct password: the
// agent checks it with sudo, runs the handler through the root supervisor,
// and sets HANSE_PRIVILEGED.
func TestPrivilegedRunElevatesTheHandler(t *testing.T) {
	st := newPrivilegedStack(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	response, err := st.callWith(ctx, t, "priv", "op_whoami", "whoami", map[string]any{}, sudoPassword)
	if err != nil {
		t.Fatalf("execute priv.whoami: %v\nagent output:\n%s", err, st.output.String())
	}
	if response.Status != runtime.StatusOK {
		t.Fatalf("status %q, want ok: %+v", response.Status, response)
	}
	if !strings.Contains(string(response.Result), `"privileged":"1"`) {
		t.Errorf("result %s, want HANSE_PRIVILEGED=1", response.Result)
	}
	assertSudoCalls(t, st, sudoPassword)
}

// assertSudoCalls proves how the agent called sudo: with -k and a command,
// never with -v or -n, with no cached credential, and with the password as
// the first stdin line and nowhere else.
func assertSudoCalls(t *testing.T, st *stack, password string) {
	t.Helper()
	argv, err := os.ReadFile(filepath.Join(st.sudoRecord, "argv"))
	if err != nil {
		t.Fatalf("read the sudo argv record: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(argv)), "\n")
	if len(lines) < 2 {
		t.Fatalf("sudo calls %q, want the check and the launch", string(argv))
	}
	for _, line := range lines {
		if strings.Contains(line, "forbidden") {
			t.Errorf("the agent used a cached credential: %q", line)
		}
		if !strings.Contains(line, " -k ") {
			t.Errorf("sudo call %q has no -k", line)
		}
		if strings.Contains(line, password) {
			t.Errorf("sudo argv holds the password: %q", line)
		}
	}
	env, err := os.ReadFile(filepath.Join(st.sudoRecord, "env"))
	if err != nil {
		t.Fatalf("read the sudo environment record: %v", err)
	}
	if strings.Contains(string(env), password) {
		t.Error("the sudo environment holds the password")
	}
	stdin, err := os.ReadFile(filepath.Join(st.sudoRecord, "stdin"))
	if err != nil {
		t.Fatalf("read the sudo stdin record: %v", err)
	}
	seen := strings.Split(strings.TrimSpace(string(stdin)), "\n")
	if len(seen) != len(lines) {
		t.Fatalf("sudo read %d passwords for %d calls", len(seen), len(lines))
	}
	for _, line := range seen {
		if line != password {
			t.Errorf("sudo read %q on stdin, want the password line", line)
		}
	}
}

// TestPrivilegedRunRejectsAWrongPassword covers the wrong password: the
// handler never starts and the run fails with elevation_denied.
func TestPrivilegedRunRejectsAWrongPassword(t *testing.T) {
	st := newPrivilegedStack(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	_, err := st.callWith(ctx, t, "priv", "op_wrong", "whoami", map[string]any{}, wrongSudoPassword)
	if err == nil {
		t.Fatal("a wrong elevation password was accepted")
	}
	if !errors.Is(err, herrors.ElevationDenied) {
		t.Fatalf("err = %v, want the elevation_denied code", err)
	}
	if strings.Contains(err.Error(), wrongSudoPassword) {
		t.Error("the error message holds the elevation password")
	}
}

// TestPrivilegedRunsLockOutAfterWrongPasswords covers the lockout: five
// wrong passwords refuse elevation for the lockout duration, so even the
// correct password fails.
func TestPrivilegedRunsLockOutAfterWrongPasswords(t *testing.T) {
	st := newPrivilegedStack(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	for attempt := 0; attempt < 5; attempt++ {
		_, err := st.callWith(ctx, t, "priv",
			"op_lockout_"+string(rune('a'+attempt)), "whoami", map[string]any{}, wrongSudoPassword)
		if !errors.Is(err, herrors.ElevationDenied) {
			t.Fatalf("attempt %d: err = %v, want elevation_denied", attempt, err)
		}
	}
	_, err := st.callWith(ctx, t, "priv", "op_locked_out", "whoami", map[string]any{}, sudoPassword)
	if !errors.Is(err, herrors.ElevationDenied) {
		t.Fatalf("the lockout accepted a correct password: %v", err)
	}
}

// TestPrivilegedPasswordNeverLeavesMemory proves that the password appears
// in no log line, no agent output, and no file under the agent's state
// directory.
func TestPrivilegedPasswordNeverLeavesMemory(t *testing.T) {
	st := newPrivilegedStack(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, err := st.callWith(ctx, t, "priv", "op_no_leak", "whoami", map[string]any{}, sudoPassword); err != nil {
		t.Fatalf("execute priv.whoami: %v", err)
	}
	for _, line := range st.logs.linesCopy() {
		if strings.Contains(line.line, sudoPassword) {
			t.Errorf("log line %q holds the elevation password", line.line)
		}
	}
	if strings.Contains(st.output.String(), sudoPassword) {
		t.Errorf("the agent output holds the elevation password:\n%s", st.output.String())
	}
	err := filepath.Walk(st.stateDir, func(path string, info os.FileInfo, err error) error {
		if os.IsNotExist(err) {
			// The agent rewrites its journal while it runs; a file that
			// vanished is nothing to read.
			return nil
		}
		if err != nil || info.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(data), sudoPassword) {
			t.Errorf("the file %s holds the elevation password", path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk the state directory: %v", err)
	}
}
