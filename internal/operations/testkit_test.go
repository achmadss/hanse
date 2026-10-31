package operations

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	"github.com/achmadss/hanse/internal/agent"
	"github.com/achmadss/hanse/internal/config"
	herrors "github.com/achmadss/hanse/internal/errors"
	"github.com/achmadss/hanse/internal/registry"
	"github.com/achmadss/hanse/internal/runtime"
)

// testkitHandlerSkip returns why the current OS cannot run the testkit
// handler, or "" when it can. The shell handler needs sh and jq; the Windows
// handler needs PowerShell. A test skips with the returned reason instead of
// failing when a program is missing, like the echo tests do.
func testkitHandlerSkip() string {
	if goruntime.GOOS == "windows" {
		if _, err := exec.LookPath("powershell"); err != nil {
			return "powershell is not installed: the testkit Windows handler needs it"
		}
		return ""
	}
	if _, err := exec.LookPath("sh"); err != nil {
		return "sh is not installed: the testkit shell handler needs it"
	}
	if _, err := exec.LookPath("jq"); err != nil {
		return "jq is not installed: the testkit shell handler needs it"
	}
	return ""
}

// testkitRegistry loads the repository's capabilities with a Gateway host
// node and a second executor node.
func testkitRegistry(t *testing.T) *registry.Registry {
	t.Helper()
	reg, _, err := registry.Load(registry.Options{
		Dirs: []string{filepath.Join("..", "..", "capabilities")},
		Input: registry.Input{
			Nodes: []registry.Node{
				{
					ID:      "gateway",
					Name:    "Gateway host",
					OS:      goruntime.GOOS,
					Arch:    goruntime.GOARCH,
					Enabled: true,
				},
				{
					ID:      "node-b",
					Name:    "Second node",
					OS:      goruntime.GOOS,
					Arch:    goruntime.GOARCH,
					Enabled: true,
				},
			},
			GatewayHost: "gateway",
			Routes: registry.Routes{Capabilities: map[string]registry.CapabilityRoute{
				"testkit": {Wildcard: true, DefaultExecutor: "gateway"},
			}},
		},
	})
	if err != nil {
		t.Fatalf("load capabilities/testkit: %v", err)
	}
	return reg
}

// testkitEnv is one manager over the embedded agent and the testkit
// capability.
type testkitEnv struct {
	registry   *registry.Registry
	dispatcher *ExecDispatcher
	manager    *Manager
}

// newTestkitEnv builds the embedded agent, the exec dispatcher, and the
// operations manager. A response limit or a log limit of zero takes the
// documented default.
func newTestkitEnv(t *testing.T, responseLimit int64, logLimit int) *testkitEnv {
	t.Helper()
	reg := testkitRegistry(t)
	host, err := agent.New(agent.Config{
		Node: runtime.Node{
			ID:   "gateway",
			Name: "Gateway host",
			OS:   goruntime.GOOS,
			Arch: goruntime.GOARCH,
		},
		Registry:      reg,
		ResponseLimit: responseLimit,
		GracePeriod:   time.Second,
		// A Windows runner can take several seconds to start PowerShell.
		OptionsTimeout: 30 * time.Second,
		WorkRoot:       t.TempDir(),
	})
	if err != nil {
		t.Fatalf("build the embedded agent: %v", err)
	}
	dispatcher, err := NewExecDispatcher(ExecConfig{Registry: reg, Agent: host})
	if err != nil {
		t.Fatalf("build the exec dispatcher: %v", err)
	}
	manager, err := New(Config{
		Registry:   reg,
		Dispatcher: dispatcher,
		Timeouts: config.Timeouts{
			Operation:   20 * time.Second,
			Options:     10 * time.Second,
			GracePeriod: time.Second,
		},
		LogLimit: logLimit,
	})
	if err != nil {
		t.Fatalf("build the manager: %v", err)
	}
	return &testkitEnv{registry: reg, dispatcher: dispatcher, manager: manager}
}

// submitTestkit submits one testkit operation with the given arguments JSON.
// A synchronous operation can wait on a handler that never answers, so the
// guard dumps the operation log with timestamps at the deadline: the last
// handler line names the step where the handler stopped.
func submitTestkit(t *testing.T, env *testkitEnv, operation, arguments string) Record {
	t.Helper()
	type result struct {
		rec Record
		err error
	}
	done := make(chan result, 1)
	go func() {
		rec, err := env.manager.Submit(Request{
			Capability: "testkit",
			Operation:  operation,
			Arguments:  json.RawMessage(arguments),
			Requester:  "test",
		})
		done <- result{rec: rec, err: err}
	}()
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("submit testkit.%s: %v", operation, got.err)
		}
		return got.rec
	case <-time.After(90 * time.Second):
		t.Fatalf("submit testkit.%s did not return within 90s:\n%s",
			operation, testkitActiveLogs(env))
		return Record{}
	}
}

