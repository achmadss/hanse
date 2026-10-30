package runtime

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/achmadss/hanse/internal/config"
	herrors "github.com/achmadss/hanse/internal/errors"
)

// Handler is one builtin handler: Go code compiled into the Gateway,
// handler contract section 10. It receives the request envelope as a Go
// value and returns the response envelope as a Go value.
//
// A returned Response carries the call's final state, status `error`
// included: an error status is the capability's own code, not a Go error.
// The Go error return means the handler could not produce a protocol
// response at all — a bug or a saturated resource — and Run reports it as
// an internal error.
type Handler func(ctx context.Context, call *Call) (Response, error)

// Call is what the runtime passes to one handler: the request envelope and
// the log emitter of the call.
type Call struct {
	// Request is the call's envelope. Handlers must not modify it; the
	// runtime shares one Request value across handler invocations.
	Request *Request
	// Log appends one line to the operation's log. Builtin handlers do
	// not write stderr; per the handler contract's kind table their log
	// lines reach the operation log as Gateway `system` events. Lines
	// keep their order and are plain text.
	Log func(line string)
}

// Registry holds the builtin handlers compiled into the Gateway binary,
// keyed by capability ID and operation ID.
type Registry struct {
	mu       sync.Mutex
	handlers map[string]Handler
}

// key is the registry key of one operation. IDs never contain a dot, so
// the first dot splits capability from operation.
func key(capability, operation string) string {
	return capability + "." + operation
}

// quote renders s for an error message.
func quote(s string) string { return fmt.Sprintf("%q", s) }

// NewRegistry returns an empty handler registry.
func NewRegistry() *Registry {
	return &Registry{handlers: map[string]Handler{}}
}

// Register installs one handler. Both IDs must be valid Hanse IDs and the
// key must be free; every other call is a bug this package reports
// instead of tolerating.
func (r *Registry) Register(capability, operation string, h Handler) error {
	if r == nil {
		return herrors.New(herrors.Internal, "cannot register into a nil runtime registry", nil)
	}
	if h == nil {
		return herrors.New(herrors.Internal,
			fmt.Sprintf("builtin handler %s.%s is nil", capability, operation), nil)
	}
	if !config.ValidID(capability) {
		return herrors.New(herrors.Internal,
			fmt.Sprintf("builtin capability ID %q is not a valid capability ID", capability), nil)
	}
	if !config.ValidID(operation) {
		return herrors.New(herrors.Internal,
			fmt.Sprintf("builtin operation ID %q is not a valid operation ID", operation), nil)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.handlers == nil {
		r.handlers = map[string]Handler{}
	}
	k := key(capability, operation)
	if _, dup := r.handlers[k]; dup {
		return herrors.New(herrors.Internal,
			fmt.Sprintf("builtin handler %s.%s is registered twice", capability, operation), nil)
	}
	r.handlers[k] = h
	return nil
}

// Handler returns one handler.
func (r *Registry) Handler(capability, operation string) (Handler, bool) {
	if r == nil {
		return nil, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	h, ok := r.handlers[key(capability, operation)]
	return h, ok
}

// hasCapability reports whether the capability has at least one handler.
func (r *Registry) hasCapability(capability string) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	prefix := capability + "."
	for k := range r.handlers {
		if strings.HasPrefix(k, prefix) {
			return true
		}
	}
	return false
}

// Run invokes the handler for the request's capability and operation. The
// context is the run's cancellation channel; deadline enforcement against
// req.Deadline belongs to the operation manager, not the runtime. onLog,
// when not nil, receives every log line in order; nil discards them.
func (r *Registry) Run(ctx context.Context, req *Request, onLog func(line string)) (Response, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if req == nil {
		return Response{}, herrors.New(herrors.Internal, "builtin run without a request envelope", nil)
	}
	if req.Contract != "" && req.Contract != ContractV1 {
		return Response{}, herrors.New(herrors.UnsupportedContract,
			fmt.Sprintf("handler contract %q, the builtin runtime speaks %s", req.Contract, ContractV1), nil)
	}
	if req.Kind != KindOperation && req.Kind != KindOptions {
		return Response{}, herrors.New(herrors.Internal,
			fmt.Sprintf("envelope kind %q is neither %s nor %s",
				req.Kind, quote(KindOperation), quote(KindOptions)), nil)
	}
	h, ok := r.Handler(req.Capability, req.Operation)
	if !ok {
		if r.hasCapability(req.Capability) {
			return Response{}, herrors.New(herrors.UnknownOperation,
				fmt.Sprintf("capability %q has no builtin operation %q", req.Capability, req.Operation), nil)
		}
		return Response{}, herrors.New(herrors.UnknownCapability,
			fmt.Sprintf("unknown capability %q", req.Capability), nil)
	}
	call := &Call{Request: req, Log: onLog}
	if call.Log == nil {
		call.Log = func(string) {}
	}
	return runHandler(ctx, call, h, req.Capability, req.Operation)
}

// runHandler invokes one handler and turns a panic into an internal error,
// so one handler's bug cannot take the Gateway down. Any other Go error
// the handler returns also means the call has no protocol response; From
// keeps herrors errors as they are and maps plain errors to internal.
func runHandler(ctx context.Context, call *Call, h Handler, capability, operation string) (resp Response, err error) {
	defer func() {
		if r := recover(); r != nil {
			resp, err = Response{}, herrors.New(herrors.Internal,
				fmt.Sprintf("builtin handler %s.%s panicked: %v", capability, operation, r), nil)
		}
	}()
	resp, err = h(ctx, call)
	if err != nil {
		return Response{}, herrors.From(err)
	}
	return resp, nil
}
