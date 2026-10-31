package runner

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"sort"
	"strings"
	"time"

	"github.com/achmadss/hanse/internal/config"
	herrors "github.com/achmadss/hanse/internal/errors"
	"github.com/achmadss/hanse/internal/runtime"
)

// DefaultResponseLimit is the documented default size of one response on
// stdout, 1 MiB, docs/handler-contract.md section 11.
const DefaultResponseLimit int64 = 1 << 20

// Runner runs exec handlers. The zero value uses the documented defaults.
// A Runner holds no per-call state, so one value serves concurrent calls.
type Runner struct {
	// EnvAllow lists extra environment variables passed from the agent's
	// environment to the handler, gateway.yaml's handlers.env_allow.
	EnvAllow []string
	// ResponseLimit is the most bytes of stdout the runner reads; zero or
	// less means DefaultResponseLimit.
	ResponseLimit int64
	// GracePeriod is the time between a stop request and the kill; zero or
	// less means the documented default of 10 seconds.
	GracePeriod time.Duration
	// WorkRoot is where HANSE_WORK_DIR is created; empty means the
	// operating system's temporary directory.
	WorkRoot string
	// Supervisor is the command prefix that starts the root supervisor
	// mode of the node agent on POSIX systems. Empty means this
	// executable with the `supervise` subcommand. Windows ignores it.
	Supervisor []string
}

// Spec is one exec handler call: the request envelope, the entry command,
// the capability directory it runs from, and the log callback.
type Spec struct {
	// Request is the call's envelope. The runner does not modify it.
	Request *runtime.Request
	// Raw is the request envelope body to write to stdin, when the caller
	// already holds the Gateway's bytes. Empty means the runner encodes
	// Request; a non-empty Raw is written unchanged
	// (docs/agent-protocol.md section 4.3).
	Raw []byte
	// Entry is the manifest entry command for the executor's OS. It is
	// an argument list and is never passed through a shell.
	Entry []string
	// Dir is the capability directory: the handler's working directory.
	Dir string
	// Log receives every stderr line as the line ends. A nil Log discards
	// the lines.
	Log func(line string)
	// Started, when not nil, runs once the handler process exists and its
	// stdin has a writer, before the runner waits for it to exit.
	Started func()
	// Privileged runs the handler with administrator rights
	// (docs/architecture.md section 8.5). On POSIX systems the runner
	// starts it through sudo and the agent's root supervisor; on Windows
	// it keeps the agent service's full rights. The handler receives
	// HANSE_PRIVILEGED=1.
	Privileged bool
	// Password is the elevation password of a privileged run. It is
	// written to sudo's stdin and never to argv, the environment, a log,
	// or a record. The caller should clear its own copy.
	Password string
	// Outcome, when not nil, receives how the process ended: its raw
	// stdout, exit status, duration, and stop reason. It exists for a
	// caller that must report the process status itself, such as the node
	// agent's result message.
	Outcome *Outcome
}

// Outcome is how one handler process ended. Every field except Started is
// zero when the process never started.
type Outcome struct {
	// Started reports that the process existed.
	Started bool
	// Stdout is the process's stdout, up to the response limit.
	Stdout []byte
	// ExitCode is the process's exit code, or nil when a signal ended it.
	ExitCode *int
	// Signal is the signal name that ended the process, empty when the
	// process exited by itself or on Windows.
	Signal string
	// Duration is the time from process start to process end.
	Duration time.Duration
	// StopReason is "", "cancel", "deadline", or "output_too_large".
	StopReason string
	// OutputError is "", "missing", "invalid", or "too_large".
	OutputError string
}

// stopReason is why the runner stopped a handler.
type stopReason int

const (
	stopNone stopReason = iota
	// stopTimeout is a stop that the call's deadline caused.
	stopTimeout
	// stopCancelled is a stop that the caller's context caused.
	stopCancelled
	// stopOversized is a kill that the stdout size limit caused.
	stopOversized
)

// waitResult is what cmd.Wait reported.
type waitResult struct {
	state *os.ProcessState
	err   error
}

// stdoutResult is what the stdout reader read and whether it exceeded the
// limit.
type stdoutResult struct {
	data      []byte
	oversized bool
	err       error
}