// testkitActiveLogs dumps the logs of the active testkit operations, with
// timestamps, so a hung handler names its own step.
func testkitActiveLogs(env *testkitEnv) string {
	records, err := env.manager.List(Filter{Capability: "testkit", Active: true})
	if err != nil {
		return "read the active operations: " + err.Error()
	}
	var b strings.Builder
	for _, rec := range records {
		fmt.Fprintf(&b, "operation %s (%s) is %s\n", rec.ID, rec.Operation, rec.Status)
		entries, err := env.manager.Logs(rec.ID, 0)
		if err != nil {
			fmt.Fprintf(&b, "  read the log: %v\n", err)
			continue
		}
		for _, entry := range entries {
			fmt.Fprintf(&b, "  [%s %s] %s\n",
				entry.Time.Format("15:04:05.000"), entry.Stream, entry.Line)
		}
	}
	if b.Len() == 0 {
		return "no active testkit operation"
	}
	return b.String()
}

// waitTestkitDone waits for one asynchronous run to finish and returns its
// final record.
func waitTestkitDone(t *testing.T, env *testkitEnv, id string, timeout time.Duration) Record {
	t.Helper()
	select {
	case <-env.manager.Done(id):
	case <-time.After(timeout):
		t.Fatalf("operation %s did not finish within %s:\n%s", id, timeout, testkitActiveLogs(env))
	}
	rec, err := env.manager.Get(id)
	if err != nil {
		t.Fatalf("read operation %s: %v", id, err)
	}
	return rec
}

