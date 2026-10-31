//go:build unix

package nodeagent

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/achmadss/hanse/internal/runner"
)

// DefaultSupervisorPoll is how often the supervisor checks the cancel file
// and the agent process. Short enough to notice a stop promptly, long
// enough to cost nothing.
const DefaultSupervisorPoll = 100 * time.Millisecond

// supervisorStartFailed is the exit code of a supervisor that could not
// start its handler. 125 is the shell convention for "the command cannot
// execute" and never collides with the handler's own 0 or 1.
const supervisorStartFailed = 125

// SuperviseSpec is one privileged handler the root supervisor runs
// (docs/handler-contract.md section 7.2). The agent's account cannot
// signal a root process, so every stop reaches the handler through this
// supervisor: a stop request is the cancel file, which is why the agent
// writes it before it signals anything.
type SuperviseSpec struct {
	// Dir is the handler's working directory.
	Dir string
	// Grace is the time between SIGTERM and SIGKILL. Zero means 10s.
	Grace time.Duration
	// CancelFile is the file the agent creates to request a stop.
	CancelFile string
	// ControlFile holds the handler's environment, which sudo strips.
	ControlFile string
	// Entry is the handler's entry command.
	Entry []string
	// Stdin, Stdout, and Stderr pass straight through to the handler.
	Stdin          io.Reader
	Stdout, Stderr io.Writer
	// Poll is how often the stop conditions are checked. Zero means
	// DefaultSupervisorPoll.
	Poll time.Duration
	// Parent returns the process ID the supervisor watches. Zero means
	// os.Getppid. If the parent changes, the agent is gone and the
	// handler must not outlive it.
	Parent func() int
}

// Supervise parses the `hanse-node supervise` command line and runs one
// privileged handler as root.
func Supervise(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("supervise", flag.ContinueOnError)
	fs.SetOutput(stderr)
	spec := SuperviseSpec{Stdin: stdin, Stdout: stdout, Stderr: stderr}
	var grace string
	fs.StringVar(&spec.Dir, "dir", "", "the handler's working directory")
	fs.StringVar(&grace, "grace", "", "the time between SIGTERM and SIGKILL")
	fs.StringVar(&spec.CancelFile, "cancel-file", "", "the file that requests a stop")
	fs.StringVar(&spec.ControlFile, "control", "", "the file that holds the handler's environment")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if len(fs.Args()) == 0 {
		superviseError(stderr, "no handler entry command")
		return 2
	}
	if grace != "" {
		parsed, err := time.ParseDuration(grace)
		if err != nil {
			superviseError(stderr, "the grace period is invalid: %v", err)
			return 2
		}
		spec.Grace = parsed
	}
	spec.Entry = fs.Args()
	return SuperviseHandler(spec)
}

// SuperviseHandler runs the handler and returns the exit code the agent
// should see. It starts the handler in its own process group, passes the
// request and the output streams through unchanged, and stops the handler
// when the agent asks or when the agent exits.
func SuperviseHandler(spec SuperviseSpec) int {
	if len(spec.Entry) == 0 {
		superviseError(spec.Stderr, "no handler entry command")
		return 2
	}
	control, err := readSupervisorControl(spec.ControlFile)
	if err != nil {
		superviseError(spec.Stderr, "%v", err)
		return supervisorStartFailed
	}
	dir := spec.Dir
	if dir == "" {
		dir = control.Dir
	}
	grace := spec.Grace
	if grace <= 0 {
		grace = 10 * time.Second
	}
	poll := spec.Poll
	if poll <= 0 {
		poll = DefaultSupervisorPoll
	}
	parent := os.Getppid
	if spec.Parent != nil {
		parent = spec.Parent
	}
	agent := parent()

	cmd := exec.Command(spec.Entry[0], spec.Entry[1:]...)
	cmd.Dir = dir
	cmd.Env = control.Env
	cmd.Stdin = spec.Stdin
	cmd.Stdout = spec.Stdout
	cmd.Stderr = spec.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(signals)

	if err := cmd.Start(); err != nil {
		superviseError(spec.Stderr, "the handler cannot start: %v", err)
		return supervisorStartFailed
	}
	wait := make(chan struct{}, 1)
	go func() {
		_ = cmd.Wait()
		wait <- struct{}{}
	}()

	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	for {
		select {
		case <-wait:
			return superviseExitCode(cmd.ProcessState)
		case <-signals:
			return superviseStop(cmd, wait, grace)
		case <-ticker.C:
			if supervisorStopRequested(spec.CancelFile, agent, parent) {
				return superviseStop(cmd, wait, grace)
			}
		}
	}
}

// superviseError writes one supervisor diagnostic to stderr. The writers
// are the process's stderr or a test buffer, so a failed write changes
// nothing.
func superviseError(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, "hanse-node supervise: "+format+"\n", args...)
}

// supervisorStopRequested reports whether the agent asked for a stop or
// died. A changed parent means the agent is gone, so the handler must not
// outlive it.
func supervisorStopRequested(cancelFile string, agent int, parent func() int) bool {
	if parent() != agent {
		return true
	}
	if cancelFile == "" {
		return false
	}
	return fileExists(cancelFile)
}

// superviseStop sends SIGTERM to the handler's process group, waits the
// grace period, then sends SIGKILL.
func superviseStop(cmd *exec.Cmd, wait <-chan struct{}, grace time.Duration) int {
	signalGroup(cmd, syscall.SIGTERM)
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-wait:
	case <-timer.C:
		signalGroup(cmd, syscall.SIGKILL)
		<-wait
	}
	return superviseExitCode(cmd.ProcessState)
}

// signalGroup sends one signal to the handler and its children.
func signalGroup(cmd *exec.Cmd, sig syscall.Signal) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, sig)
}

// superviseExitCode mirrors the handler's exit status: its exit code, or
// 128 plus the signal that ended it.
func superviseExitCode(state *os.ProcessState) int {
	if state == nil {
		return supervisorStartFailed
	}
	if status, ok := state.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		return 128 + int(status.Signal())
	}
	return state.ExitCode()
}

// readSupervisorControl reads the handler environment the runner wrote.
// The file holds no password: it never leaves sudo's stdin.
func readSupervisorControl(path string) (runner.SupervisorControl, error) {
	var control runner.SupervisorControl
	if path == "" {
		return control, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return control, fmt.Errorf("the control file %s cannot be read: %w", path, err)
	}
	if err := json.Unmarshal(data, &control); err != nil {
		return control, fmt.Errorf("the control file %s is not valid: %w", path, err)
	}
	return control, nil
}

// fileExists reports whether path exists.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
