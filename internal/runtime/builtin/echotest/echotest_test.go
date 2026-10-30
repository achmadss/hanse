package echotest

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	herrors "github.com/achmadss/hanse/internal/errors"
	"github.com/achmadss/hanse/internal/registry"
	"github.com/achmadss/hanse/internal/runtime"
)

// fastInterval keeps the sleep tests under 500 ms each.
const fastInterval = 30 * time.Millisecond

// logSink is a mutex-guarded log line slice.
type logSink struct {
	mu    sync.Mutex
	lines []string
}

func (s *logSink) add(line string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lines = append(s.lines, line)
}

func (s *logSink) snapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.lines...)
}

// waitForLines returns once the sink has at least n lines, or gives up.
func waitForLines(t *testing.T, sink *logSink, n int) {
	t.Helper()
	t0 := time.Now()
	for {
		if len(sink.snapshot()) >= n {
			return
		}
		if time.Since(t0) > 5*time.Second {
			t.Fatalf("timed out waiting for %d log lines, have %v", n, sink.snapshot())
		}
		time.Sleep(time.Millisecond)
	}
}

// install runs New and fails the test on an error.
func install(t *testing.T, interval time.Duration) (*runtime.Registry, *registry.Registry) {
	t.Helper()
	handlers, capabilities, err := New(interval)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return handlers, capabilities
}

// TestEchoPipeline exercises the pipeline as far as it exists with issue
// #15's scope: register echo, look the operation up through the capability
// registry, and run it through the builtin handler runtime. The HTTP
// request flow arrives with issue #16.
func TestEchoPipeline(t *testing.T) {
	handlers, capabilities := install(t, 0)

	capability, ok := capabilities.Capability(CapabilityID)
	if !ok {
		t.Fatal("the echo capability is not registered")
	}
	if capability.ID != CapabilityID || capability.Name != "Echo" {
		t.Fatalf("capability %s / %s", capability.ID, capability.Name)
	}
	if got := capability.OperationIDs(); len(got) != 2 || got[0] != SayOperation || got[1] != SleepOperation {
		t.Fatalf("operation IDs %v", got)
	}
	say, ok := capability.Operation(SayOperation)
	if !ok || say.Mode != registry.ModeSync {
		t.Fatalf("say: %v, %v", say, ok)
	}
	sleep, ok := capability.Operation(SleepOperation)
	if !ok || sleep.Mode != registry.ModeAsync {
		t.Fatalf("sleep: %v, %v", sleep, ok)
	}
	routing, ok := capability.RoutingOf(SayOperation)
	if !ok || routing.DefaultExecutor != DefaultGatewayHost || routing.Executors[0] != DefaultGatewayHost {
		t.Fatalf("routing of say: %v, %v", routing, ok)
	}
	if _, ok := handlers.Handler(CapabilityID, SayOperation); !ok {
		t.Fatal("echo.say is not in the handler registry")
	}

	// The run + result half of the pipeline: submit echo.say with the
	// envelope the Gateway would send, read the response.
	resp, err := RunSay(handlers, "op_test_say", "hello pipeline")
	if err != nil {
		t.Fatalf("RunSay: %v", err)
	}
	if resp.Status != runtime.StatusOK {
		t.Fatalf("status %q, want ok", resp.Status)
	}
	if got := string(resp.Result); got != `{"said":"hello pipeline"}` {
		t.Fatalf("result %s", got)
	}
	if resp.Summary != `said "hello pipeline"` {
		t.Fatalf("summary %q", resp.Summary)
	}
}

// TestEchoSchemaValidatesArguments runs candidate arguments through the
// registry-compiled schemas, the check the Gateway does before dispatch.
func TestEchoSchemaValidatesArguments(t *testing.T) {
	_, capabilities := install(t, 0)
	capability, _ := capabilities.Capability(CapabilityID)
	say, _ := capability.Operation(SayOperation)
	sleep, _ := capability.Operation(SleepOperation)

	t.Run("say accepts a message", func(t *testing.T) {
		if err := say.Schema.Validate(map[string]any{"message": "one"}); err != nil {
			t.Fatalf("valid arguments rejected: %v", err)
		}
	})
	t.Run("say rejects a missing or wrong-typed message", func(t *testing.T) {
		if err := say.Schema.Validate(map[string]any{}); err == nil {
			t.Error("missing message accepted")
		}
		if err := say.Schema.Validate(map[string]any{"message": 5.0}); err == nil {
			t.Error("numeric message accepted")
		}
	})
	t.Run("sleep accepts seconds in range", func(t *testing.T) {
		if err := sleep.Schema.Validate(map[string]any{"seconds": 600.0}); err != nil {
			t.Fatalf("valid arguments rejected: %v", err)
		}
	})
	t.Run("sleep rejects out-of-range seconds", func(t *testing.T) {
		if err := sleep.Schema.Validate(map[string]any{"seconds": 601.0}); err == nil {
			t.Error("601 accepted")
		}
		if err := sleep.Schema.Validate(map[string]any{"seconds": 0.0}); err == nil {
			t.Error("0 accepted")
		}
		if err := sleep.Schema.Validate(map[string]any{}); err == nil {
			t.Error("missing seconds accepted")
		}
	})
}

