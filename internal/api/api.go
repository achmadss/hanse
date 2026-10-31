package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/achmadss/hanse/internal/agentconn"
	"github.com/achmadss/hanse/internal/enroll"
	herrors "github.com/achmadss/hanse/internal/errors"
	"github.com/achmadss/hanse/internal/operations"
	"github.com/achmadss/hanse/internal/registry"
)

// New returns the routes of the versioned API: the discovery endpoints, the
// execution endpoints of issue #16, and the agent endpoints of issues #37
// and #38. The server mounts the handler under /v1 and wraps it with
// auth.Store.Middleware, which authenticates every request and stores the
// identity the execution routes authorize. Three routes are the exception:
// POST /v1/agent/join, GET /v1/agent/connect, and GET /v1/agent/bundles/
// {hash} authenticate themselves with a join token or a node credential,
// so the middleware must skip them when the server wires the two together.
//
// reg may be nil: every discovery route then answers with the Internal
// code instead of panicking. ops may be nil too; the execution routes then
// answer with the Internal code, so a Gateway without a dispatcher never
// pretends to run an operation. enr may be nil as well; the join route then
// answers with the Internal code. Options carry the agent connection hub
// and the bundle source; a Gateway without them answers the agent routes
// with the Internal or BundleUnavailable code.
func New(reg *registry.Registry, ops *operations.Manager, enr *enroll.Service, options ...Option) http.Handler {
	return newServer(reg, ops, enr, DefaultSSEKeepAlive, options...)
}

// NewWithOptionProviders is New with the handler-backed option providers of
// the Gateway. The exec provider runs through the embedded agent and the
// HTTP provider through the HTTP dispatcher (docs/handler-contract.md
// section 8).
func NewWithOptionProviders(reg *registry.Registry, ops *operations.Manager, enr *enroll.Service, providers OptionProviders) http.Handler {
	return newServer(reg, ops, enr, DefaultSSEKeepAlive, WithOptionProviders(providers))
}