// waitTestkitState waits until one run reaches a state.
func waitTestkitState(t *testing.T, env *testkitEnv, id string, state State, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		rec, err := env.manager.Get(id)
		if err != nil {
			t.Fatalf("read operation %s: %v", id, err)
		}
		if rec.Status == state {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("operation %s did not reach state %q within %s", id, state, timeout)
}

// waitTestkitLog waits until one operation log holds text. A slow executor
// can take seconds to start its handler, so a test waits for the handler's
// own line instead of sleeping a fixed time.
func waitTestkitLog(t *testing.T, env *testkitEnv, id, text string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if logContains(t, env.manager, id, text) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("operation %s log does not hold %q within %s: %v",
		id, text, timeout, logLines(t, env.manager, id))
}

// warmTestkitHandler runs one handler through testkit.long, whose 10 minute
// timeout leaves room for a cold PowerShell start on a Windows CI runner. A
// synchronous operation must not spend its 30 second budget on that start, so
// the first test of the package warms the handler once.
//
// TODO(#85): drop the warm-up once the Windows baseline environment fixes the
// slow first-use module load under the restricted token.
func warmTestkitHandler(t *testing.T, env *testkitEnv) {
	t.Helper()
	rec := submitTestkit(t, env, "long", `{"node":"gateway","seconds":1}`)
	done := waitTestkitDone(t, env, rec.ID, 120*time.Second)
	if done.Status != StateCompleted {
		t.Fatalf("warm up the testkit handler: status = %q (failure %+v, logs %v)",
			done.Status, done.Failure, logLines(t, env.manager, done.ID))
	}
}

// TestTestkitManifest covers the manifest of every testkit operation: the
// mode, the timeout, the grace period, the concurrency limit, the privileged
// flag, the option provider, and the required node argument.
func TestTestkitManifest(t *testing.T) {
	reg := testkitRegistry(t)
	capability, ok := reg.Capability("testkit")
	if !ok {
		t.Fatal("capabilities/testkit is not loaded")
	}
	if capability.Manifest.Handler.Kind != registry.KindExec {
		t.Errorf("handler kind = %q, want exec", capability.Manifest.Handler.Kind)
	}
	wantEntry := map[string][]string{
		"linux":  {"sh", "handler.sh"},
		"darwin": {"sh", "handler.sh"},
		"windows": {
			"powershell", "-NoProfile", "-NonInteractive", "-InputFormat", "Text",
			"-ExecutionPolicy", "Bypass", "-File", "handler.ps1",
		},
	}
	for osName, want := range wantEntry {
		got := capability.Manifest.Handler.Entry[osName]
		if strings.Join(got, " ") != strings.Join(want, " ") {
			t.Errorf("handler.entry.%s = %v, want %v", osName, got, want)
		}
	}

	type wantSpec struct {
		mode       string
		timeout    time.Duration
		grace      time.Duration
		privileged bool
	}
	want := map[string]wantSpec{
		"say":        {mode: registry.ModeSync, timeout: 30 * time.Second},
		"long":       {mode: registry.ModeAsync, timeout: 10 * time.Minute},
		"background": {mode: registry.ModeAsync, timeout: 2 * time.Hour},
		"fail":       {mode: registry.ModeSync, timeout: 30 * time.Second},
		"hang":       {mode: registry.ModeAsync, timeout: 5 * time.Minute, grace: 2 * time.Second},
		"timeout":    {mode: registry.ModeAsync, timeout: 3 * time.Second, grace: time.Second},
		"flood":      {mode: registry.ModeAsync, timeout: 5 * time.Minute},
		"choose":     {mode: registry.ModeSync, timeout: 30 * time.Second},
		"privileged": {mode: registry.ModeSync, timeout: 30 * time.Second, privileged: true},
	}
	ids := capability.OperationIDs()
	if len(ids) != len(want) {
		t.Errorf("operations = %v, want %d entries", ids, len(want))
	}
	for opID, spec := range want {
		op, ok := capability.Operation(opID)
		if !ok {
			t.Errorf("operation %q is missing", opID)
			continue
		}
		if op.Mode != spec.mode {
			t.Errorf("%s.mode = %q, want %q", opID, op.Mode, spec.mode)
		}
		if op.Timeout == nil || *op.Timeout != spec.timeout {
			t.Errorf("%s.timeout = %v, want %v", opID, op.Timeout, spec.timeout)
		}
		if spec.grace == 0 {
			if op.GracePeriod != nil {
				t.Errorf("%s.grace_period = %v, want none", opID, *op.GracePeriod)
			}
		} else if op.GracePeriod == nil || *op.GracePeriod != spec.grace {
			t.Errorf("%s.grace_period = %v, want %v", opID, op.GracePeriod, spec.grace)
		}
		if op.Privileged != spec.privileged {
			t.Errorf("%s.privileged = %v, want %v", opID, op.Privileged, spec.privileged)
		}
	}

	// say takes the optional target node argument, and its result reports
	// the target and the executor. It has no concurrency limit.
	say, _ := capability.Operation("say")
	if say == nil || say.Schema == nil {
		t.Fatal("testkit.say is missing")
	}
	if len(say.Schema.Required) != 0 {
		t.Errorf("say.required = %v, want no required argument", say.Schema.Required)
	}
	target, ok := say.Schema.Properties["target"]
	if !ok {
		t.Fatal("say has no target argument")
	}
	if target.Format == nil || target.Format.Name != "hanse:node" {
		t.Errorf("say.target format = %v, want hanse:node", target.Format)
	}
	if _, ok := say.Schema.Properties["text"]; !ok {
		t.Errorf("say has no text argument")
	}
	if say.Concurrency != nil {
		t.Errorf("say.concurrency = %+v, want none", say.Concurrency)
	}

	// long keeps the node argument only as its concurrency lock key.
	long, _ := capability.Operation("long")
	if long == nil || long.Schema == nil {
		t.Fatal("testkit.long is missing")
	}
	nodeProperty, ok := long.Schema.Properties["node"]
	if !ok || !testkitContains(long.Schema.Required, "node") {
		t.Fatal("long does not require the node argument")
	}
	if nodeProperty.Format == nil || nodeProperty.Format.Name != "hanse:node" {
		t.Errorf("long.node format = %v, want hanse:node", nodeProperty.Format)
	}
	if long.Concurrency == nil || long.Concurrency.Limit != 1 ||
		len(long.Concurrency.Key) != 1 || long.Concurrency.Key[0] != "node" {
		t.Errorf("long.concurrency = %+v, want limit 1 keyed by node", long.Concurrency)
	}

	// Every other operation takes no node argument: the executor is chosen
	// with --executor.
	for _, opID := range ids {
		if opID == "long" {
			continue
		}
		op, ok := capability.Operation(opID)
		if !ok {
			continue
		}
		if _, ok := op.Schema.Properties["node"]; ok {
			t.Errorf("%s still takes a node argument", opID)
		}
	}

	choose, _ := capability.Operation("choose")
	if choose == nil || choose.Schema == nil {
		t.Fatal("testkit.choose is missing")
	}
	spec := capability.Manifest.Operations["choose"]
	if spec == nil || spec.Options["choice"] != "provider" {
		t.Errorf("choose.options = %+v, want choice: provider", spec)
	}
	if !testkitContains(choose.Schema.Required, "choice") {
		t.Errorf("choose does not require the choice argument")
	}
}

// testkitContains reports whether a string slice holds one value.
func testkitContains(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}

// TestTestkitSay covers the synchronous operation that returns its
// arguments, the executor, and the target node.
func TestTestkitSay(t *testing.T) {
	if reason := testkitHandlerSkip(); reason != "" {
		t.Skip(reason)
	}
	env := newTestkitEnv(t, 0, 0)
	warmTestkitHandler(t, env)
	rec := submitTestkit(t, env, "say", `{"target":"node-b","text":"hello"}`)
	if rec.Status != StateCompleted {
		t.Fatalf("testkit.say status = %q, want completed (failure %+v, logs %v)",
			rec.Status, rec.Failure, logLines(t, env.manager, rec.ID))
	}
	type sayResult struct {
		Target struct {
			ID   string `json:"id"`
			OS   string `json:"os"`
			Arch string `json:"arch"`
		} `json:"target"`
		Executor struct {
			ID string `json:"id"`
		} `json:"executor"`
		Arguments map[string]any `json:"arguments"`
	}
	var result sayResult
	if err := json.Unmarshal(rec.Result, &result); err != nil {
		t.Fatalf("decode testkit.say result %s: %v", rec.Result, err)
	}
	if result.Target.ID != "node-b" || result.Target.OS != goruntime.GOOS {
		t.Errorf("result target = %+v, want node-b on %s", result.Target, goruntime.GOOS)
	}
	if result.Executor.ID != "gateway" {
		t.Errorf("result executor = %q, want gateway", result.Executor.ID)
	}
	if result.Arguments["text"] != "hello" || result.Arguments["target"] != "node-b" {
		t.Errorf("result arguments = %v, want the submitted arguments", result.Arguments)
	}
	if !logContains(t, env.manager, rec.ID, "say: hello") {
		t.Errorf("testkit.say logs do not hold the handler's stderr line: %v",
			logLines(t, env.manager, rec.ID))
	}

	// The target is optional: without it, the result reports the executor
	// and a null target.
	plain := submitTestkit(t, env, "say", `{"text":"hello"}`)
	if plain.Status != StateCompleted {
		t.Fatalf("testkit.say without a target status = %q, want completed (failure %+v)",
			plain.Status, plain.Failure)
	}
	var plainResult struct {
		Target   *sayResult `json:"target"`
		Executor struct {
			ID string `json:"id"`
		} `json:"executor"`
	}
	if err := json.Unmarshal(plain.Result, &plainResult); err != nil {
		t.Fatalf("decode testkit.say result %s: %v", plain.Result, err)
	}
	if plainResult.Target != nil {
		t.Errorf("result target = %+v, want null without a target argument", plainResult.Target)
	}
	if plainResult.Executor.ID != "gateway" {
		t.Errorf("result executor = %q, want gateway", plainResult.Executor.ID)
	}
}

// TestTestkitMissingJq covers a host without jq: the shell handler writes a
// handler error and exits nonzero instead of failing with a shell error.
func TestTestkitMissingJq(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("the jq guard is part of the shell handler")
	}
	reg := testkitRegistry(t)
	capability, ok := reg.Capability("testkit")
	if !ok {
		t.Fatal("capabilities/testkit is not loaded")
	}
	operation, ok := capability.Operation("say")
	if !ok {
		t.Fatal("testkit.say is missing")
	}
	entry := operation.Entry[goruntime.GOOS]
	if len(entry) == 0 {
		t.Fatalf("testkit has no entry command for OS %q", goruntime.GOOS)
	}
	shPath, err := exec.LookPath(entry[0])
	if err != nil {
		t.Skipf("%s is not installed: %v", entry[0], err)
	}
	catPath, err := exec.LookPath("cat")
	if err != nil {
		t.Skipf("cat is not installed: %v", err)
	}
	// A PATH that holds cat and no jq: the handler reads stdin, then
	// reports the missing dependency itself.
	binDir := t.TempDir()
	if err := os.Symlink(catPath, filepath.Join(binDir, "cat")); err != nil {
		t.Fatalf("link cat: %v", err)
	}
	envelope := `{"contract":"hanse.handler/v1","kind":"operation","id":"op_test",` +
		`"capability":"testkit","operation":"say","arguments":{},"nodes":{},` +
		`"executor":{"id":"gateway","name":"Gateway host","os":"` + goruntime.GOOS + `",` +
		`"arch":"` + goruntime.GOARCH + `","address":"","attributes":{}},` +
		`"secrets":{},"deadline":"2030-01-01T00:00:00Z"}`

	cmd := exec.Command(shPath, entry[1:]...)
	cmd.Dir = capability.Dir
	cmd.Stdin = strings.NewReader(envelope)
	cmd.Env = []string{"PATH=" + binDir}
	out, err := cmd.Output()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("run the handler without jq: error = %v, want a nonzero exit", err)
	}
	if exitErr.ExitCode() == 0 {
		t.Fatalf("the handler exited 0, want a nonzero code")
	}
	var response struct {
		Status string `json:"status"`
		Error  struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(out, &response); err != nil {
		t.Fatalf("decode the handler response %q: %v", out, err)
	}
	if response.Status != "error" || response.Error.Code != "missing_dependency" {
		t.Errorf("response = %+v, want a missing_dependency error", response)
	}
	if !strings.Contains(response.Error.Message, "jq") {
		t.Errorf("error message %q does not name jq", response.Error.Message)
	}
}

