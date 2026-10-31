package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync"
	"testing"
	"time"

	herrors "github.com/achmadss/hanse/internal/errors"
	"github.com/achmadss/hanse/internal/runtime"
)

// The helper process environment variable names.
const (
	helperModeEnv   = "RUNNER_TEST_MODE"
	helperMarkerEnv = "RUNNER_TEST_MARKER"
)

// TestRunnerHelper is the test helper program. The runner re-executes the
// test binary with -test.run=TestRunnerHelper and one mode in the
// environment, so the tests run the same on every operating system.
func TestRunnerHelper(_ *testing.T) {
	mode := os.Getenv(helperModeEnv)
	if mode == "" {
		return
	}
	switch mode {
	case "ok":
		helperReadRequest()
		helperOK()
	case "error":
		helperReadRequest()
		helperWrite(`{"contract":"hanse.handler/v1","status":"error","error":{"code":"test_failure","message":"the test handler failed"}}`)
		os.Exit(1)
	case "malformed":
		helperReadRequest()
		helperWrite(`{"status":`)
		os.Exit(0)
	case "exit":
		helperReadRequest()
		os.Exit(3)
	case "ok_nonzero":
		helperReadRequest()
		helperWrite(`{"contract":"hanse.handler/v1","status":"ok","result":{"done":true}}`)
		os.Exit(2)
	case "cancelled":
		helperReadRequest()
		helperWrite(`{"contract":"hanse.handler/v1","status":"cancelled","result":{"partial":true}}`)
		os.Exit(0)
	case "cancel_file":
		helperReadRequest()
		// Ignore SIGTERM so the test proves that the cancel file alone
		// reaches a handler that does not watch signals.
		helperIgnoreTermination()
		helperWaitForCancelFile()
	case "sigterm":
		helperReadRequest()
		helperWaitForTerminationSignal()
	case "stubborn":
		helperReadRequest()
		helperIgnoreTermination()
		for {
			time.Sleep(10 * time.Millisecond)
		}
	case "hang":
		helperReadRequest()
		time.Sleep(10 * time.Second)
		os.Exit(0)
	case "live":
		helperReadRequest()
		fmt.Fprintln(os.Stderr, "first line")
		time.Sleep(250 * time.Millisecond)
		fmt.Fprintln(os.Stderr, "second line")
		helperOK()
	case "oversize":
		helperReadRequest()
		_, _ = os.Stdout.Write([]byte(strings.Repeat("x", 64<<10)))
		os.Exit(0)
	case "env":
		helperReadRequest()
		for _, name := range []string{
			"HANSE_CONTRACT", "HANSE_CALL_ID", "HANSE_WORK_DIR", "HANSE_CANCEL_FILE",
			"HANSE_PRIVILEGED", "PATH", "HOME", "USERPROFILE", "TMPDIR", "TEMP", "TMP",
			"LANG", "RUNNER_TEST_ALLOWED", "RUNNER_TEST_HIDDEN",
			// The Windows baseline variables of docs/handler-contract.md
			// section 4.2. They are empty on other systems.
			"PSModulePath", "windir", "SystemRoot", "SystemDrive", "ComSpec", "PATHEXT",
			"ProgramData", "ProgramFiles", "ProgramFiles(x86)", "CommonProgramFiles",
			"APPDATA", "LOCALAPPDATA", "NUMBER_OF_PROCESSORS", "PROCESSOR_ARCHITECTURE", "OS",
		} {
			fmt.Fprintf(os.Stderr, "ENV %s=%s\n", name, os.Getenv(name))
		}
		helperOK()
	case "workdir":
		helperReadRequest()
		workDir := os.Getenv("HANSE_WORK_DIR")
		cancelFile := os.Getenv("HANSE_CANCEL_FILE")
		fmt.Fprintf(os.Stderr, "WORKDIR=%s\n", workDir)
		if info, err := os.Stat(workDir); err == nil && info.IsDir() {
			fmt.Fprintln(os.Stderr, "WORKDIR_EXISTS=true")
		}
		if _, err := os.Stat(cancelFile); os.IsNotExist(err) {
			fmt.Fprintln(os.Stderr, "CANCEL_FILE_ABSENT=true")
		}
		_ = os.WriteFile(filepath.Join(workDir, "scratch"), []byte("scratch"), 0o600)
		helperOK()
	case "tree":
		helperReadRequest()
		helperSpawnGrandchild()
		for {
			time.Sleep(time.Second)
		}
	case "rights":
		helperReadRequest()
		helperReportRights()
		helperOK()
	case "grandchild":
		time.Sleep(800 * time.Millisecond)
		marker := os.Getenv(helperMarkerEnv)
		if marker != "" {
			_ = os.WriteFile(marker, []byte("alive"), 0o600)
		}
		os.Exit(0)
	default:
		fmt.Fprintf(os.Stderr, "unknown helper mode %q\n", mode)
		os.Exit(9)
	}
}