// newServer builds the routes of a server with the given keepalive interval.
// Tests lower the interval to see a keepalive comment on a short stream.
func newServer(reg *registry.Registry, ops *operations.Manager, enr *enroll.Service, keepAlive time.Duration, options ...Option) http.Handler {
	s := &server{reg: reg, ops: ops, enr: enr, keepAlive: keepAlive}
	for _, option := range options {
		option(s)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /readyz", s.readyz)
	mux.HandleFunc("GET /v1/version", s.getVersion)
	mux.HandleFunc("GET /v1/capabilities", s.listCapabilities)
	mux.HandleFunc("GET /v1/capabilities/{id}", s.getCapability)
	mux.HandleFunc("POST /v1/capabilities/reload", s.reloadCapabilities)
	mux.HandleFunc("GET /v1/nodes", s.listNodes)
	mux.HandleFunc("GET /v1/nodes/{id}", s.getNode)
	mux.HandleFunc("GET /v1/nodes/{id}/capabilities", s.getNodeCapabilities)
	mux.HandleFunc("GET /v1/operations/{capability}/{operation}/schema", s.getSchema)
	mux.HandleFunc("GET /v1/operations/{capability}/{operation}/describe", s.describeOperation)
	mux.HandleFunc("GET /v1/operations/{capability}/{operation}/options/{field}", s.listOptions)
	mux.HandleFunc("POST /v1/operations", s.submitOperation)
	mux.HandleFunc("POST /v1/operations/validate", s.validateOperation)
	mux.HandleFunc("GET /v1/operations", s.listOperations)
	mux.HandleFunc("GET /v1/operations/{id}", s.getOperation)
	mux.HandleFunc("GET /v1/operations/{id}/logs", s.getOperationLogs)
	mux.HandleFunc("POST /v1/operations/{id}/cancel", s.cancelOperation)
	mux.HandleFunc("POST /v1/agent/join", s.joinAgent)
	mux.HandleFunc("GET /v1/agent/connect", s.connectAgent)
	mux.HandleFunc("GET /v1/agent/bundles/{hash}", s.getBundle)
	return mux
}

// DefaultSSEKeepAlive is how often a followed log sends an SSE comment line
// while it has no new line. A reverse proxy closes an idle upstream after
// about 60 seconds, so the stream must not stay silent
// (docs/architecture.md section 10).
const DefaultSSEKeepAlive = 15 * time.Second

// Option sets an optional part of the API, such as the agent connection
// hub. The variadic form keeps existing callers working while the Gateway
// wiring adds parts.
type Option func(*server)

// WithAgentHub sets the node agent connection hub that the connect route
// and the bundle credential check use.
func WithAgentHub(hub *agentconn.Hub) Option {
	return func(s *server) { s.agents = hub }
}

// WithOptionProviders sets the handler-backed option providers of the
// capabilities the Gateway loaded, handler contract section 8.
func WithOptionProviders(providers OptionProviders) Option {
	return func(s *server) { s.optionProviders = providers }
}

// server is the shared state of the API handlers.
type server struct {
	reg *registry.Registry
	ops *operations.Manager
	enr *enroll.Service
	// keepAlive is how often a followed log sends a keepalive comment.
	// Zero disables the keepalive.
	keepAlive time.Duration
	agents    *agentconn.Hub
	bundles   BundleSource
	// embeddedNode is the node ID the Gateway runs handlers for inside its
	// own process, from gateway.yaml's `node`. Empty means the Gateway has
	// no embedded agent.
	embeddedNode string
	// optionProviders are the handler-backed option providers of the
	// capabilities this Gateway loaded.
	optionProviders OptionProviders
	// ready reports whether the Gateway can serve API traffic. Nil means
	// not ready; the /readyz route then answers 503.
	ready func() bool
}

// connectAgent answers GET /v1/agent/connect: the hub authenticates the
// node credential, upgrades the connection, and runs the agent protocol. A
// credential of the Gateway host node is refused first when the Gateway
// runs an embedded agent for it (docs/architecture.md §8).
func (s *server) connectAgent(w http.ResponseWriter, r *http.Request) {
	if s.agents == nil {
		writeError(w, herrors.New(herrors.Internal,
			"the agent connection hub is not available", nil))
		return
	}
	if err := s.refuseEmbeddedNode(r); err != nil {
		writeError(w, err)
		return
	}
	s.agents.ServeConnect(w, r)
}

// writeJSON writes v as an indented JSON response with the given status.
// Keys keep a fixed order: struct fields keep their declaration order and
// map keys are sorted by encoding/json, so two responses to the same
// request are byte-identical (docs/architecture.md section 11). HTML
// escaping is off so that argument descriptions and schema patterns keep
// their text unchanged.
func writeJSON(w http.ResponseWriter, status int, v any) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		writeError(w, herrors.New(herrors.Internal,
			"the response could not be encoded: "+err.Error(), nil))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

// writeError writes the shared error object with the code's HTTP status
// (docs/architecture.md section 14).
func writeError(w http.ResponseWriter, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(herrors.HTTPStatus(err))
	_ = herrors.WriteJSON(w, err)
}

// lookupCapability returns one capability or writes the UnknownCapability
// error, HTTP 404.
func (s *server) lookupCapability(w http.ResponseWriter, id string) (*registry.Capability, bool) {
	if s.reg == nil {
		writeError(w, errNoRegistry())
		return nil, false
	}
	c, ok := s.reg.Capability(id)
	if !ok {
		writeError(w, herrors.New(herrors.UnknownCapability,
			fmt.Sprintf("capability %q does not exist", id),
			map[string]any{"capability": id}))
		return nil, false
	}
	return c, true
}

// lookupOperation returns one operation or writes the UnknownCapability or
// UnknownOperation error, HTTP 404.
func (s *server) lookupOperation(w http.ResponseWriter, capID, opID string) (*registry.Capability, *registry.Operation, bool) {
	c, ok := s.lookupCapability(w, capID)
	if !ok {
		return nil, nil, false
	}
	op, ok := c.Operation(opID)
	if !ok {
		writeError(w, herrors.New(herrors.UnknownOperation,
			fmt.Sprintf("capability %q has no operation %q", capID, opID),
			map[string]any{"capability": capID, "operation": opID}))
		return nil, nil, false
	}
	return c, op, true
}

// lookupNode returns one node or writes the InvalidExecutor error, HTTP
// 404: the code set has no separate "unknown node" code, and this is the
// one that reports a node ID that is not known.
func (s *server) lookupNode(w http.ResponseWriter, id string) (registry.NodeView, bool) {
	if s.reg == nil {
		writeError(w, errNoRegistry())
		return registry.NodeView{}, false
	}
	view, ok := s.reg.Nodes.Get(id)
	if !ok {
		writeError(w, herrors.New(herrors.InvalidExecutor,
			fmt.Sprintf("node %q does not exist", id),
			map[string]any{"node": id}))
		return registry.NodeView{}, false
	}
	return view, true
}

// errNoRegistry is the error of every route when the Gateway has no loaded
// registry.
func errNoRegistry() error {
	return herrors.New(herrors.Internal, "the capability registry is not available", nil)
}