// TestTestkitLongCompletes covers one asynchronous run that ends by itself.
func TestTestkitLongCompletes(t *testing.T) {
	if reason := testkitHandlerSkip(); reason != "" {
		t.Skip(reason)
	}
	env := newTestkitEnv(t, 0, 0)
	rec := submitTestkit(t, env, "long", `{"node":"gateway","seconds":1}`)
	if rec.Status != StateQueued {
		t.Fatalf("testkit.long status = %q, want queued", rec.Status)
	}
	done := waitTestkitDone(t, env, rec.ID, 120*time.Second)
	if done.Status != StateCompleted {
		t.Fatalf("testkit.long status = %q, want completed (failure %+v)", done.Status, done.Failure)
	}
	if string(done.Result) != `{"seconds":1}` {
		t.Errorf("testkit.long result = %s, want {\"seconds\":1}", done.Result)
	}
	if !logContains(t, env.manager, done.ID, "long: second 1 of 1") {
		t.Errorf("testkit.long logs do not hold a progress line: %v", logLines(t, env.manager, done.ID))
	}
}

// TestTestkitLongConflict covers the concurrency limit of testkit.long: one
// run per node, so a second run on the same node conflicts while another
// node accepts a run.
func TestTestkitLongConflict(t *testing.T) {
	if reason := testkitHandlerSkip(); reason != "" {
		t.Skip(reason)
	}
	env := newTestkitEnv(t, 0, 0)
	first := submitTestkit(t, env, "long", `{"node":"gateway","seconds":60}`)

	_, err := env.manager.Submit(Request{
		Capability: "testkit",
		Operation:  "long",
		Arguments:  json.RawMessage(`{"node":"gateway","seconds":60}`),
		Requester:  "test",
	})
	if !errors.Is(err, herrors.OperationConflict) {
		t.Fatalf("a second run on the same node: error = %v, want operation_conflict", err)
	}
	var apiErr *herrors.Error
	if errors.As(err, &apiErr) {
		if apiErr.Details["operation_id"] != first.ID {
			t.Errorf("conflict details %v do not name the running operation %s",
				apiErr.Details, first.ID)
		}
	}

	other := submitTestkit(t, env, "long", `{"node":"node-b","seconds":1}`)
	if other.Status != StateQueued {
		t.Fatalf("a run on another node: status = %q, want queued", other.Status)
	}
	otherDone := waitTestkitDone(t, env, other.ID, 120*time.Second)
	if otherDone.Status != StateCompleted {
		t.Errorf("the run on node-b: status = %q, want completed (failure %+v)",
			otherDone.Status, otherDone.Failure)
	}

	// Cancel can return while a slow executor is still starting the
	// handler; the final state is what this test checks.
	if _, err := env.manager.Cancel(first.ID); err != nil {
		t.Fatalf("cancel testkit.long: %v", err)
	}
	done := waitTestkitDone(t, env, first.ID, 120*time.Second)
	if done.Status != StateCancelled {
		t.Errorf("final status = %q, want cancelled (failure %+v)", done.Status, done.Failure)
	}
}