// helperReadRequest consumes the request envelope, as every handler does.
func helperReadRequest() {
	_, _ = io.Copy(io.Discard, os.Stdin)
}

// helperOK writes a successful response.
func helperOK() {
	helperWrite(`{"contract":"hanse.handler/v1","status":"ok","result":{"done":true},"summary":"done"}`)
	os.Exit(0)
}

// helperWrite writes one response envelope to stdout, without buffering, so
// the runner sees it immediately.
func helperWrite(body string) {
	_, _ = fmt.Fprintln(os.Stdout, body)
}

// helperWaitForCancelFile polls the cancel file and answers with a
// cancelled response once the stop request arrives.
func helperWaitForCancelFile() {
	path := os.Getenv("HANSE_CANCEL_FILE")
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			fmt.Fprintln(os.Stderr, "cancel file seen")
			helperWrite(`{"contract":"hanse.handler/v1","status":"cancelled","result":{"partial":true}}`)
			os.Exit(0)
		}
		time.Sleep(5 * time.Millisecond)
	}
	os.Exit(4)
}

// helperSpawnGrandchild starts a child process that writes the marker file
// after a delay, so a test can prove that the whole process tree was
// killed.
func helperSpawnGrandchild() {
	marker := os.Getenv(helperMarkerEnv)
	self, err := os.Executable()
	if err != nil {
		self = os.Args[0]
	}
	cmd := exec.Command(self, "-test.run=TestRunnerHelper")
	cmd.Env = helperChildEnv(marker)
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "grandchild start failed: %v\n", err)
		os.Exit(5)
	}
}

// helperChildEnv returns the environment of the grandchild: the parent's
// environment with the grandchild mode and marker set.
func helperChildEnv(marker string) []string {
	env := make([]string, 0, len(os.Environ())+2)
	for _, item := range os.Environ() {
		name := item
		if i := strings.IndexByte(item, '='); i >= 0 {
			name = item[:i]
		}
		if name == helperModeEnv || name == helperMarkerEnv {
			continue
		}
		env = append(env, item)
	}
	env = append(env, helperModeEnv+"=grandchild", helperMarkerEnv+"="+marker)
	return env
}

// logCollector collects the runner's log lines safely.
type logCollector struct {
	mu    sync.Mutex
	lines []string
}

// log appends one line.
func (c *logCollector) log(line string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lines = append(c.lines, line)
}

// all returns the collected lines.
func (c *logCollector) all() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.lines...)
}

// contains reports whether one line contains text.
func (c *logCollector) contains(text string) bool {
	for _, line := range c.all() {
		if strings.Contains(line, text) {
			return true
		}
	}
	return false
}

// newTestRunner builds a runner with a short grace period and a temporary
// work root.
func newTestRunner(t *testing.T, allow ...string) *Runner {
	t.Helper()
	envAllow := append([]string{helperModeEnv, helperMarkerEnv}, allow...)
	return &Runner{
		EnvAllow:      envAllow,
		ResponseLimit: 1 << 20,
		GracePeriod:   100 * time.Millisecond,
		WorkRoot:      t.TempDir(),
	}
}

