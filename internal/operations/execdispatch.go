package operations

import (
	"context"
	"fmt"

	"github.com/achmadss/hanse/internal/agent"
	herrors "github.com/achmadss/hanse/internal/errors"
	"github.com/achmadss/hanse/internal/registry"
	"github.com/achmadss/hanse/internal/runtime"
)

// ExecConfig is what an ExecDispatcher needs.
type ExecConfig struct {
	// Registry supplies the capabilities and their exec handlers.
	// Required.
	Registry *registry.Registry
	// Agent runs the handlers on the Gateway host. Required.
	Agent *agent.Agent
}

// ExecDispatcher runs exec handler calls whose executor is the Gateway
// host node, through the embedded agent and the shared runner
// (docs/architecture.md sections 7.2 and 8.3).
//
// The dispatcher covers operation runs and option provider calls: both use
// the same envelope and the same entry command. A call for a remote
// executor belongs to the node agent dispatcher of issue #38, not here.
type ExecDispatcher struct {
	registry *registry.Registry
	agent    *agent.Agent
}

// NewExecDispatcher builds a dispatcher over the loaded capabilities and
// the embedded agent.
func NewExecDispatcher(cfg ExecConfig) (*ExecDispatcher, error) {
	if cfg.Registry == nil {
		return nil, herrors.New(herrors.Internal,
			"the exec dispatcher needs a capability registry", nil)
	}
	if cfg.Agent == nil {
		return nil, herrors.New(herrors.Internal,
			"the exec dispatcher needs the embedded agent", nil)
	}
	return &ExecDispatcher{registry: cfg.Registry, agent: cfg.Agent}, nil
}

// Execute runs one call on the Gateway host and returns the parsed
// response. Every stderr line goes to call.Log as it ends. Elevation is
// ignored: the embedded agent does not elevate handlers, and a privileged
// operation is refused rather than run with fewer rights.
func (d *ExecDispatcher) Execute(ctx context.Context, call *Call) (runtime.Response, error) {
	if call == nil || call.Request == nil {
		return runtime.Response{}, herrors.New(herrors.Internal,
			"the exec dispatcher received no request envelope", nil)
	}
	if d == nil || d.registry == nil || d.agent == nil {
		return runtime.Response{}, herrors.New(herrors.Internal,
			"the exec dispatcher is not configured", nil)
	}
	req := call.Request
	capability, ok := d.registry.Capability(req.Capability)
	if !ok {
		return runtime.Response{}, herrors.New(herrors.UnknownCapability,
			fmt.Sprintf("unknown capability %q", req.Capability), nil)
	}
	if capability.Manifest == nil || capability.Manifest.Handler.Kind != registry.KindExec {
		return runtime.Response{}, herrors.New(herrors.Internal,
			fmt.Sprintf("capability %q is not an exec capability", req.Capability), nil)
	}
	host := d.agent.Node().ID
	if req.Executor.ID != "" && req.Executor.ID != host {
		return runtime.Response{}, herrors.New(herrors.ExecutorUnavailable,
			fmt.Sprintf("executor %q is not the Gateway host node %q; remote executors use the node agent dispatcher",
				req.Executor.ID, host),
			map[string]any{"executor": req.Executor.ID})
	}
	var onLog func(line string)
	if call.Log != nil {
		onLog = func(line string) { call.Log(StreamStderr, line) }
	}
	var onSystem func(line string)
	if call.Log != nil {
		onSystem = func(line string) { call.Log(StreamSystem, line) }
	}
	return d.agent.Execute(ctx, req, onLog, onSystem)
}