// TestTestkitBackground covers the sparse lines of the long-running
// operation.
func TestTestkitBackground(t *testing.T) {
	if reason := testkitHandlerSkip(); reason != "" {
		t.Skip(reason)
	}
	env := newTestkitEnv(t, 0, 0)
	rec := submitTestkit(t, env, "background", `{"seconds":2,"interval_seconds":1}`)
	done := waitTestkitDone(t, env, rec.ID, 120*time.Second)
	if done.Status != StateCompleted {
		t.Fatalf("testkit.background status = %q, want completed (failure %+v, logs %v)",
			done.Status, done.Failure, logLines(t, env.manager, done.ID))
	}
	var result struct {
		Seconds int `json:"seconds"`
		Ticks   int `json:"ticks"`
	}
	if err := json.Unmarshal(done.Result, &result); err != nil {
		t.Fatalf("decode testkit.background result %s: %v", done.Result, err)
	}
	if result.Seconds != 2 || result.Ticks < 2 {
		t.Errorf("result = %+v, want 2 seconds and at least 2 ticks", result)
	}
	if !logContains(t, env.manager, done.ID, "background: tick 1") {
		t.Errorf("testkit.background logs do not hold a tick line: %v", logLines(t, env.manager, done.ID))
	}
}

// TestTestkitFail covers both halves of the operation: a chosen exit code,
// and a chosen handler error.
func TestTestkitFail(t *testing.T) {
	if reason := testkitHandlerSkip(); reason != "" {
		t.Skip(reason)
	}
	env := newTestkitEnv(t, 0, 0)

	handlerError := submitTestkit(t, env, "fail",
		`{"error_code":"target_unreachable","message":"no answer","details":{"waited_seconds":120}}`)
	if handlerError.Status != StateFailed {
		t.Fatalf("testkit.fail with an error status = %q, want failed", handlerError.Status)
	}
	if handlerError.Failure == nil || handlerError.Failure.Code != "target_unreachable" {
		t.Fatalf("failure = %+v, want code target_unreachable", handlerError.Failure)
	}
	if handlerError.Failure.Message != "no answer" {
		t.Errorf("failure message = %q, want %q", handlerError.Failure.Message, "no answer")
	}
	if got := fmt.Sprint(handlerError.Failure.Details["waited_seconds"]); got != "120" {
		t.Errorf("failure details waited_seconds = %q, want 120", got)
	}

	exit := submitTestkit(t, env, "fail", `{"exit_code":3}`)
	if exit.Status != StateFailed {
		t.Fatalf("testkit.fail with exit code 3 status = %q, want failed", exit.Status)
	}
	if exit.Failure == nil || exit.Failure.Code != herrors.HandlerExit.Name() {
		t.Fatalf("failure = %+v, want code %s", exit.Failure, herrors.HandlerExit.Name())
	}
	if !logContains(t, env.manager, exit.ID, "fail: exiting with code 3") {
		t.Errorf("testkit.fail logs do not hold the exit line: %v", logLines(t, env.manager, exit.ID))
	}
}

