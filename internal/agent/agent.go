// Package agent runs exec handlers on the Gateway host, using the same
// runner as the node agent (docs/architecture.md sections 7.3, 8 and 8.3).
//
// The Gateway host needs no separate node agent. The embedded agent is the
// executor of every operation whose resolved executor is the Gateway host
// node, and the runner gives it the same deadline, cancellation, log, and
// error behavior as a remote executor.
package agent

import (
	"context"
	"fmt"
	goruntime "runtime"
	"time"

	"github.com/achmadss/hanse/internal/config"
	herrors "github.com/achmadss/hanse/internal/errors"
	"github.com/achmadss/hanse/internal/registry"
	"github.com/achmadss/hanse/internal/runner"
	"github.com/achmadss/hanse/internal/runtime"
)

// Config is what an Agent needs.
type Config struct {
	// Node is the Gateway host node: the executor the embedded agent
	// represents. Its ID is required.
	Node runtime.Node
	// Registry supplies the capability directories and manifests.
	// Required.
	Registry *registry.Registry
	// ResponseLimit is gateway.yaml's limits.response_size; zero uses the
	// documented 1 MiB default.
	ResponseLimit int64
	// GracePeriod is gateway.yaml's timeouts.grace_period; zero uses the
	// documented 10 second default.
	GracePeriod time.Duration
	// OptionsTimeout is gateway.yaml's timeouts.options; zero uses the
	// documented 10 second default.
	OptionsTimeout time.Duration
	// EnvAllow is gateway.yaml's handlers.env_allow.
	EnvAllow []string
	// WorkRoot is where HANSE_WORK_DIR is created; empty means the
	// operating system's temporary directory.
	WorkRoot string
}

// Agent runs exec handlers on the Gateway host. It holds no per-call state
// and is safe for concurrent use.
type Agent struct {
	node     runtime.Node
	registry *registry.Registry
	runner   *runner.Runner
	os       string
	options  time.Duration
}

// New builds the embedded agent. It fails when the node ID or the
// capability registry is missing.
func New(cfg Config) (*Agent, error) {
	if !config.ValidID(cfg.Node.ID) {
		return nil, herrors.New(herrors.Internal,
			fmt.Sprintf("the embedded agent needs a Gateway host node ID, got %q", cfg.Node.ID), nil)
	}
	if cfg.Registry == nil {
		return nil, herrors.New(herrors.Internal,
			"the embedded agent needs a capability registry", nil)
	}
	node := cfg.Node
	if node.OS == "" {
		node.OS = goruntime.GOOS
	}
	if node.Arch == "" {
		node.Arch = goruntime.GOARCH
	}
	options := cfg.OptionsTimeout
	if options <= 0 {
		options = config.DefaultTimeouts().Options
	}
	return &Agent{
		node:     node,
		registry: cfg.Registry,
		runner: &runner.Runner{
			EnvAllow:      append([]string(nil), cfg.EnvAllow...),
			ResponseLimit: cfg.ResponseLimit,
			GracePeriod:   cfg.GracePeriod,
			WorkRoot:      cfg.WorkRoot,
		},
		os:      node.OS,
		options: options,
	}, nil
}

// Node returns the Gateway host node the agent represents.
func (a *Agent) Node() runtime.Node {
	if a == nil {
		return runtime.Node{}
	}
	attributes := make(map[string]any, len(a.node.Attributes))
	for name, value := range a.node.Attributes {
		attributes[name] = value
	}
	node := a.node
	node.Attributes = attributes
	return node
}

// Execute runs one exec handler call: an operation run or an option
// provider call. The log callback receives every stderr line as it ends; a
// nil callback discards the lines.
func (a *Agent) Execute(ctx context.Context, req *runtime.Request, onLog func(line string)) (runtime.Response, error) {
	if a == nil || a.runner == nil || a.registry == nil {
		return runtime.Response{}, herrors.New(herrors.Internal,
			"the embedded agent is not configured", nil)
	}
	if req == nil {
		return runtime.Response{}, herrors.New(herrors.Internal,
			"the embedded agent received no request envelope", nil)
	}
	if req.Contract != "" && req.Contract != runtime.ContractV1 {
		return runtime.Response{}, herrors.New(herrors.UnsupportedContract,
			fmt.Sprintf("handler contract %q, the embedded agent speaks %s",
				req.Contract, runtime.ContractV1), nil)
	}
	switch req.Kind {
	case runtime.KindOperation, runtime.KindOptions:
	default:
		return runtime.Response{}, herrors.New(herrors.Internal,
			fmt.Sprintf("envelope kind %q is neither %s nor %s",
				req.Kind, runtime.KindOperation, runtime.KindOptions), nil)
	}
	capability, ok := a.registry.Capability(req.Capability)
	if !ok {
		return runtime.Response{}, herrors.New(herrors.UnknownCapability,
			fmt.Sprintf("unknown capability %q", req.Capability), nil)
	}
	operation, ok := capability.Operation(req.Operation)
	if !ok {
		return runtime.Response{}, herrors.New(herrors.UnknownOperation,
			fmt.Sprintf("capability %q has no operation %q", req.Capability, req.Operation), nil)
	}
	if capability.Manifest == nil || capability.Manifest.Handler.Kind != registry.KindExec {
		return runtime.Response{}, herrors.New(herrors.Internal,
			fmt.Sprintf("capability %q is not an exec capability", req.Capability), nil)
	}
	if req.Executor.ID != "" && req.Executor.ID != a.node.ID {
		return runtime.Response{}, herrors.New(herrors.ExecutorUnavailable,
			fmt.Sprintf("executor %q is not the Gateway host node %q", req.Executor.ID, a.node.ID),
			map[string]any{"executor": req.Executor.ID})
	}
	if req.Executor.OS != "" && req.Executor.OS != a.os {
		return runtime.Response{}, herrors.New(herrors.ExecutorNotEligible,
			fmt.Sprintf("executor OS %q differs from this agent's OS %q", req.Executor.OS, a.os), nil)
	}
	if operation.Privileged {
		return runtime.Response{}, herrors.New(herrors.ElevationDenied,
			"the embedded agent does not elevate handlers", nil)
	}
	call := *req
	if call.Executor.ID == "" {
		call.Executor = a.Node()
	}
	if call.Kind == runtime.KindOptions {
		if call.Field == "" {
			return runtime.Response{}, herrors.New(herrors.Internal,
				"an options request envelope needs the argument field", nil)
		}
		spec := capability.Manifest.Operations[req.Operation]
		if spec == nil || spec.Options[call.Field] != "provider" {
			return runtime.Response{}, herrors.New(herrors.InvalidArguments,
				fmt.Sprintf("argument %q of %s.%s has no option provider",
					call.Field, req.Capability, req.Operation),
				map[string]any{"capability": req.Capability, "operation": req.Operation, "field": call.Field})
		}
		if call.Deadline.IsZero() {
			call.Deadline = time.Now().Add(a.options).UTC()
		}
	} else if call.Deadline.IsZero() {
		return runtime.Response{}, herrors.New(herrors.Internal,
			"an operation call needs a deadline", nil)
	}
	entry := operation.Entry[a.os]
	if len(entry) == 0 {
		return runtime.Response{}, herrors.New(herrors.HandlerStartFailed,
			fmt.Sprintf("capability %q has no entry command for OS %q", req.Capability, a.os),
			map[string]any{"capability": req.Capability, "os": a.os})
	}
	return a.runner.Run(ctx, runner.Spec{
		Request: &call,
		Entry:   entry,
		Dir:     capability.Dir,
		Log:     onLog,
	})
}