// Run performs one exec handler call, docs/handler-contract.md section 4.
//
// A returned response with status error is the handler's own error. A Go
// error means the call produced no protocol response: a start failure, a
// timeout, a cancellation, an oversized response, a nonzero exit without a
// valid response, or a malformed response.
func (r *Runner) Run(ctx context.Context, spec Spec) (runtime.Response, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if spec.Request == nil {
		return runtime.Response{}, herrors.New(herrors.Internal,
			"the exec runner received no request envelope", nil)
	}
	if len(spec.Entry) == 0 {
		return runtime.Response{}, herrors.New(herrors.HandlerStartFailed,
			"the handler has no entry command", nil)
	}
	limit := DefaultResponseLimit
	if r != nil && r.ResponseLimit > 0 {
		limit = r.ResponseLimit
	}
	grace := config.DefaultTimeouts().GracePeriod
	workRoot := ""
	if r != nil {
		if r.GracePeriod > 0 {
			grace = r.GracePeriod
		}
		workRoot = r.WorkRoot
	}
	envAllow := []string(nil)
	if r != nil {
		envAllow = r.EnvAllow
	}
	payload := spec.Raw
	if len(payload) == 0 {
		encoded, err := encodeRequest(spec.Request)
		if err != nil {
			return runtime.Response{}, err
		}
		payload = encoded
	}
	workDir, err := os.MkdirTemp(workRoot, "hanse-call-")
	if err != nil {
		return runtime.Response{}, herrors.New(herrors.Internal,
			fmt.Sprintf("the handler work directory cannot be created: %s", err), nil)
	}
	defer func() { _ = os.RemoveAll(workDir) }()
	cancelFile := filepath.Join(workDir, "cancel")

	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		return runtime.Response{}, pipeError(err)
	}
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		closeFiles(stdinR, stdinW)
		return runtime.Response{}, pipeError(err)
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		closeFiles(stdinR, stdinW, stdoutR, stdoutW)
		return runtime.Response{}, pipeError(err)
	}

	argv := resolveEntry(spec.Dir, spec.Entry)
	plan := &launchPlan{
		Entry:      argv,
		Dir:        spec.Dir,
		Env:        buildEnv(spec.Request, workDir, cancelFile, envAllow, spec.Privileged),
		WorkDir:    workDir,
		CancelFile: cancelFile,
		Grace:      grace,
		Password:   spec.Password,
	}
	if spec.Privileged {
		if err := preparePrivileged(r, plan); err != nil {
			closeFiles(stdinR, stdinW, stdoutR, stdoutW, stderrR, stderrW)
			return runtime.Response{}, err
		}
	}
	// The password now lives only in the prefix buffer, which is zeroed as
	// soon as sudo has read it.
	spec.Password = ""
	plan.Password = ""
	cmd := exec.Command(plan.Entry[0], plan.Entry[1:]...)
	cmd.Dir = plan.Dir
	cmd.Env = plan.Env
	cmd.Stdin = stdinR
	cmd.Stdout = stdoutW
	cmd.Stderr = stderrW
	opts, err := configureProcess(cmd, spec.Privileged)
	if err != nil {
		closeFiles(stdinR, stdinW, stdoutR, stdoutW, stderrR, stderrW)
		return runtime.Response{}, herrors.New(herrors.HandlerStartFailed,
			fmt.Sprintf("the handler process cannot be configured: %s", err), nil)
	}
	if opts != nil {
		defer opts.close()
	}

	started := time.Now()

	tree, err := newProcessTree()
	if err != nil {
		closeFiles(stdinR, stdinW, stdoutR, stdoutW, stderrR, stderrW)
		return runtime.Response{}, herrors.New(herrors.HandlerStartFailed,
			fmt.Sprintf("the handler process tree cannot be created: %s", err), nil)
	}
	defer tree.close()

	if err := cmd.Start(); err != nil {
		closeFiles(stdinR, stdinW, stdoutR, stdoutW, stderrR, stderrW)
		return runtime.Response{}, startFailed(err)
	}
	// The child owns its ends now; the parent keeps the read end of stdout
	// and stderr and the write end of stdin.
	closeFiles(stdinR, stdoutW, stderrW)
	if err := tree.attach(cmd.Process.Pid); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		closeFiles(stdinW, stdoutR, stderrR)
		return runtime.Response{}, herrors.New(herrors.HandlerStartFailed,
			fmt.Sprintf("the handler process tree cannot be attached: %s", err), nil)
	}

	go writeRequest(stdinW, plan.StdinPrefix, payload)
	if spec.Started != nil {
		spec.Started()
	}

	stdoutDone := make(chan stdoutResult, 1)
	oversized := make(chan struct{}, 1)
	go readStdout(stdoutR, limit, stdoutDone, oversized)
	stderrDone := make(chan struct{})
	go readStderr(stderrR, spec.Log, stderrDone)

	exited := make(chan waitResult, 1)
	go func() {
		waitErr := cmd.Wait()
		exited <- waitResult{state: cmd.ProcessState, err: waitErr}
	}()

	reason, waited := waitForExit(ctx, spec.Request.Deadline, grace, tree, exited, cancelFile, oversized)
	// In every stop path the process is gone, so the readers see EOF. The
	// oversized reader has already returned.
	out := <-stdoutDone
	<-stderrDone

	exitCode, signalName := exitStatus(waited.state)
	overLimit := out.oversized
	if reason == stopNone && overLimit {
		reason = stopOversized
	}
	resp, valid := runtime.Response{}, false
	if !overLimit {
		resp, err = decodeResponse(out.data)
		valid = err == nil
	}
	if valid && spec.Request.Kind == runtime.KindOptions && resp.Status == runtime.StatusOK {
		if problem := optionsProblem(resp.Result); problem != "" {
			valid = false
			err = invalidOutput("the handler returned an invalid options result: " + problem)
		}
	}
	if spec.Outcome != nil {
		*spec.Outcome = outcomeDetails(reason, started, out, exitCode, signalName, valid)
	}
	return outcome(reason, resp, valid, exitCode, signalName, limit, out.err)
}