// TestTestkitHangCancel covers a handler that ignores the stop request: the
// agent waits for the grace period and then kills the process tree.
func TestTestkitHangCancel(t *testing.T) {
	if reason := testkitHandlerSkip(); reason != "" {
		t.Skip(reason)
	}
	env := newTestkitEnv(t, 0, 0)
	rec := submitTestkit(t, env, "hang", `{"seconds":60}`)
	waitTestkitState(t, env, rec.ID, StateRunning, 120*time.Second)
	// Wait for the handler's own line, so the stop request arrives while it
	// ignores it: a slow executor can take seconds to start the handler.
	waitTestkitLog(t, env, rec.ID, "hang: ignoring stop requests", 120*time.Second)

	if _, err := env.manager.Cancel(rec.ID); err != nil {
		t.Fatalf("cancel testkit.hang: %v", err)
	}
	done := waitTestkitDone(t, env, rec.ID, 120*time.Second)
	if done.Status != StateCancelled {
		t.Fatalf("testkit.hang status = %q, want cancelled (failure %+v)", done.Status, done.Failure)
	}
	if !logContains(t, env.manager, done.ID, "hang: ignoring stop requests") {
		t.Errorf("testkit.hang logs do not hold the handler line: %v", logLines(t, env.manager, done.ID))
	}
}

