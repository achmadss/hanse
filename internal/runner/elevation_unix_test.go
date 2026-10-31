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

// fakeSudoScript records how sudo was called and answers like sudo: it
// reads the password from the first stdin line and runs the command when
// the password matches. It never uses a cached credential.
const fakeSudoScript = `#!/bin/sh
for arg in "$@"; do
  case "$arg" in
    -v|-n) echo "forbidden sudo option $arg" >> "RECORD/argv"; exit 9 ;;
  esac
done
printf '%s\n' "$*" >> "RECORD/argv"
env >> "RECORD/env"
while [ "$1" != "--" ]; do shift; done
shift
IFS= read -r password
printf '%s\n' "$password" >> "RECORD/stdin"
if [ "$password" != "PASSWORD" ]; then
  exit 1
fi
exec "$@"
`

// fakeSudo is one fake sudo and the directory that records its calls.
type fakeSudo struct {
	dir string
}

// newFakeSudo writes the fake sudo and puts it first on PATH. password is
// the placeholder the fake accepts; record holds the argv, environment,
// and stdin the calls produced.
func newFakeSudo(t *testing.T, password string) *fakeSudo {
	t.Helper()
	dir := t.TempDir()
	record := filepath.Join(dir, "record")
	if err := os.MkdirAll(record, 0o700); err != nil {
		t.Fatalf("create record directory: %v", err)
	}
	script := strings.NewReplacer("RECORD", record, "PASSWORD", password).Replace(fakeSudoScript)
	if err := os.WriteFile(filepath.Join(dir, "sudo"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake sudo: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return &fakeSudo{dir: dir}
}

// record returns the contents of one record file, or "" when the fake did
// not write it.
func (f *fakeSudo) record(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(f.dir, "record", name))
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatalf("read the %s record: %v", name, err)
	}
	return string(data)
}

// TestVerifyCredentialUsesNoTimestamp covers the private pipe rule of
// docs/agent-protocol.md section 9 and the no-timestamp rule of the
// architecture's elevation section: sudo is called with `-k` and a
// command, never with `-v` or `-n`, and the password arrives on the first
// stdin line and nowhere else.
func TestVerifyCredentialUsesNoTimestamp(t *testing.T) {
	const password = "placeholder-good-password"
	sudo := newFakeSudo(t, password)
	if err := VerifyCredential("", password, "/bin/true"); err != nil {
		t.Fatalf("correct password: %v", err)
	}
	argv := sudo.record(t, "argv")
	if !strings.Contains(argv, "-k") {
		t.Errorf("sudo argv %q has no -k", argv)
	}
	if strings.Contains(argv, "-v") || strings.Contains(argv, "-n") {
		t.Errorf("sudo argv %q uses a cached credential", argv)
	}
	if strings.Contains(argv, password) {
		t.Errorf("sudo argv holds the password: %q", argv)
	}
	if env := sudo.record(t, "env"); strings.Contains(env, password) {
		t.Errorf("the sudo environment holds the password: %q", env)
	}
	if stdin := sudo.record(t, "stdin"); stdin != password+"\n" {
		t.Errorf("sudo stdin %q, want the password line only", stdin)
	}
}

// TestVerifyCredentialClassifiesFailures covers which failures count as a
// wrong password: only a failure where sudo read the password. A missing
// sudo or a refusal that never asks for a password is another error.
func TestVerifyCredentialClassifiesFailures(t *testing.T) {
	_ = newFakeSudo(t, "placeholder-good-password")
	if err := VerifyCredential("", "placeholder-wrong-password", "/bin/true"); !errors.Is(err, ErrPasswordRejected) {
		t.Errorf("wrong password: err = %v, want ErrPasswordRejected", err)
	}
	// A sudo that refuses without reading the password, like a policy
	// that forbids the command.
	refusing := filepath.Join(t.TempDir(), "sudo")
	if err := os.WriteFile(refusing, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("write refusing sudo: %v", err)
	}
	t.Setenv("PATH", filepath.Dir(refusing)+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := VerifyCredential("", "placeholder-wrong-password", "/bin/true"); err == nil ||
		errors.Is(err, ErrPasswordRejected) {
		t.Errorf("refused check: err = %v, want a non-rejection error", err)
	}
	// A PATH without sudo at all.
	t.Setenv("PATH", t.TempDir())
	if err := VerifyCredential("", "placeholder-wrong-password", "/bin/true"); err == nil ||
		errors.Is(err, ErrPasswordRejected) {
		t.Errorf("missing sudo: err = %v, want a non-rejection error", err)
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