// outcomeDetails describes one finished process for a caller that reports
// the process status itself, docs/agent-protocol.md section 4.7.
func outcomeDetails(reason stopReason, started time.Time, out stdoutResult, exitCode int, signal string, valid bool) Outcome {
	details := Outcome{
		Started:  true,
		Stdout:   out.data,
		Signal:   signal,
		Duration: time.Since(started),
	}
	if signal == "" && exitCode >= 0 {
		code := exitCode
		details.ExitCode = &code
	}
	switch reason {
	case stopTimeout:
		details.StopReason = "deadline"
	case stopCancelled:
		details.StopReason = "cancel"
	case stopOversized:
		details.StopReason = "output_too_large"
	}
	switch {
	case out.oversized:
		details.OutputError = "too_large"
	case !valid && len(bytes.TrimSpace(out.data)) == 0:
		details.OutputError = "missing"
	case !valid:
		details.OutputError = "invalid"
	}
	return details
}

// waitForExit waits for the process to exit or for a stop condition. It
// returns stopNone when the process exited on its own; otherwise it carries
// out the stop steps of docs/handler-contract.md sections 6 and 7 and
// returns the process's wait result.
func waitForExit(ctx context.Context, deadline time.Time, grace time.Duration, tree *processTree, exited <-chan waitResult, cancelFile string, oversized <-chan struct{}) (stopReason, waitResult) {
	var deadlineCh <-chan time.Time
	var timer *time.Timer
	if !deadline.IsZero() {
		timer = time.NewTimer(time.Until(deadline))
		defer timer.Stop()
		deadlineCh = timer.C
	}
	for {
		select {
		case waited := <-exited:
			return stopNone, waited
		case <-deadlineCh:
			return stopTimeout, stopHandler(tree, grace, exited, cancelFile)
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) || deadlinePassed(deadline) {
				return stopTimeout, stopHandler(tree, grace, exited, cancelFile)
			}
			return stopCancelled, stopHandler(tree, grace, exited, cancelFile)
		case <-oversized:
			tree.kill()
			return stopOversized, <-exited
		}
	}
}

// stopHandler carries out the stop steps: create the cancel file, send the
// platform's stop signal, wait the grace period, then kill the process
// tree. It also kills leftover children when the handler itself exited
// first.
func stopHandler(tree *processTree, grace time.Duration, exited <-chan waitResult, cancelFile string) waitResult {
	_ = writeCancelFile(cancelFile)
	tree.terminate()
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case waited := <-exited:
		tree.kill()
		return waited
	case <-timer.C:
		tree.kill()
		return <-exited
	}
}

