// Package echotest installs the echo test capability: a minimal builtin
// capability that tests the Gateway pipeline without real machines,
// GitHub issue #15. It provides `echo.say`, a synchronous operation that
// echoes one message, and `echo.sleep`, an asynchronous operation that
// writes one log line per interval and honours cancellation.
//
// The handler contract reserves the `builtin` kind for core Gateway
// operations, so a normal Gateway binary never imports this package:
// `echo` exists only where a test harness calls Register. Issue #20 adds
// `capabilities/echo`, an `exec` package with the same operations, for
// real use and the quickstart.
package echotest

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/achmadss/hanse/internal/config"
	"github.com/achmadss/hanse/internal/registry"
	"github.com/achmadss/hanse/internal/runtime"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// The echo capability's operation IDs and the placeholder Gateway host
// node ID the convenience constructor routes to.
const (
	CapabilityID   = "echo"
	SayOperation   = "say"
	SleepOperation = "sleep"
	// DefaultGatewayHost is the placeholder Gateway host node ID that New
	// uses. It is not a real node anywhere; routes built from it resolve
	// only inside the system under test.
	DefaultGatewayHost = "gateway-host"
)

// DefaultSleepInterval is the interval echo.sleep logs at: one line per
// second, GitHub issue #15.
const DefaultSleepInterval = time.Second

// deadline never passes: the test requests outlive it.
var deadline = time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)

// Options configures Register.
type Options struct {
	// GatewayHost is the node ID of the Gateway host node, from
	// gateway.yaml's `node`. Echo runs inside the Gateway, so that node is
	// echo's only eligible executor. Required and validated.
	GatewayHost string
	// SleepInterval is the interval echo.sleep logs at; zero or less keeps
	// DefaultSleepInterval. Tests pass a sub-second interval to stay fast.
	SleepInterval time.Duration
}

// Register installs the echo capability into the registries a test
// harness assembled: the handlers echo.say and echo.sleep into the builtin
// runtime registry, and the capability itself into the capability registry
// through the builtin hook, routed to opts.GatewayHost. A loaded registry
// from registry.Load works: its node registry learns echo's executable
// operations too, because RegisterBuiltin refreshes the mapping that
// Load's node registry built before echo existed.
func Register(handlers *runtime.Registry, capabilities *registry.Registry, opts Options) error {
	if handlers == nil {
		return fmt.Errorf("echotest: the handler registry is nil")
	}
	if capabilities == nil {
		return fmt.Errorf("echotest: the capability registry is nil")
	}
	if opts.GatewayHost == "" {
		return fmt.Errorf("echotest: Options.GatewayHost is required")
	}
	if !config.ValidID(opts.GatewayHost) {
		return fmt.Errorf("echotest: Options.GatewayHost %q is not a valid node ID", opts.GatewayHost)
	}
	interval := opts.SleepInterval
	if interval <= 0 {
		interval = DefaultSleepInterval
	}
	if err := handlers.Register(CapabilityID, SayOperation, sayHandler); err != nil {
		return err
	}
	if err := handlers.Register(CapabilityID, SleepOperation, sleepHandler(interval)); err != nil {
		return err
	}
	say, saySpec, err := sayOperation()
	if err != nil {
		return err
	}
	sleep, sleepSpec, err := sleepOperation()
	if err != nil {
		return err
	}
	capability := &registry.Capability{
		ID:          CapabilityID,
		Name:        "Echo",
		Description: "Test capability for the Gateway pipeline, without real machines",
		Manifest: &registry.Manifest{
			Version:     1,
			ID:          CapabilityID,
			Name:        "Echo",
			Description: "Test capability for the Gateway pipeline, without real machines",
			Handler:     registry.HandlerSpec{Kind: registry.KindBuiltin},
			// Discovery serves the manifest's raw argument schemas,
			// so every builtin operation carries its OperationSpec.
			Operations: map[string]*registry.OperationSpec{
				SayOperation:   saySpec,
				SleepOperation: sleepSpec,
			},
		},
		Operations: map[string]*registry.Operation{
			SayOperation:   say,
			SleepOperation: sleep,
		},
		Routing: map[string]registry.OperationRouting{
			SayOperation:   gatewayHostRoute(opts.GatewayHost),
			SleepOperation: gatewayHostRoute(opts.GatewayHost),
		},
	}
	return capabilities.RegisterBuiltin(capability)
}

// New builds both registries itself and registers echo in them, with the
// placeholder Gateway host node ID DefaultGatewayHost. Harnesses that
// carry real configuration use Register with a registry from Load.
func New(sleepInterval time.Duration) (*runtime.Registry, *registry.Registry, error) {
	handlers := runtime.NewRegistry()
	capabilities := registry.New()
	opts := Options{GatewayHost: DefaultGatewayHost, SleepInterval: sleepInterval}
	if err := Register(handlers, capabilities, opts); err != nil {
		return nil, nil, err
	}
	return handlers, capabilities, nil
}

// gatewayHostRoute routes one operation to the Gateway host node, the
// only eligible executor of a builtin capability.
func gatewayHostRoute(host string) registry.OperationRouting {
	return registry.OperationRouting{
		Executors:       []string{host},
		DefaultExecutor: host,
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

// sayOperation builds echo.say's registry entry: mode sync.
func sayOperation() (*registry.Operation, *registry.OperationSpec, error) {
	return newOperation(SayOperation, "Echo one message", registry.ModeSync, saySchema)
}

// sleepOperation builds echo.sleep's registry entry: mode async.
func sleepOperation() (*registry.Operation, *registry.OperationSpec, error) {
	return newOperation(SleepOperation, "Log one line per second for the given number of seconds", registry.ModeAsync, sleepSchema)
}

// newOperation builds one echo operation with its compiled schema and the
// manifest operation spec that discovery serves.
func newOperation(id, description, mode, schemaText string) (*registry.Operation, *registry.OperationSpec, error) {
	op := &registry.Operation{ID: id, Description: description, Mode: mode}
	raw, err := decodeSchema(schemaText)
	if err != nil {
		return nil, nil, fmt.Errorf("echotest: operation %s: %w", id, err)
	}
	if err := compileInto(raw, op); err != nil {
		return nil, nil, fmt.Errorf("echotest: operation %s: %w", id, err)
	}
	spec := &registry.OperationSpec{
		Description: description,
		Mode:        mode,
		Arguments:   raw,
	}
	return op, spec, nil
}

// decodeSchema parses one schema text into the value a manifest's
// `arguments` field would hold, so discovery can serve it back as JSON.
func decodeSchema(schemaText string) (any, error) {
	return jsonschema.UnmarshalJSON(strings.NewReader(schemaText))
}

// compileInto compiles one self-contained draft 2020-12 argument schema
// the same dialect manifest.yaml `arguments` values use, into op.Schema.
func compileInto(raw any, op *registry.Operation) error {
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	const location = "http://hanse.local/schemas/argument.json"
	if err := c.AddResource(location, raw); err != nil {
		return err
	}
	schema, err := c.Compile(location)
	if err != nil {
		return err
	}
	op.Schema = schema
	return nil
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
// with the partial count when the context is cancelled. The interval
// comes from Register, so tests stay fast; production harnesses pass
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
		Executor:   runtime.Node{ID: DefaultGatewayHost},
		Deadline:   deadline,
	}, nil)
}