// TestTestkitTimeout covers a run that exceeds the manifest timeout.
func TestTestkitTimeout(t *testing.T) {
	if reason := testkitHandlerSkip(); reason != "" {
		t.Skip(reason)
	}
	env := newTestkitEnv(t, 0, 0)
	rec := submitTestkit(t, env, "timeout", `{"seconds":60}`)
	done := waitTestkitDone(t, env, rec.ID, 120*time.Second)
	if done.Status != StateFailed {
		t.Fatalf("testkit.timeout status = %q, want failed (logs %v)", done.Status, logLines(t, env.manager, done.ID))
	}
	if done.Failure == nil || done.Failure.Code != herrors.Timeout.Name() {
		t.Fatalf("failure = %+v, want code %s", done.Failure, herrors.Timeout.Name())
	}
}

// TestTestkitFloodResponse covers a response larger than the response limit.
func TestTestkitFloodResponse(t *testing.T) {
	if reason := testkitHandlerSkip(); reason != "" {
		t.Skip(reason)
	}
	env := newTestkitEnv(t, 1<<20, 0)
	rec := submitTestkit(t, env, "flood", `{"mode":"stdout","kilobytes":2048}`)
	done := waitTestkitDone(t, env, rec.ID, 120*time.Second)
	if done.Status != StateFailed {
		t.Fatalf("testkit.flood status = %q, want failed (logs %v)", done.Status, logLines(t, env.manager, done.ID))
	}
	if done.Failure == nil || done.Failure.Code != herrors.HandlerOutputTooLarge.Name() {
		t.Fatalf("failure = %+v, want code %s", done.Failure, herrors.HandlerOutputTooLarge.Name())
	}
}

// TestTestkitFloodLogs covers the total log limit: the handler keeps
// running, the log is cut, and one system entry says so.
func TestTestkitFloodLogs(t *testing.T) {
	if reason := testkitHandlerSkip(); reason != "" {
		t.Skip(reason)
	}
	const logLimit = 8 << 10
	env := newTestkitEnv(t, 0, logLimit)
	rec := submitTestkit(t, env, "flood", `{"mode":"stderr","kilobytes":64}`)
	done := waitTestkitDone(t, env, rec.ID, 120*time.Second)
	if done.Status != StateCompleted {
		t.Fatalf("testkit.flood status = %q, want completed (failure %+v)", done.Status, done.Failure)
	}
	if !logContainsStream(t, env.manager, done.ID, StreamSystem, "log limit reached") {
		t.Errorf("the log has no limit entry: %v", logLines(t, env.manager, done.ID))
	}
	entries, err := env.manager.Logs(done.ID, 0)
	if err != nil {
		t.Fatalf("read the log: %v", err)
	}
	total := 0
	for _, entry := range entries {
		total += len(entry.Line)
		if len(entry.Line) > DefaultLogLineLimit {
			t.Errorf("one log line has %d bytes, over the line limit", len(entry.Line))
		}
	}
	if total > logLimit+512 {
		t.Errorf("stored log = %d bytes, want no more than %d plus the limit entry", total, logLimit)
	}
}

// TestTestkitChoose covers the option provider and the operation that uses
// its values.
func TestTestkitChoose(t *testing.T) {
	if reason := testkitHandlerSkip(); reason != "" {
		t.Skip(reason)
	}
	env := newTestkitEnv(t, 0, 0)

	resp, err := env.dispatcher.Execute(context.Background(), &Call{Request: &runtime.Request{
		Contract:   runtime.ContractV1,
		Kind:       runtime.KindOptions,
		ID:         "opt_test",
		Capability: "testkit",
		Operation:  "choose",
		Field:      "choice",
		Arguments:  json.RawMessage(`{}`),
	}})
	if err != nil {
		t.Fatalf("call the option provider: %v", err)
	}
	if resp.Status != runtime.StatusOK {
		t.Fatalf("option provider status = %q, want ok (error %+v)", resp.Status, resp.Err)
	}
	var options struct {
		Options []struct {
			Value string `json:"value"`
		} `json:"options"`
	}
	if err := json.Unmarshal(resp.Result, &options); err != nil {
		t.Fatalf("decode options %s: %v", resp.Result, err)
	}
	values := make([]string, 0, len(options.Options))
	for _, option := range options.Options {
		values = append(values, option.Value)
	}
	if len(values) < 2 || values[0] != "alpha" {
		t.Errorf("options = %v, want alpha and more", values)
	}

	rec := submitTestkit(t, env, "choose", `{"choice":"beta"}`)
	if rec.Status != StateCompleted {
		t.Fatalf("testkit.choose status = %q, want completed (failure %+v)", rec.Status, rec.Failure)
	}
	if string(rec.Result) != `{"choice":"beta","known":true}` {
		t.Errorf("testkit.choose result = %s, want the chosen value", rec.Result)
	}
}