// helperSpec builds one call to the helper process.
func helperSpec(t *testing.T, _ string, log func(string)) Spec {
	t.Helper()
	self, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatalf("resolve the test binary: %v", err)
	}
	if log == nil {
		log = func(string) {}
	}
	return Spec{
		Request: &runtime.Request{
			Contract:   runtime.ContractV1,
			Kind:       runtime.KindOperation,
			ID:         "op_test",
			Capability: "test",
			Operation:  "run",
			Arguments:  json.RawMessage(`{}`),
			Nodes:      map[string]runtime.Node{},
			Executor:   runtime.Node{ID: "gateway", OS: goruntime.GOOS, Arch: goruntime.GOARCH},
			Secrets:    map[string]string{},
			Deadline:   time.Now().Add(5 * time.Second),
		},
		Entry: []string{self, "-test.run=TestRunnerHelper"},
		Dir:   t.TempDir(),
		Log:   log,
	}
}

// helperWantMode puts the helper mode in the child environment. The runner
// passes it through because newTestRunner allow-lists it.
func helperWantMode(t *testing.T, mode string) {
	t.Helper()
	t.Setenv(helperModeEnv, mode)
}

// TestRunSuccess covers a valid ok response and exit code 0.
func TestRunSuccess(t *testing.T) {
	helperWantMode(t, "ok")
	runner := newTestRunner(t)
	resp, err := runner.Run(context.Background(), helperSpec(t, "ok", nil))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if resp.Status != runtime.StatusOK {
		t.Fatalf("status = %q, want ok", resp.Status)
	}
	if string(resp.Result) != `{"done":true}` {
		t.Errorf("result = %s, want {\"done\":true}", resp.Result)
	}
	if resp.Summary != "done" {
		t.Errorf("summary = %q, want done", resp.Summary)
	}
}

// TestRunHandlerError covers a valid error response. The response is the
// handler's own error, not a runner error.
func TestRunHandlerError(t *testing.T) {
	helperWantMode(t, "error")
	runner := newTestRunner(t)
	resp, err := runner.Run(context.Background(), helperSpec(t, "error", nil))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if resp.Status != runtime.StatusError || resp.Err == nil {
		t.Fatalf("response = %+v, want status error with an error object", resp)
	}
	if resp.Err.Code != "test_failure" {
		t.Errorf("handler code = %q, want test_failure", resp.Err.Code)
	}
}

// TestRunMalformedOutput covers a response that is not a JSON envelope.
func TestRunMalformedOutput(t *testing.T) {
	helperWantMode(t, "malformed")
	runner := newTestRunner(t)
	_, err := runner.Run(context.Background(), helperSpec(t, "malformed", nil))
	if !errors.Is(err, herrors.HandlerOutputInvalid) {
		t.Fatalf("error = %v, want handler_output_invalid", err)
	}
}

// TestRunNonzeroWithoutResponse covers a nonzero exit with no response.
func TestRunNonzeroWithoutResponse(t *testing.T) {
	helperWantMode(t, "exit")
	runner := newTestRunner(t)
	_, err := runner.Run(context.Background(), helperSpec(t, "exit", nil))
	if !errors.Is(err, herrors.HandlerExit) {
		t.Fatalf("error = %v, want handler_exit", err)
	}
	var mapped *herrors.Error
	if !errors.As(err, &mapped) {
		t.Fatalf("error = %v, want a herrors.Error", err)
	}
	if mapped.Details["exit_code"] != 3 {
		t.Errorf("exit_code detail = %v, want 3", mapped.Details["exit_code"])
	}
}

// TestRunOKWithNonzeroExit covers an ok response with a nonzero exit code.
func TestRunOKWithNonzeroExit(t *testing.T) {
	helperWantMode(t, "ok_nonzero")
	runner := newTestRunner(t)
	_, err := runner.Run(context.Background(), helperSpec(t, "ok_nonzero", nil))
	if !errors.Is(err, herrors.HandlerExit) {
		t.Fatalf("error = %v, want handler_exit", err)
	}
}

// TestRunStartFailure covers an entry command that cannot start.
func TestRunStartFailure(t *testing.T) {
	runner := newTestRunner(t)
	spec := helperSpec(t, "ok", nil)
	spec.Entry = []string{filepath.Join(t.TempDir(), "missing-handler")}
	_, err := runner.Run(context.Background(), spec)
	if !errors.Is(err, herrors.HandlerStartFailed) {
		t.Fatalf("error = %v, want handler_start_failed", err)
	}
}

