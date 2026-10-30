// Package echotest installs the echo test capability: a minimal builtin
// capability that tests the Gateway pipeline without real machines,
// GitHub issue #15. It provides `echo.say`, a synchronous operation that
// echoes one message, and `echo.sleep`, an asynchronous operation that
// writes one log line per interval and honours cancellation.
//
// The handler contract reserves the `builtin` kind for core Gateway
// operations, so a normal Gateway binary never imports this package:
// `echo` exists only where a test harness calls New. Issue #20 adds
// `capabilities/echo`, an `exec` package with the same operations, for
// real use and the quickstart.
package echotest

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/achmadss/hanse/internal/registry"
	"github.com/achmadss/hanse/internal/runtime"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// The echo capability's IDs and the executor its operations route to.
// Builtin handlers run inside the Gateway, so the Gateway host node is
// the only eligible executor.
const (
	CapabilityID   = "echo"
	SayOperation   = "say"
	SleepOperation = "sleep"
	// ExecutorID is the placeholder executor node ID of the Gateway host.
	// A node of this ID does not exist automatically; keep it in sync
	// with the node registry of the system under test.
	ExecutorID = "gateway-host"
)

// DefaultSleepInterval is the interval echo.sleep logs at: one line per
// second, GitHub issue #15.
const DefaultSleepInterval = time.Second

// deadline never passes: the test requests outlive it.
var deadline = time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)

// New builds the echo test capability and registers it in both registries.
// It returns the builtin handler registry holding echo.say and echo.sleep,
// and the capability registry holding the echo capability, which it adds
// through the builtin hook, registry.RegisterBuiltin. A non-positive
// sleepInterval falls back to DefaultSleepInterval.
func New(sleepInterval time.Duration) (*runtime.Registry, *registry.Registry, error) {
	if sleepInterval <= 0 {
		sleepInterval = DefaultSleepInterval
	}
	handlers := runtime.NewRegistry()
	if err := handlers.Register(CapabilityID, SayOperation, sayHandler); err != nil {
		return nil, nil, err
	}
	if err := handlers.Register(CapabilityID, SleepOperation, sleepHandler(sleepInterval)); err != nil {
		return nil, nil, err
	}

	say, err := newOperation(SayOperation, registry.ModeSync, saySchema)
	if err != nil {
		return nil, nil, err
	}
	sleep, err := newOperation(SleepOperation, registry.ModeAsync, sleepSchema)
	if err != nil {
		return nil, nil, err
	}
	capability := &registry.Capability{
		ID:          CapabilityID,
		Name:        "Echo",
		Description: "Test capability for the Gateway pipeline, without real machines",
		Manifest: &registry.Manifest{
			Version: 1,
			ID:      CapabilityID,
			Name:    "Echo",
			Handler: registry.HandlerSpec{Kind: registry.KindBuiltin},
		},
		Operations: map[string]*registry.Operation{
			SayOperation:   say,
			SleepOperation: sleep,
		},
		Routing: map[string]registry.OperationRouting{
			SayOperation:   gatewayHostRoute(),
			SleepOperation: gatewayHostRoute(),
		},
	}
	capabilities := registry.New()
	if err := capabilities.RegisterBuiltin(capability); err != nil {
		return nil, nil, err
	}
	return handlers, capabilities, nil
}

// gatewayHostRoute routes one operation to the Gateway host node, the only
// eligible executor of a builtin capability.
func gatewayHostRoute() registry.OperationRouting {
	return registry.OperationRouting{
		Executors:       []string{ExecutorID},
		DefaultExecutor: ExecutorID,
	}
}

// saySchema is echo.say's argument schema: one required message string.
const saySchema = `{
	"type": "object",
	"required": ["message"],
	"properties": {
		"message": {"type": "string", "minLength": 1, "maxLength": 4096}
	}
}`

// sleepSchema is echo.sleep's argument schema: the seconds to sleep, 1 to
// 600. Tests shrink the interval; the logged line count stays `seconds`.
const sleepSchema = `{
	"type": "object",
	"required": ["seconds"],
	"properties": {
		"seconds": {"type": "integer", "minimum": 1, "maximum": 600}
	}
}`

// newOperation builds one echo operation with its compiled schema.
func newOperation(id, mode, schemaText string) (*registry.Operation, error) {
	schema, err := compileSchema(schemaText)
	if err != nil {
		return nil, err
	}
	return &registry.Operation{
		ID:     id,
		Mode:   mode,
		Schema: schema,
	}, nil
}

// compileSchema compiles one self-contained draft 2020-12 argument schema,
// the same dialect manifest.yaml `arguments` values use.
func compileSchema(schemaText string) (*jsonschema.Schema, error) {
	doc, err := jsonschema.UnmarshalJSON(strings.NewReader(schemaText))
	if err != nil {
		return nil, err
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	const location = "http://hanse.local/schemas/argument.json"
	if err := c.AddResource(location, doc); err != nil {
		return nil, err
	}
	return c.Compile(location)
}

// sayHandler answers with the message, synchronously.
func sayHandler(_ context.Context, call *runtime.Call) (runtime.Response, error) {
	var args struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(call.Request.Arguments, &args); err != nil {
		return runtime.Response{}, err
	}
	resp, err := runtime.OK(
		map[string]any{"said": args.Message},
		fmt.Sprintf("said %q", args.Message),
	)
	if err != nil {
		return runtime.Response{}, err
	}
	return resp, nil
}

// sleepHandler logs once per interval, `seconds` times, and stops cleanly
// with the partial count when the context is cancelled. The interval comes
// from New, so tests stay fast; production builds pass
// DefaultSleepInterval.
func sleepHandler(interval time.Duration) runtime.Handler {
	return func(ctx context.Context, call *runtime.Call) (runtime.Response, error) {
		var args struct {
			Seconds int `json:"seconds"`
		}
		if err := json.Unmarshal(call.Request.Arguments, &args); err != nil {
			return runtime.Response{}, err
		}
		for i := 1; i <= args.Seconds; i++ {
			select {
			case <-ctx.Done():
				call.Log(fmt.Sprintf("sleep: stop requested, stopping at %d of %d", i-1, args.Seconds))
				resp, err := runtime.Cancelled(
					map[string]any{"slept": i - 1},
					fmt.Sprintf("stopped at %d of %d seconds", i-1, args.Seconds),
				)
				if err != nil {
					return runtime.Response{}, err
				}
				return resp, nil
			case <-time.After(interval):
			}
			call.Log(fmt.Sprintf("sleep %d of %d", i, args.Seconds))
		}
		resp, err := runtime.OK(
			map[string]any{"slept": args.Seconds},
			fmt.Sprintf("slept for %d seconds", args.Seconds),
		)
		if err != nil {
			return runtime.Response{}, err
		}
		return resp, nil
	}
}

// RunSay is the shortest way for a test harness to run echo.say: it fills
// the envelope and runs the handler. Other operations use Run directly.
func RunSay(handlers *runtime.Registry, callID, message string) (runtime.Response, error) {
	args, err := json.Marshal(map[string]any{"message": message})
	if err != nil {
		return runtime.Response{}, err
	}
	return handlers.Run(context.Background(), &runtime.Request{
		Contract:   runtime.ContractV1,
		Kind:       runtime.KindOperation,
		ID:         callID,
		Capability: CapabilityID,
		Operation:  SayOperation,
		Arguments:  args,
		Executor:   runtime.Node{ID: ExecutorID},
		Deadline:   deadline,
	}, nil)
}