// deadlinePassed reports whether the call's deadline has passed.
func deadlinePassed(deadline time.Time) bool {
	return !deadline.IsZero() && !time.Now().Before(deadline)
}

// outcome applies the final-state table of docs/handler-contract.md
// section 5.
func outcome(reason stopReason, resp runtime.Response, valid bool, exitCode int, signal string, limit int64, readErr error) (runtime.Response, error) {
	switch reason {
	case stopOversized:
		return runtime.Response{}, herrors.New(herrors.HandlerOutputTooLarge,
			fmt.Sprintf("the handler response exceeded the %d byte limit", limit),
			map[string]any{"limit": limit})
	case stopTimeout:
		// An ok response with exit code 0 that arrived before the stop is
		// a completed run, section 5's first two rows.
		if valid && resp.Status == runtime.StatusOK && exitCode == 0 && signal == "" {
			return resp, nil
		}
		return runtime.Response{}, herrors.New(herrors.Timeout,
			"the handler did not finish before its deadline", nil)
	case stopCancelled:
		if valid {
			switch resp.Status {
			case runtime.StatusOK, runtime.StatusCancelled:
				return resp, nil
			case runtime.StatusError:
				return resp, nil
			}
		}
		return runtime.Response{}, herrors.New(herrors.Cancelled,
			"the handler stopped after a cancellation request", nil)
	}
	if !valid {
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return runtime.Response{}, herrors.New(herrors.HandlerOutputInvalid,
				fmt.Sprintf("the handler response cannot be read: %s", readErr), nil)
		}
		if exitCode != 0 || signal != "" {
			return runtime.Response{}, handlerExit(exitCode, signal)
		}
		return runtime.Response{}, herrors.New(herrors.HandlerOutputInvalid,
			"the handler exited without a valid response envelope", nil)
	}
	switch resp.Status {
	case runtime.StatusOK:
		if exitCode == 0 && signal == "" {
			return resp, nil
		}
		return runtime.Response{}, handlerExit(exitCode, signal)
	case runtime.StatusError:
		return resp, nil
	case runtime.StatusCancelled:
		// A cancelled response is valid only after a cancellation request;
		// outcome only reaches here when no stop happened.
		return runtime.Response{}, herrors.New(herrors.HandlerOutputInvalid,
			"the handler cancelled a run that nobody asked to cancel", nil)
	default:
		return runtime.Response{}, herrors.New(herrors.HandlerOutputInvalid,
			fmt.Sprintf("the handler returned status %q", resp.Status), nil)
	}
}

// handlerExit is the handler_exit error with the exit code or signal in the
// details, docs/handler-contract.md section 5.
func handlerExit(code int, signal string) error {
	details := map[string]any{}
	if code >= 0 {
		details["exit_code"] = code
	}
	if signal != "" {
		details["signal"] = signal
	}
	if len(details) == 0 {
		details = nil
	}
	return herrors.New(herrors.HandlerExit,
		"the handler exited without a valid response envelope", details)
}

// startFailed maps a start failure to handler_start_failed.
func startFailed(err error) error {
	return herrors.New(herrors.HandlerStartFailed,
		fmt.Sprintf("the handler entry command could not start: %s", err), nil)
}

// pipeError maps a pipe creation failure to an internal error.
func pipeError(err error) error {
	return herrors.New(herrors.Internal,
		fmt.Sprintf("the handler pipes cannot be created: %s", err), nil)
}

// writeCancelFile creates the cancellation file. It does not exist before
// the stop request, docs/handler-contract.md section 7.1.
func writeCancelFile(path string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	return f.Close()
}

// writeRequest writes the password line of a privileged run and the
// request envelope, then closes stdin. sudo reads the password one byte at
// a time up to the newline, so the envelope that follows reaches the
// supervisor and then the handler unchanged. A handler that exits before
// reading it makes the write fail; that is the handler's business, not a
// runner error.
func writeRequest(w *os.File, prefix, payload []byte) {
	if len(prefix) > 0 {
		_, _ = w.Write(prefix)
		zeroBytes(prefix)
	}
	_, _ = w.Write(payload)
	_ = w.Close()
}

