//go:build unix

package runner

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"

	herrors "github.com/achmadss/hanse/internal/errors"
)

// processOptions is the platform state of one launch that must outlive
// Start. POSIX needs none: the launcher itself carries the privilege.
type processOptions struct{}

// close releases the options. POSIX needs nothing.
func (o *processOptions) close() {}

// configureProcess starts the handler as the leader of a new process group
// (docs/architecture.md section 8.5, docs/handler-contract.md section
// 7.2). A privileged run reaches this point already wrapped in sudo, so
// the process group is the one the agent can still signal.
func configureProcess(cmd *exec.Cmd, _ bool) (*processOptions, error) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return nil, nil
}

// preparePrivileged wraps the handler in sudo and the agent's root
// supervisor. sudo checks the password itself and starts the supervisor as
// root; the supervisor starts the handler and carries out every stop
// itself, because the agent's account cannot signal a root process
// (docs/handler-contract.md section 7.2).
//
// sudo runs with `-S -k` and a command, so `-k` both ignores any cached
// credential and leaves no new timestamp behind: the launch never depends
// on a cached credential, and a later process the agent starts cannot run
// sudo without a password. The password reaches sudo on its own stdin,
// never in the argument list or the environment, and the request follows
// on the same pipe because sudo reads the password one byte at a time up
// to the newline.
func preparePrivileged(r *Runner, plan *launchPlan) error {
	supervisor, err := r.supervisor()
	if err != nil {
		return err
	}
	control, err := json.Marshal(SupervisorControl{Dir: plan.Dir, Env: plan.Env})
	if err != nil {
		return herrors.New(herrors.Internal,
			fmt.Sprintf("the supervisor control file cannot be encoded: %s", err), nil)
	}
	controlPath := filepath.Join(plan.WorkDir, "supervisor.json")
	if err := os.WriteFile(controlPath, control, 0o600); err != nil {
		return herrors.New(herrors.Internal,
			fmt.Sprintf("the supervisor control file cannot be written: %s", err), nil)
	}
	argv := append([]string{"sudo", "-S", "-k", "-p", "", "--"}, supervisor...)
	argv = append(argv,
		"--dir", plan.Dir,
		"--grace", plan.Grace.String(),
		"--cancel-file", plan.CancelFile,
		"--control", controlPath,
		// The supervisor watches the agent itself, because sudo can run
		// the supervisor as a child of its own helper process.
		"--agent-pid", strconv.Itoa(os.Getpid()),
		"--")
	argv = append(argv, plan.Entry...)
	plan.Entry = argv
	plan.Env = sudoEnv()
	plan.StdinPrefix = []byte(plan.Password + "\n")
	// Drop the plan's own reference now that the bytes exist.
	plan.Password = ""
	return nil
}

// sudoEnv is the environment of the sudo process. The handler's own
// environment travels in the control file, so sudo needs only what it
// takes to run.
func sudoEnv() []string {
	env := make([]string, 0, 4)
	for _, name := range []string{"PATH", "LANG", "LC_ALL", "TERM"} {
		if value, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+value)
		}
	}
	return env
}