// TestSleepLogsOneLinePerInterval checks the log half of the pipeline.
func TestSleepLogsOneLinePerInterval(t *testing.T) {
	handlers, _ := install(t, fastInterval)
	sink := &logSink{}
	resp, err := handlers.Run(context.Background(), &runtime.Request{
		Contract:   runtime.ContractV1,
		Kind:       runtime.KindOperation,
		ID:         "op_test_sleep",
		Capability: CapabilityID,
		Operation:  SleepOperation,
		Arguments:  []byte(`{"seconds":3}`),
		Executor:   runtime.Node{ID: DefaultGatewayHost},
		Deadline:   time.Now().Add(time.Hour),
	}, sink.add)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if resp.Status != runtime.StatusOK {
		t.Fatalf("status %q, want ok", resp.Status)
	}
	if got := string(resp.Result); got != `{"slept":3}` {
		t.Fatalf("result %s", got)
	}
	lines := sink.snapshot()
	if len(lines) != 3 {
		t.Fatalf("expected 3 log lines, got %d: %v", len(lines), lines)
	}
	for i, want := range []string{"sleep 1 of 3", "sleep 2 of 3", "sleep 3 of 3"} {
		if lines[i] != want {
			t.Fatalf("log line %d is %q, want %q", i+1, lines[i], want)
		}
	}
}