// TestTestkitPrivileged covers the privileged operation: the embedded agent
// refuses to elevate it, and the handler reports HANSE_PRIVILEGED and the
// real administrator rights when it runs.
func TestTestkitPrivileged(t *testing.T) {
	if reason := testkitHandlerSkip(); reason != "" {
		t.Skip(reason)
	}
	env := newTestkitEnv(t, 0, 0)
	rec, err := env.manager.Submit(Request{
		Capability: "testkit",
		Operation:  "privileged",
		Arguments:  json.RawMessage(`{}`),
		Elevation:  &Elevation{Password: "placeholder-password"},
		Requester:  "test",
	})
	if err != nil {
		t.Fatalf("submit testkit.privileged: %v", err)
	}
	if rec.Status != StateFailed {
		t.Fatalf("testkit.privileged status = %q, want failed", rec.Status)
	}
	if rec.Failure == nil || rec.Failure.Code != herrors.ElevationDenied.Name() {
		t.Fatalf("failure = %+v, want code %s from the embedded agent",
			rec.Failure, herrors.ElevationDenied.Name())
	}

	for _, value := range []string{"0", "1"} {
		result := runTestkitPrivilegedHandler(t, env.registry, value)
		if result.HANSEPrivileged != value {
			t.Errorf("HANSE_PRIVILEGED=%s result hanse_privileged = %q", value, result.HANSEPrivileged)
		}
		if result.Privileged != (value == "1") {
			t.Errorf("HANSE_PRIVILEGED=%s result privileged = %v", value, result.Privileged)
		}
		if result.User == "" {
			t.Errorf("HANSE_PRIVILEGED=%s result has no user", value)
		}
	}
}

// privilegedResult is the result of testkit.privileged.
type privilegedResult struct {
	HANSEPrivileged string `json:"hanse_privileged"`
	Privileged      bool   `json:"privileged"`
	Admin           bool   `json:"admin"`
	User            string `json:"user"`
	OS              string `json:"os"`
}

// runTestkitPrivilegedHandler runs the real handler of testkit.privileged
// with one HANSE_PRIVILEGED value. The embedded agent refuses to elevate, so
// this helper proves the handler's own report on every platform.
func runTestkitPrivilegedHandler(t *testing.T, reg *registry.Registry, privilegedValue string) privilegedResult {
	t.Helper()
	capability, ok := reg.Capability("testkit")
	if !ok {
		t.Fatal("capabilities/testkit is not loaded")
	}
	operation, ok := capability.Operation("privileged")
	if !ok {
		t.Fatal("testkit.privileged is missing")
	}
	entry := operation.Entry[goruntime.GOOS]
	if len(entry) == 0 {
		t.Fatalf("testkit has no entry command for OS %q", goruntime.GOOS)
	}
	envelope := `{"contract":"hanse.handler/v1","kind":"operation","id":"op_test",` +
		`"capability":"testkit","operation":"privileged","arguments":{},` +
		`"nodes":{},` +
		`"executor":{"id":"gateway","name":"Gateway host","os":"` + goruntime.GOOS + `",` +
		`"arch":"` + goruntime.GOARCH + `","address":"192.0.2.10","attributes":{}},` +
		`"secrets":{},"deadline":"2030-01-01T00:00:00Z"}`

	cmd := exec.Command(entry[0], entry[1:]...)
	cmd.Dir = capability.Dir
	cmd.Stdin = strings.NewReader(envelope)
	cmd.Env = append(os.Environ(), "HANSE_PRIVILEGED="+privilegedValue)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("run the testkit.privileged handler: %v", err)
	}
	var response struct {
		Status string           `json:"status"`
		Result privilegedResult `json:"result"`
	}
	if err := json.Unmarshal(out, &response); err != nil {
		t.Fatalf("decode the handler response %q: %v", out, err)
	}
	if response.Status != "ok" {
		t.Fatalf("handler status = %q, want ok", response.Status)
	}
	return response.Result
}
