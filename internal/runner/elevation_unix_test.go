//go:build unix

package runner

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeSudoScript mimics sudo for the tests. MODE selects the behavior:
// accept reads the password and runs the command, reject asks for a
// password and rejects it, nopasswd runs the command without reading the
// password, and refuse denies the command without asking for one.
const fakeSudoScript = `#!/bin/sh
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
case "MODE" in
  accept)
    IFS= read -r password
    printf '%s\n' "$password" >> "$record/stdin"
    exec "$@"
    ;;
  reject)
    printf 'PROMPT' >&2
    IFS= read -r password
    printf '%s\n' "$password" >> "$record/stdin"
    exit 1
    ;;
  nopasswd)
    exec "$@"
    ;;
esac
exit 1
`

// fakeCheckScript implements the supervisor's check contract: it reports
// whether standard input still held the password line.
const fakeCheckScript = `#!/bin/sh
if IFS= read -r line; then
  printf '%s\n' 'UNREAD'
else
  printf '%s\n' 'READ'
fi
`

// fakeSudo is one fake sudo and the directory that records its calls.
type fakeSudo struct {
	dir    string
	record string
}

// newFakeSudo writes a fake sudo in one mode and puts it first on PATH. It
// also returns the path of a command that behaves like the supervisor's
// check mode.
func newFakeSudo(t *testing.T, mode string) *fakeSudo {
	t.Helper()
	dir := t.TempDir()
	record := filepath.Join(dir, "record")
	if err := os.MkdirAll(record, 0o700); err != nil {
		t.Fatalf("create record directory: %v", err)
	}
	replacer := strings.NewReplacer(
		"RECORD", record,
		"MODE", mode,
		"PROMPT", ElevationPrompt,
	)
	if err := os.WriteFile(filepath.Join(dir, "sudo"), []byte(replacer.Replace(fakeSudoScript)), 0o755); err != nil {
		t.Fatalf("write fake sudo: %v", err)
	}
	check := strings.NewReplacer(
		"READ", SupervisorCheckRead,
		"UNREAD", SupervisorCheckUnread,
	).Replace(fakeCheckScript)
	if err := os.WriteFile(filepath.Join(dir, "hanse-check"), []byte(check), 0o755); err != nil {
		t.Fatalf("write fake check: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return &fakeSudo{dir: dir, record: record}
}

// checkCommand returns the fake supervisor's check command.
func (f *fakeSudo) checkCommand() []string {
	return []string{filepath.Join(f.dir, "hanse-check")}
}

// recorded returns one record file, or "" when the fake did not write it.
func (f *fakeSudo) recorded(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(f.record, name))
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatalf("read the %s record: %v", name, err)
	}
	return string(data)
}

// withoutNoNewPrivileges points the no_new_privs check at a missing file,
// so a test machine that itself runs with no_new_privs does not change the
// outcome of a test about sudo. It restores the path when the test ends.
func withoutNoNewPrivileges(t *testing.T) {
	t.Helper()
	original := noNewPrivsFile
	noNewPrivsFile = filepath.Join(t.TempDir(), "missing-status")
	t.Cleanup(func() { noNewPrivsFile = original })
}

// TestVerifyCredentialUsesNoTimestamp covers the private stdin rule of
// docs/agent-protocol.md section 9 and the no-timestamp rule of the
// elevation section: sudo is called with `-k` and a command, never with
// `-v` or `-n`, and the password arrives on the first stdin line and
// nowhere else.
func TestVerifyCredentialUsesNoTimestamp(t *testing.T) {
	withoutNoNewPrivileges(t)
	const password = "placeholder-good-password"
	sudo := newFakeSudo(t, "accept")
	if err := VerifyCredential("", password, sudo.checkCommand()...); err != nil {
		t.Fatalf("correct password: %v", err)
	}
	argv := sudo.recorded(t, "argv")
	if !strings.Contains(argv, " -k ") {
		t.Errorf("sudo argv %q has no -k", argv)
	}
	if strings.Contains(argv, " -v ") || strings.Contains(argv, " -n ") {
		t.Errorf("sudo argv %q uses a cached credential", argv)
	}
	if strings.Contains(argv, password) {
		t.Errorf("sudo argv holds the password: %q", argv)
	}
	if env := sudo.recorded(t, "env"); strings.Contains(env, password) {
		t.Errorf("the sudo environment holds the password: %q", env)
	}
	if stdin := sudo.recorded(t, "stdin"); stdin != password+"\n" {
		t.Errorf("sudo stdin %q, want the password line only", stdin)
	}
}

// TestVerifyCredentialClassifiesFailures covers which failures count as a
// wrong password. Only a check where sudo asked for a password and
// rejected it counts; a policy refusal or a missing sudo does not.
func TestVerifyCredentialClassifiesFailures(t *testing.T) {
	withoutNoNewPrivileges(t)
	const password = "placeholder-good-password"
	rejecting := newFakeSudo(t, "reject")
	if err := VerifyCredential("", password, rejecting.checkCommand()...); !errors.Is(err, ErrPasswordRejected) {
		t.Errorf("wrong password: err = %v, want ErrPasswordRejected", err)
	}
	refusing := newFakeSudo(t, "refuse")
	if err := VerifyCredential("", password, refusing.checkCommand()...); err == nil ||
		errors.Is(err, ErrPasswordRejected) {
		t.Errorf("refused check: err = %v, want a non-counting refusal", err)
	}
	// A PATH without sudo at all.
	t.Setenv("PATH", t.TempDir())
	if err := VerifyCredential("", password, rejecting.checkCommand()...); err == nil ||
		errors.Is(err, ErrPasswordRejected) {
		t.Errorf("missing sudo: err = %v, want a non-counting refusal", err)
	}
}

