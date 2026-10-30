package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/achmadss/hanse/internal/enroll"
	herrors "github.com/achmadss/hanse/internal/errors"
	"github.com/achmadss/hanse/internal/operations"
	"github.com/achmadss/hanse/internal/registry"
)

// New returns the routes of the versioned API: the discovery endpoints, the
// execution endpoints of issue #16, and the agent enrollment endpoint of
// issue #37. The server mounts the handler under /v1 and wraps it with
// auth.Store.Middleware, which authenticates every request and stores the
// identity the execution routes authorize. POST /v1/agent/join is the one
// exception: its join token authenticates it, so the middleware must skip
// that route when the server wires the two together.
//
// reg may be nil: every discovery route then answers with the Internal
// code instead of panicking. ops may be nil too; the execution routes then
// answer with the Internal code, so a Gateway without a dispatcher never
// pretends to run an operation. enr may be nil as well; the join route then
// answers with the Internal code.
func New(reg *registry.Registry, ops *operations.Manager, enr *enroll.Service) http.Handler {
	s := &server{reg: reg, ops: ops, enr: enr}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/version", s.getVersion)
	mux.HandleFunc("GET /v1/capabilities", s.listCapabilities)
	mux.HandleFunc("GET /v1/capabilities/{id}", s.getCapability)
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
	return mux
}

// server is the shared state of the API handlers.
type server struct {
	reg *registry.Registry
	ops *operations.Manager
	enr *enroll.Service
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