// TestRunOversizedOutput covers stdout over the response limit. The runner
// kills the handler and reports handler_output_too_large.
func TestRunOversizedOutput(t *testing.T) {
	helperWantMode(t, "oversize")
	runner := newTestRunner(t)
	runner.ResponseLimit = 1024
	_, err := runner.Run(context.Background(), helperSpec(t, "oversize", nil))
	if !errors.Is(err, herrors.HandlerOutputTooLarge) {
		t.Fatalf("error = %v, want handler_output_too_large", err)
	}
}

// TestRunTimeout covers a handler that outlives the call's deadline.
func TestRunTimeout(t *testing.T) {
	helperWantMode(t, "hang")
	runner := newTestRunner(t)
	spec := helperSpec(t, "hang", nil)
	spec.Request.Deadline = time.Now().Add(150 * time.Millisecond)
	start := time.Now()
	_, err := runner.Run(context.Background(), spec)
	if !errors.Is(err, herrors.Timeout) {
		t.Fatalf("error = %v, want timeout", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("timeout took %v, want a short stop", elapsed)
	}
}

// TestRunTimeoutKillsProcessTree covers a timeout that must kill a
// grandchild too. The grandchild writes the marker only if it survives.
func TestRunTimeoutKillsProcessTree(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "grandchild-alive")
	t.Setenv(helperModeEnv, "tree")
	t.Setenv(helperMarkerEnv, marker)
	runner := newTestRunner(t)
	spec := helperSpec(t, "tree", nil)
	spec.Request.Deadline = time.Now().Add(200 * time.Millisecond)
	if _, err := runner.Run(context.Background(), spec); !errors.Is(err, herrors.Timeout) {
		t.Fatalf("error = %v, want timeout", err)
	}
	time.Sleep(1200 * time.Millisecond)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("the grandchild survived the process tree kill: stat %s = %v", marker, err)
	}
}