// TestVerifyCredentialFailsClosedWithoutAPasswordPrompt covers a sudoers
// rule with NOPASSWD: sudo runs the check without reading the password, so
// any password would look valid. The check fails closed and does not count
// as a wrong password.
func TestVerifyCredentialFailsClosedWithoutAPasswordPrompt(t *testing.T) {
	withoutNoNewPrivileges(t)
	const password = "placeholder-any-password"
	sudo := newFakeSudo(t, "nopasswd")
	err := VerifyCredential("", password, sudo.checkCommand()...)
	if err == nil {
		t.Fatal("a sudo that never asked for a password was accepted")
	}
	if errors.Is(err, ErrPasswordRejected) {
		t.Errorf("err = %v, want a non-counting refusal", err)
	}
	if !strings.Contains(err.Error(), "NOPASSWD") {
		t.Errorf("err = %v, want a reason that names the sudoers rule", err)
	}
}

// TestVerifyCredentialRefusesNoNewPrivileges covers an agent unit that
// sets NoNewPrivileges=yes: sudo cannot gain root, so the check fails
// with a message that names the setting, and sudo never runs.
func TestVerifyCredentialRefusesNoNewPrivileges(t *testing.T) {
	const password = "placeholder-good-password"
	sudo := newFakeSudo(t, "accept")
	status := filepath.Join(t.TempDir(), "status")
	content := "Name:\thanse-node\nNoNewPrivs:\t1\nSeccomp:\t0\n"
	if err := os.WriteFile(status, []byte(content), 0o600); err != nil {
		t.Fatalf("write the status fixture: %v", err)
	}
	original := noNewPrivsFile
	noNewPrivsFile = status
	t.Cleanup(func() { noNewPrivsFile = original })

	err := VerifyCredential("", password, sudo.checkCommand()...)
	if !errors.Is(err, ErrNoNewPrivileges) {
		t.Fatalf("err = %v, want ErrNoNewPrivileges", err)
	}
	if !strings.Contains(err.Error(), "NoNewPrivileges") {
		t.Errorf("err = %v, want a message that names the unit setting", err)
	}
	if argv := sudo.recorded(t, "argv"); argv != "" {
		t.Errorf("sudo ran under no_new_privs: %q", argv)
	}
}

// TestParseNoNewPrivs covers the Linux status file parse: only a
// NoNewPrivs line with the value 1 means the flag is set.
func TestParseNoNewPrivs(t *testing.T) {
	tests := []struct {
		name   string
		status string
		want   bool
	}{
		{name: "set", status: "Name:\thanse-node\nNoNewPrivs:\t1\nSeccomp:\t0\n", want: true},
		{name: "clear", status: "Name:\thanse-node\nNoNewPrivs:\t0\n", want: false},
		{name: "missing", status: "Name:\thanse-node\n", want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseNoNewPrivs(tc.status); got != tc.want {
				t.Errorf("parseNoNewPrivs(%q) = %v, want %v", tc.status, got, tc.want)
			}
		})
	}
}

// TestPreparePrivilegedUsesNoTimestamp covers the launch command: sudo
// runs with `-k` and the supervisor, never with `-v` or `-n`, and the
// password becomes the first stdin line rather than an argument or an
// environment value.
func TestPreparePrivilegedUsesNoTimestamp(t *testing.T) {
	const password = "placeholder-good-password"
	workDir := t.TempDir()
	runner := &Runner{Supervisor: []string{"/usr/local/bin/hanse-node", "supervise"}}
	plan := &launchPlan{
		Entry:      []string{"sh", "handler.sh"},
		Dir:        t.TempDir(),
		Env:        []string{"PATH=/usr/bin"},
		WorkDir:    workDir,
		CancelFile: filepath.Join(workDir, "cancel"),
		Grace:      time.Second,
		Password:   password,
	}
	if err := preparePrivileged(runner, plan); err != nil {
		t.Fatalf("prepare the privileged launch: %v", err)
	}
	argv := strings.Join(plan.Entry, " ")
	if !strings.HasPrefix(argv, "sudo ") || !strings.Contains(argv, " -k ") {
		t.Errorf("command %q, want sudo with -k", argv)
	}
	if strings.Contains(argv, " -v ") || strings.Contains(argv, " -n ") {
		t.Errorf("command %q uses a cached credential", argv)
	}
	if strings.Contains(argv, password) {
		t.Errorf("command %q holds the password", argv)
	}
	for _, item := range plan.Env {
		if strings.Contains(item, password) {
			t.Errorf("the sudo environment holds the password: %q", item)
		}
	}
	if got := string(plan.StdinPrefix); got != password+"\n" {
		t.Errorf("stdin prefix %q, want the password line", got)
	}
	if plan.Password != "" {
		t.Error("the plan kept its copy of the password")
	}
}
