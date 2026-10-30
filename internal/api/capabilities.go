package api

import (
	"net/http"

	"github.com/achmadss/hanse/internal/registry"
)

// handlerView is the capability's handler kind without its private
// settings: clients need to know whether the handler runs in the Gateway
// or on the executor, but never see entry commands, URLs, or credentials.
type handlerView struct {
	Kind string `json:"kind"`
}

// operationSummary is one operation inside a capability definition.
type operationSummary struct {
	ID          string `json:"id"`
	Description string `json:"description"`
	Mode        string `json:"mode"`
	Privileged  bool   `json:"privileged"`
}

// capabilityView is one capability's definition (docs/architecture.md
// section 11). The same view backs the list and the single-capability
// endpoint, so a client parses one shape.
type capabilityView struct {
	ID          string             `json:"id"`
	Name        string             `json:"name"`
	Description string             `json:"description"`
	Contract    string             `json:"contract,omitempty"`
	Handler     handlerView        `json:"handler"`
	Operations  []operationSummary `json:"operations"`
}

// capabilitiesResponse is the body of GET /v1/capabilities.
type capabilitiesResponse struct {
	Capabilities []capabilityView `json:"capabilities"`
}

// capabilityResponse is the body of GET /v1/capabilities/{id}.
type capabilityResponse struct {
	Capability capabilityView `json:"capability"`
}

// capabilityViewOf renders one capability. Operations are sorted by ID.
func capabilityViewOf(c *registry.Capability) capabilityView {
	v := capabilityView{
		ID:          c.ID,
		Name:        c.Name,
		Description: c.Description,
	}
	if c.Manifest != nil {
		v.Contract = c.Manifest.Contract
		v.Handler.Kind = c.Manifest.Handler.Kind
	}
	ids := c.OperationIDs()
	v.Operations = make([]operationSummary, 0, len(ids))
	for _, id := range ids {
		op := c.Operations[id]
		if op == nil {
			continue
		}
		v.Operations = append(v.Operations, operationSummary{
			ID:          op.ID,
			Description: op.Description,
			Mode:        op.Mode,
			Privileged:  op.Privileged,
		})
	}
	return v
}

// listCapabilities answers GET /v1/capabilities.
func (s *server) listCapabilities(w http.ResponseWriter, _ *http.Request) {
	if s.reg == nil {
		writeError(w, errNoRegistry())
		return
	}
	caps := s.reg.Capabilities()
	out := capabilitiesResponse{Capabilities: make([]capabilityView, 0, len(caps))}
	for _, c := range caps {
		out.Capabilities = append(out.Capabilities, capabilityViewOf(c))
	}
	writeJSON(w, http.StatusOK, out)
}

// getCapability answers GET /v1/capabilities/{id}.
func (s *server) getCapability(w http.ResponseWriter, r *http.Request) {
	c, ok := s.lookupCapability(w, r.PathValue("id"))
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, capabilityResponse{Capability: capabilityViewOf(c)})
}
