package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	herrors "github.com/achmadss/hanse/internal/errors"
)

// logCollector collects the lines the test's onLog callback receives.
type logCollector struct {
	mu    sync.Mutex
	lines []string
}

func (c *logCollector) add(line string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lines = append(c.lines, line)
}

func (c *logCollector) linesCopy() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.lines))
	copy(out, c.lines)
	return out
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
		t.Fatalf("expected code %s, got %s (message: %s)", want.Name(), e.Code().Name(), e.Message)
	}
}

func TestRegisterAndLookup(t *testing.T) {
	h := Handler(func(context.Context, *Call) (Response, error) { return Response{}, nil })
	r := NewRegistry()
	if err := r.Register("echo", "say", h); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, ok := r.Handler("echo", "say"); !ok {
		t.Fatal("say lookup failed")
	}
	if _, ok := r.Handler("echo", "other"); ok {
		t.Fatal("unknown operation lookup succeeded")
	}
	if !r.hasCapability("echo") || r.hasCapability("other") {
		t.Error("hasCapability is wrong")
	}
}

func TestRegisterRejects(t *testing.T) {
	h := Handler(func(context.Context, *Call) (Response, error) { return Response{}, nil })
	r := NewRegistry()
	if err := r.Register("echo", "say", nil); err == nil {
		t.Error("nil handler accepted")
	}
	if err := r.Register("Echo", "say", h); err == nil {
		t.Error("invalid capability ID accepted")
	} else {
		codeOf(t, err, herrors.Internal)
	}
	if err := r.Register("echo", "2say", h); err == nil {
		t.Error("invalid operation ID accepted")
	}
	if err := r.Register("echo", "say", h); err != nil {
		t.Fatalf("first Register: %v", err)
	}
	if err := r.Register("echo", "say", h); err == nil {
		t.Error("duplicate accepted")
	} else {
		codeOf(t, err, herrors.Internal)
	}
}