// zeroBytes clears a buffer that held the elevation password, so the value
// does not stay in the process's memory once sudo has read it.
func zeroBytes(data []byte) {
	for i := range data {
		data[i] = 0
	}
}

// readStdout reads at most limit+1 bytes and reports whether the handler
// exceeded the limit.
func readStdout(r *os.File, limit int64, out chan<- stdoutResult, oversized chan<- struct{}) {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	_ = r.Close()
	over := int64(len(data)) > limit
	if over {
		oversized <- struct{}{}
	}
	out <- stdoutResult{data: data, oversized: over, err: err}
}

// readStderr passes every line to log as soon as the line ends. Lines keep
// their order, blank lines are kept, a trailing \r is removed, and bytes
// that are not valid UTF-8 become U+FFFD, docs/handler-contract.md section
// 4.4.
func readStderr(r *os.File, log func(line string), done chan<- struct{}) {
	defer close(done)
	defer func() { _ = r.Close() }()
	if log == nil {
		log = func(string) {}
	}
	reader := bufio.NewReader(r)
	for {
		line, err := reader.ReadString('\n')
		if len(line) > 0 {
			line = strings.TrimSuffix(line, "\n")
			line = strings.TrimSuffix(line, "\r")
			log(strings.ToValidUTF8(line, "\uFFFD"))
		}
		if err != nil {
			return
		}
	}
}

// resolveEntry applies the contract's rule that relative paths in the entry
// command resolve from the capability directory, while a bare interpreter
// name such as sh still comes from PATH.
func resolveEntry(dir string, entry []string) []string {
	out := append([]string(nil), entry...)
	if dir == "" || len(out) == 0 {
		return out
	}
	first := out[0]
	if filepath.IsAbs(first) {
		return out
	}
	if strings.ContainsAny(first, `/\`) {
		out[0] = filepath.Join(dir, first)
		return out
	}
	if info, err := os.Stat(filepath.Join(dir, first)); err == nil && !info.IsDir() {
		out[0] = filepath.Join(dir, first)
	}
	return out
}

// buildEnv builds the handler environment of docs/handler-contract.md
// section 4.2: the HANSE_* variables, the documented per-OS variables, and
// only the allow-listed extra variables.
func buildEnv(req *runtime.Request, workDir, cancelFile string, allow []string, privileged bool) []string {
	type entry struct {
		name  string
		value string
	}
	values := map[string]entry{}
	key := func(name string) string {
		if goruntime.GOOS == "windows" {
			return strings.ToUpper(name)
		}
		return name
	}
	set := func(name, value string) {
		if name == "" {
			return
		}
		values[key(name)] = entry{name: name, value: value}
	}
	set("HANSE_CONTRACT", runtime.ContractV1)
	set("HANSE_CALL_ID", req.ID)
	set("HANSE_WORK_DIR", workDir)
	set("HANSE_CANCEL_FILE", cancelFile)
	if privileged {
		set("HANSE_PRIVILEGED", "1")
	} else {
		set("HANSE_PRIVILEGED", "0")
	}
	if path := os.Getenv("PATH"); path != "" {
		set("PATH", path)
	}
	if goruntime.GOOS == "windows" {
		set("USERPROFILE", os.Getenv("USERPROFILE"))
		set("TEMP", workDir)
		set("TMP", workDir)
		set("SystemRoot", os.Getenv("SystemRoot"))
	} else {
		set("HOME", os.Getenv("HOME"))
		set("TMPDIR", workDir)
		set("LANG", "C.UTF-8")
	}
	for _, name := range allow {
		if name == "" {
			continue
		}
		if value, ok := os.LookupEnv(name); ok {
			set(name, value)
		}
	}
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	env := make([]string, 0, len(names))
	for _, name := range names {
		item := values[name]
		env = append(env, item.name+"="+item.value)
	}
	return env
}

// closeFiles closes every file, ignoring errors: they are pipe ends whose
// read or write side already failed.
func closeFiles(files ...*os.File) {
	for _, f := range files {
		if f != nil {
			_ = f.Close()
		}
	}
}