// TestSleepCancellation proves echo.sleep honours the context: the run
// returns cancelled with the partial count soon after the cancel, and no
// line beyond the stop line follows.
func TestSleepCancellation(t *testing.T) {
	handlers, _ := install(t, fastInterval)
	sink := &logSink{}
	ctx, cancel := context.WithCancel(context.Background())
	type outcome struct {
		resp runtime.Response
		err  error
	}
	done := make(chan outcome, 1)
	go func() {
		resp, err := handlers.Run(ctx, &runtime.Request{
			Contract:   runtime.ContractV1,
			Kind:       runtime.KindOperation,
			ID:         "op_test_cancel",
			Capability: CapabilityID,
			Operation:  SleepOperation,
			Arguments:  []byte(`{"seconds":600}`),
			Executor:   runtime.Node{ID: DefaultGatewayHost},
			Deadline:   time.Now().Add(time.Hour),
		}, sink.add)
		done <- outcome{resp, err}
	}()

	// A slow run must still be running past two seconds here... asserted
	// as "no result yet" by the buffer below.
	select {
	case <-done:
		t.Fatal("echo.sleep finished instantly; the interval is ignored")
	default:
	}
	waitForLines(t, sink, 2)
	cancel()

	select {
	case o := <-done:
		if o.err != nil {
			t.Fatalf("Run: %v", o.err)
		}
		if o.resp.Status != runtime.StatusCancelled {
			t.Fatalf("status %q, want cancelled", o.resp.Status)
		}
		if got := string(o.resp.Result); !strings.Contains(got, `"slept":`) {
			t.Fatalf("partial result %s", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("echo.sleep ignored the cancellation")
	}
	lines := sink.snapshot()
	if n := len(lines); n == 0 || !strings.Contains(lines[n-1], "stop requested") {
		t.Fatalf("last log line is %q, want the stop line; all lines: %v", lines[n-1], lines)
	}
	// The interval is fastInterval, so at most a few ticks can race the
	// cancel; the full 600 is out of reach.
	if n := len(lines); n > 6 {
		t.Fatalf("echo.sleep logged %d lines after two ticks; it ignored the cancel", n)
	}
}

// TestRunRejectsWrongLookup checks the shared error codes the lookup step
// reports.
func TestRunRejectsWrongLookup(t *testing.T) {
	handlers, _ := install(t, 0)
	_, err := handlers.Run(context.Background(), &runtime.Request{
		Contract:   runtime.ContractV1,
		Kind:       runtime.KindOperation,
		ID:         "op_test_missing",
		Capability: CapabilityID,
		Operation:  "gossip",
	}, nil)
	codeOf(t, err, herrors.UnknownOperation)

	_, err = handlers.Run(context.Background(), &runtime.Request{
		Contract:   runtime.ContractV1,
		Kind:       runtime.KindOperation,
		ID:         "op_test_missing",
		Capability: "fable",
		Operation:  SayOperation,
	}, nil)
	codeOf(t, err, herrors.UnknownCapability)
}

// codeOf asserts err carries the expected shared error code.
func codeOf(t *testing.T, err error, want *herrors.Code) {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	var e *herrors.Error
	if !errors.As(err, &e) {
		t.Fatalf("expected an *herrors.Error, got %T: %v", err, err)
	}
	if e.Code() != want {
		t.Errorf("expected code %s, got %s (message: %s)", want.Name(), e.Code().Name(), e.Message)
	}
}

// TestRegisterIntoLoadedRegistry installs echo into a registry that
// registry.Load built, the shape the execution and HTTP wiring issue #16
// will have, and checks the routing, the node registry's view, and a run.
func TestRegisterIntoLoadedRegistry(t *testing.T) {
	dir := t.TempDir()
	loadInput := registry.Input{
		Nodes: []registry.Node{
			{ID: "host-1", OS: "linux", Arch: "amd64", Enabled: true},
			{ID: "node-2", OS: "windows", Arch: "amd64", Enabled: true},
		},
		GatewayHost: "host-1",
	}
	loaded, warnings, err := registry.Load(registry.Options{Dirs: []string{dir}, Input: loadInput})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings %v", warnings)
	}
	handlers := runtime.NewRegistry()
	opts := Options{GatewayHost: "host-1", SleepInterval: fastInterval}
	if err := Register(handlers, loaded, opts); err != nil {
		t.Fatalf("Register: %v", err)
	}

	capability, ok := loaded.Capability(CapabilityID)
	if !ok {
		t.Fatal("the loaded registry does not hold echo")
	}
	routing, ok := capability.RoutingOf(SayOperation)
	if !ok || routing.DefaultExecutor != "host-1" || len(routing.Executors) != 1 || routing.Executors[0] != "host-1" {
		t.Fatalf("routing of say: %v, %v", routing, ok)
	}

	// The node registry learned echo after Load built it: the Gateway
	// host lists echo's operations, the other node lists nothing.
	hostCaps, ok := loaded.Nodes.Capabilities("host-1")
	if !ok {
		t.Fatal("the Gateway host is not a known node")
	}
	found := false
	for _, nc := range hostCaps {
		if nc.Capability != CapabilityID {
			continue
		}
		found = true
		if len(nc.Operations) != 2 || nc.Operations[0] != SayOperation || nc.Operations[1] != SleepOperation {
			t.Fatalf("the Gateway host lists %v", nc.Operations)
		}
	}
	if !found {
		t.Fatalf("the Gateway host lists %v, without echo", hostCaps)
	}
	otherCaps, _ := loaded.Nodes.Capabilities("node-2")
	for _, nc := range otherCaps {
		if nc.Capability == CapabilityID {
			t.Fatal("node-2 must not list echo")
		}
	}

	// The run: the loaded registry resolved the operation, the handler
	// registry executed it.
	resp, err := RunSay(handlers, "op_loaded", "hello host")
	if err != nil {
		t.Fatalf("RunSay: %v", err)
	}
	if resp.Status != runtime.StatusOK || string(resp.Result) != `{"said":"hello host"}` {
		t.Fatalf("response %+v", resp)
	}
}

func TestRegisterRejectsOptions(t *testing.T) {
	handlers := runtime.NewRegistry()
	loaded, err := LoadMinimal(t)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	t.Run("missing GatewayHost", func(t *testing.T) {
		err := Register(handlers, loaded, Options{})
		if err == nil || !strings.Contains(err.Error(), "GatewayHost is required") {
			t.Fatalf("err %v", err)
		}
		if _, ok := loaded.Capability(CapabilityID); ok {
			t.Fatal("echo was registered despite the error")
		}
	})
	t.Run("invalid GatewayHost", func(t *testing.T) {
		err := Register(handlers, loaded, Options{GatewayHost: "Host-1"})
		if err == nil || !strings.Contains(err.Error(), "not a valid node ID") {
			t.Fatalf("err %v", err)
		}
		if _, ok := loaded.Capability(CapabilityID); ok {
			t.Fatal("echo was registered despite the error")
		}
	})
	t.Run("unknown node", func(t *testing.T) {
		err := Register(handlers, loaded, Options{GatewayHost: "ghost-node"})
		if err == nil {
			t.Fatal("an unknown Gateway host node was accepted")
		}
		if _, ok := loaded.Capability(CapabilityID); ok {
			t.Fatal("echo was registered despite the error")
		}
	})
	t.Run("nil registries", func(t *testing.T) {
		if err := Register(nil, loaded, Options{GatewayHost: "host-1"}); err == nil {
			t.Fatal("a nil handler registry was accepted")
		}
		if err := Register(handlers, nil, Options{GatewayHost: "host-1"}); err == nil {
			t.Fatal("a nil capability registry was accepted")
		}
	})
}

// LoadMinimal loads an empty capability tree with two nodes, the Gateway
// host among them: the registries the execution wiring issue #16 will
// own, minus real capabilities.
func LoadMinimal(t *testing.T) (*registry.Registry, error) {
	t.Helper()
	loadInput := registry.Input{
		Nodes: []registry.Node{
			{ID: "host-1", OS: "linux", Arch: "amd64", Enabled: true},
			{ID: "node-2", OS: "windows", Arch: "amd64", Enabled: true},
		},
		GatewayHost: "host-1",
	}
	loaded, _, err := registry.Load(registry.Options{Dirs: []string{t.TempDir()}, Input: loadInput})
	return loaded, err
}
