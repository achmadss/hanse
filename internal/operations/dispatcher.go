package operations

import (
	"context"

	herrors "github.com/achmadss/hanse/internal/errors"
	"github.com/achmadss/hanse/internal/runtime"
)

// Call is one dispatch: the request envelope the handler receives and the
// optional elevation input of a privileged operation. Elevation travels
// only from the submitting request to the dispatcher; it is never stored
// in an operation record.
type Call struct {
	Request   *runtime.Request
	Elevation *Elevation
}

// Dispatcher runs one validated call on its executor. The builtin runtime
// uses BuiltinDispatcher; issue #18 wires the exec and http runtimes and
// the node agent behind the same interface. A response with status error
// is a handler failure, not a dispatcher failure; a Go error means the call
// produced no protocol response.
type Dispatcher interface {
	Execute(ctx context.Context, call *Call) (runtime.Response, error)
}

// BuiltinDispatcher dispatches to the builtin handler registry, handler
// contract section 10. Every builtin capability runs inside the Gateway.
type BuiltinDispatcher struct {
	handlers *runtime.Registry
}

// NewBuiltinDispatcher returns a dispatcher over the registered builtin
// handlers.
func NewBuiltinDispatcher(handlers *runtime.Registry) *BuiltinDispatcher {
	return &BuiltinDispatcher{handlers: handlers}
}

// Execute runs one call with the builtin runtime. The elevation input is
// accepted and ignored: builtin handlers are core Gateway operations and
// none of them is privileged, so there is nothing to elevate.
func (d *BuiltinDispatcher) Execute(ctx context.Context, call *Call) (runtime.Response, error) {
	if call == nil || call.Request == nil {
		return runtime.Response{}, herrors.New(herrors.Internal,
			"the builtin dispatcher received no request envelope", nil)
	}
	if d == nil || d.handlers == nil {
		return runtime.Response{}, herrors.New(herrors.Internal,
			"the builtin handler registry is not available", nil)
	}
	// Operation logs are issue #19; until then the lines are discarded.
	return d.handlers.Run(ctx, call.Request, nil)
}