// TestRunCancelFile covers cancellation through the cancel file: the
// handler sees the file and answers with a cancelled response.
func TestRunCancelFile(t *testing.T) {
	helperWantMode(t, "cancel_file")
	runner := newTestRunner(t)
	runner.GracePeriod = 2 * time.Second
	logs := &logCollector{}
	spec := helperSpec(t, "cancel_file", logs.log)
	spec.Request.Deadline = time.Now().Add(5 * time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	resp, err := runner.Run(ctx, spec)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if resp.Status != runtime.StatusCancelled {
		t.Fatalf("status = %q, want cancelled", resp.Status)
	}
	if !logs.contains("cancel file seen") {
		t.Errorf("log lines %v do not show the cancel file", logs.all())
	}
}

// TestRunCancelledWithoutRequest covers a cancelled response that nobody
// asked for.
func TestRunCancelledWithoutRequest(t *testing.T) {
	helperWantMode(t, "cancelled")
	runner := newTestRunner(t)
	_, err := runner.Run(context.Background(), helperSpec(t, "cancelled", nil))
	if !errors.Is(err, herrors.HandlerOutputInvalid) {
		t.Fatalf("error = %v, want handler_output_invalid", err)
	}
}

// TestRunTerminationSignal covers the POSIX stop request: the cancel file
// exists before SIGTERM reaches the handler.
func TestRunTerminationSignal(t *testing.T) {
	if !helperSupportsTerminationSignal() {
		t.Skip("the platform has no SIGTERM for a process without a console")
	}
	helperWantMode(t, "sigterm")
	runner := newTestRunner(t)
	runner.GracePeriod = 2 * time.Second
	logs := &logCollector{}
	spec := helperSpec(t, "sigterm", logs.log)
	spec.Request.Deadline = time.Now().Add(5 * time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	resp, err := runner.Run(ctx, spec)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if resp.Status != runtime.StatusCancelled {
		t.Fatalf("status = %q, want cancelled", resp.Status)
	}
	if !logs.contains("sigterm seen") {
		t.Errorf("log lines %v do not show SIGTERM", logs.all())
	}
	if !logs.contains("cancel file seen before sigterm") {
		t.Errorf("the cancel file was not there before SIGTERM: %v", logs.all())
	}
}

// TestRunGracePeriodKill covers a handler that ignores the stop request:
// the runner kills the process group after the grace period.
func TestRunGracePeriodKill(t *testing.T) {
	helperWantMode(t, "stubborn")
	runner := newTestRunner(t)
	runner.GracePeriod = 150 * time.Millisecond
	spec := helperSpec(t, "stubborn", nil)
	spec.Request.Deadline = time.Now().Add(5 * time.Second)
	outcome := &Outcome{}
	spec.Outcome = outcome
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	_, err := runner.Run(ctx, spec)
	if !errors.Is(err, herrors.Cancelled) {
		t.Fatalf("error = %v, want cancelled", err)
	}
	if elapsed := time.Since(start); elapsed < 150*time.Millisecond {
		t.Errorf("run returned after %v, before the grace period ended", elapsed)
	}
	if !outcome.Killed {
		t.Error("the outcome does not report the kill after the grace period")
	}
}

// TestRunStderrLinesWhileRunning covers live stderr: the first line reaches
// the callback while the handler still runs.
func TestRunStderrLinesWhileRunning(t *testing.T) {
	helperWantMode(t, "live")
	runner := newTestRunner(t)
	lines := make(chan string, 16)
	spec := helperSpec(t, "live", func(line string) { lines <- line })
	done := make(chan error, 1)
	var resp runtime.Response
	go func() {
		var err error
		resp, err = runner.Run(context.Background(), spec)
		done <- err
	}()
	select {
	case line := <-lines:
		if line != "first line" {
			t.Errorf("first line = %q, want first line", line)
		}
	case err := <-done:
		t.Fatalf("run finished before the first line: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("the first stderr line did not arrive while the handler ran")
	}
	if err := <-done; err != nil {
		t.Fatalf("run: %v", err)
	}
	if resp.Status != runtime.StatusOK {
		t.Errorf("status = %q, want ok", resp.Status)
	}
}

// TestRunEnvironmentAllowList covers the environment of section 4.2: the
// HANSE_* variables and the allow-listed variables only.
func TestRunEnvironmentAllowList(t *testing.T) {
	helperWantMode(t, "env")
	t.Setenv("RUNNER_TEST_ALLOWED", "visible")
	t.Setenv("RUNNER_TEST_HIDDEN", "hidden-value")
	runner := newTestRunner(t, "RUNNER_TEST_ALLOWED")
	logs := &logCollector{}
	if _, err := runner.Run(context.Background(), helperSpec(t, "env", logs.log)); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !logs.contains("ENV HANSE_CONTRACT=hanse.handler/v1") {
		t.Errorf("missing HANSE_CONTRACT in %v", logs.all())
	}
	if !logs.contains("ENV HANSE_CALL_ID=op_test") {
		t.Errorf("missing HANSE_CALL_ID in %v", logs.all())
	}
	if !logs.contains("ENV HANSE_PRIVILEGED=0") {
		t.Errorf("missing HANSE_PRIVILEGED in %v", logs.all())
	}
	if !logs.contains("ENV RUNNER_TEST_ALLOWED=visible") {
		t.Errorf("the allow-listed variable is missing from %v", logs.all())
	}
	for _, line := range logs.all() {
		if strings.Contains(line, "hidden-value") {
			t.Errorf("a variable that is not allow-listed reached the handler: %s", line)
		}
	}
}

// TestRunWorkDir covers HANSE_WORK_DIR: it exists during the call and the
// runner removes it afterwards.
func TestRunWorkDir(t *testing.T) {
	helperWantMode(t, "workdir")
	runner := newTestRunner(t)
	logs := &logCollector{}
	if _, err := runner.Run(context.Background(), helperSpec(t, "workdir", logs.log)); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !logs.contains("WORKDIR_EXISTS=true") {
		t.Errorf("HANSE_WORK_DIR was not a directory: %v", logs.all())
	}
	if !logs.contains("CANCEL_FILE_ABSENT=true") {
		t.Errorf("the cancel file existed before the call ended: %v", logs.all())
	}
	workDir := ""
	for _, line := range logs.all() {
		if strings.HasPrefix(line, "WORKDIR=") {
			workDir = strings.TrimPrefix(line, "WORKDIR=")
		}
	}
	if workDir == "" {
		t.Fatal("the handler did not report its work directory")
	}
	if _, err := os.Stat(workDir); !os.IsNotExist(err) {
		t.Errorf("the work directory survived the call: stat %s = %v", workDir, err)
	}
}
