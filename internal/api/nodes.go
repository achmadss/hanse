package api

import (
	"net/http"

	"github.com/achmadss/hanse/internal/registry"
)

// nodeView is one node with its configured values and its current status
// (docs/configuration.md section 4.1, architecture.md section 8.2).
// Attributes are non-secret by rule, so they are part of the response.
type nodeView struct {
	ID         string            `json:"id"`
	OS         string            `json:"os"`
	Arch       string            `json:"arch"`
	Roles      []string          `json:"roles"`
	Attributes map[string]string `json:"attributes"`
	Enabled    bool              `json:"enabled"`
	Status     string            `json:"status"`
	// AgentVersion is the version the node's agent reported on its last
	// connection, or "" when none has connected since the Gateway
	// started (docs/agent-protocol.md section 4.1).
	AgentVersion string `json:"agent_version"`
}

// nodeCapabilityView is one capability a node can execute, with the
// operations it may run.
type nodeCapabilityView struct {
	ID         string   `json:"id"`
	Operations []string `json:"operations"`
}

// nodesResponse is the body of GET /v1/nodes.
type nodesResponse struct {
	Nodes []nodeView `json:"nodes"`
}

// nodeResponse is the body of GET /v1/nodes/{id}.
type nodeResponse struct {
	Node nodeView `json:"node"`
}

// nodeCapabilitiesResponse is the body of GET /v1/nodes/{id}/capabilities.
type nodeCapabilitiesResponse struct {
	Node         string               `json:"node"`
	Capabilities []nodeCapabilityView `json:"capabilities"`
}

// nodeViewOf renders one node. Empty lists and maps stay empty rather than
// null, so a client always parses the same JSON types.
func nodeViewOf(view registry.NodeView) nodeView {
	n := nodeView{
		ID:           view.ID,
		OS:           view.OS,
		Arch:         view.Arch,
		Roles:        append([]string{}, view.Roles...),
		Enabled:      view.Enabled,
		Status:       string(view.Status),
		AgentVersion: view.AgentVersion,
	}
	n.Attributes = make(map[string]string, len(view.Attributes))
	for k, v := range view.Attributes {
		n.Attributes[k] = v
	}
	return n
}

// listNodes answers GET /v1/nodes. NodeRegistry.List sorts by node ID.
func (s *server) listNodes(w http.ResponseWriter, _ *http.Request) {
	if s.reg == nil {
		writeError(w, errNoRegistry())
		return
	}
	views := s.reg.Nodes.List()
	out := nodesResponse{Nodes: make([]nodeView, 0, len(views))}
	for _, view := range views {
		out.Nodes = append(out.Nodes, nodeViewOf(view))
	}
	writeJSON(w, http.StatusOK, out)
}

// getNode answers GET /v1/nodes/{id}.
func (s *server) getNode(w http.ResponseWriter, r *http.Request) {
	view, ok := s.lookupNode(w, r.PathValue("id"))
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, nodeResponse{Node: nodeViewOf(view)})
}

// getNodeCapabilities answers GET /v1/nodes/{id}/capabilities. A node that
// runs nothing, such as a target without an agent, returns an empty list.
func (s *server) getNodeCapabilities(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.lookupNode(w, id); !ok {
		return
	}
	caps, _ := s.reg.Nodes.Capabilities(id)
	out := nodeCapabilitiesResponse{
		Node:         id,
		Capabilities: make([]nodeCapabilityView, 0, len(caps)),
	}
	for _, c := range caps {
		out.Capabilities = append(out.Capabilities, nodeCapabilityView{
			ID:         c.Capability,
			Operations: append([]string{}, c.Operations...),
		})
	}
	writeJSON(w, http.StatusOK, out)
}