func TestRunHappyPath(t *testing.T) {
	r := NewRegistry()
	err := r.Register("echo", "say", func(_ context.Context, call *Call) (Response, error) {
		call.Log("first")
		call.Log("second")
		return OK(map[string]any{"said": "hi"}, "said hi")
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	collected := &logCollector{}
	resp, err := r.Run(context.Background(), &Request{
		Contract:   ContractV1,
		Kind:       KindOperation,
		ID:         "op_test",
		Capability: "echo",
		Operation:  "say",
		Arguments:  []byte(`{"message":"hi"}`),
	}, collected.add)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if resp.Status != StatusOK {
		t.Fatalf("status %q, want ok", resp.Status)
	}
	if got := string(resp.Result); got != `{"said":"hi"}` {
		t.Fatalf("result %s", got)
	}
	if resp.Summary != "said hi" {
		t.Fatalf("summary %q", resp.Summary)
	}
	lines := collected.linesCopy()
	if len(lines) != 2 || lines[0] != "first" || lines[1] != "second" {
		t.Fatalf("log lines %v", lines)
	}
}

func TestRunRejectsEnvelope(t *testing.T) {
	r := NewRegistry()
	err := r.Register("echo", "say", Handler(func(context.Context, *Call) (Response, error) {
		t.Error("handler ran for a rejected envelope")
		return Response{}, nil
	}))
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	_, err = r.Run(context.Background(), &Request{
		Kind: KindOperation, ID: "x", Capability: "missing", Operation: "say",
	}, nil)
	if err == nil {
		t.Fatal("unknown capability accepted")
	}
	codeOf(t, err, herrors.UnknownCapability)

	_, err = r.Run(context.Background(), &Request{
		Kind: KindOperation, ID: "x", Capability: "echo", Operation: "listen",
	}, nil)
	if err == nil {
		t.Fatal("unknown operation accepted")
	}
	codeOf(t, err, herrors.UnknownOperation)

	_, err = r.Run(context.Background(), &Request{
		Contract: "hanse.handler/v2", Kind: KindOperation, ID: "x",
		Capability: "echo", Operation: "say",
	}, nil)
	if err == nil {
		t.Fatal("wrong contract accepted")
	}
	codeOf(t, err, herrors.UnsupportedContract)

	_, err = r.Run(context.Background(), &Request{
		Kind: "query", ID: "x", Capability: "echo", Operation: "say",
	}, nil)
	if err == nil {
		t.Fatal("wrong kind accepted")
	}
	codeOf(t, err, herrors.Internal)

	_, err = r.Run(context.Background(), nil, nil)
	if err == nil {
		t.Fatal("nil request accepted")
	}
	codeOf(t, err, herrors.Internal)
}

func TestErrorResponsePassesThrough(t *testing.T) {
	r := NewRegistry()
	err := r.Register("echo", "fail", Handler(func(context.Context, *Call) (Response, error) {
		return Fail("target_unreachable", "does not matter", map[string]any{"waited_seconds": 2.0}), nil
	}))
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	resp, err := r.Run(context.Background(), &Request{
		Contract: ContractV1, Kind: KindOperation, ID: "op_test",
		Capability: "echo", Operation: "fail",
	}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if resp.Status != StatusError || resp.Err == nil {
		t.Fatalf("response %+v, want an error status", resp)
	}
	if resp.Err.Code != "target_unreachable" || resp.Err.Message != "does not matter" {
		t.Fatalf("error object %+v", resp.Err)
	}
	if v, ok := resp.Err.Details["waited_seconds"]; !ok || v != 2.0 {
		t.Fatalf("details %+v", resp.Err.Details)
	}
}

func TestGoErrorBecomesInternal(t *testing.T) {
	r := NewRegistry()
	err := r.Register("echo", "say", Handler(func(context.Context, *Call) (Response, error) {
		return Response{}, errors.New("boom")
	}))
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	_, err = r.Run(context.Background(), &Request{
		Contract: ContractV1, Kind: KindOperation, ID: "op_test",
		Capability: "echo", Operation: "say",
	}, nil)
	codeOf(t, err, herrors.Internal)
}

func TestPanickedHandlerBecomesInternal(t *testing.T) {
	r := NewRegistry()
	err := r.Register("echo", "say", Handler(func(context.Context, *Call) (Response, error) {
		panic("handler bug")
	}))
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	_, err = r.Run(context.Background(), &Request{
		Contract: ContractV1, Kind: KindOperation, ID: "op_test",
		Capability: "echo", Operation: "say",
	}, nil)
	codeOf(t, err, herrors.Internal)
	if got := err.Error(); !strings.Contains(got, "panicked: handler bug") {
		t.Fatalf("error text %q", err.Error())
	}
}

func TestContextCancellationReaches(t *testing.T) {
	r := NewRegistry()
	err := r.Register("echo", "wait", Handler(func(ctx context.Context, _ *Call) (Response, error) {
		<-ctx.Done()
		return Cancelled(nil, "")
	}))
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_, err := r.Run(ctx, &Request{
			Contract: ContractV1, Kind: KindOperation, ID: "op_test",
			Capability: "echo", Operation: "wait",
		}, nil)
		if err != nil {
			t.Errorf("Run: %v", err)
		}
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
}

func TestFailBuildsErrorEnvelope(t *testing.T) {
	resp := Fail("bad-code", "not snake_case", nil)
	if resp.Err.Details != nil {
		t.Fatalf("details %+v", resp.Err.Details)
	}
	if resp.Status != StatusError {
		t.Fatalf("status %q", resp.Status)
	}
}

func TestRunValidatesResponse(t *testing.T) {
	cases := []Response{
		{},
		{Status: ""},
		{Status: "done"},
		{Status: StatusError, Err: nil, Result: []byte(`{}`)},
		{Status: StatusOK, Err: &ErrInfo{Code: "x", Message: "y"}},
		{Status: StatusCancelled, Err: &ErrInfo{Code: "x", Message: "y"}},
	}
	for _, bad := range cases {
		got := bad
		t.Run(fmt.Sprintf("case status %q", got.Status), func(t *testing.T) {
			r := NewRegistry()
			err := r.Register("echo", "say", Handler(func(context.Context, *Call) (Response, error) {
				return got, nil
			}))
			if err != nil {
				t.Fatalf("Register: %v", err)
			}
			resp, out := r.Run(context.Background(), &Request{
				Contract: ContractV1, Kind: KindOperation, ID: "op_test",
				Capability: "echo", Operation: "say",
			}, nil)
			codeOf(t, out, herrors.Internal)
			if !strings.Contains(out.Error(), "returned an invalid response") {
				t.Errorf("error text %q", out.Error())
			}
			if resp.Status != "" || len(resp.Result) != 0 || resp.Summary != "" || resp.Err != nil {
				t.Errorf("Run returned a response along with the error: %+v", resp)
			}
		})
	}
}

func TestRunRejectsOptionsKind(t *testing.T) {
	r := NewRegistry()
	err := r.Register("echo", "say", Handler(func(context.Context, *Call) (Response, error) {
		t.Error("the operation handler ran for an options call")
		return Response{}, nil
	}))
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	_, out := r.Run(context.Background(), &Request{
		Contract: ContractV1, Kind: KindOptions, ID: "opt_test",
		Capability: "echo", Operation: "say", Field: "message",
	}, nil)
	codeOf(t, out, herrors.Internal)
}

// TestRunSerializesLog calls call.Log from many goroutines; without the
// runtime's mutex the -race detector fires and lines could be lost.
func TestRunSerializesLog(t *testing.T) {
	const goroutines, perGoroutine = 8, 50
	r := NewRegistry()
	err := r.Register("echo", "chatty", Handler(func(_ context.Context, call *Call) (Response, error) {
		var done sync.WaitGroup
		for g := 0; g < goroutines; g++ {
			done.Add(1)
			go func(g int) {
				defer done.Done()
				for i := 0; i < perGoroutine; i++ {
					call.Log(fmt.Sprintf("g%d-%d", g, i))
				}
			}(g)
		}
		done.Wait()
		return OK(nil, "")
	}))
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	collected := &logCollector{}
	resp, out := r.Run(context.Background(), &Request{
		Contract: ContractV1, Kind: KindOperation, ID: "op_test",
		Capability: "echo", Operation: "chatty",
	}, collected.add)
	if out != nil {
		t.Fatalf("Run: %v", out)
	}
	if resp.Status != StatusOK {
		t.Fatalf("status %q", resp.Status)
	}
	lines := collected.linesCopy()
	if len(lines) != goroutines*perGoroutine {
		t.Fatalf("got %d lines, want %d", len(lines), goroutines*perGoroutine)
	}
	seen := map[string]bool{}
	for _, line := range lines {
		seen[line] = true
	}
	for g := 0; g < goroutines; g++ {
		for i := 0; i < perGoroutine; i++ {
			if !seen[fmt.Sprintf("g%d-%d", g, i)] {
				t.Fatalf("line g%d-%d is missing", g, i)
			}
		}
	}
}
