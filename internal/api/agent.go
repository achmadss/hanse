package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	herrors "github.com/achmadss/hanse/internal/errors"
)

// WithEmbeddedNode sets the node ID whose exec handlers the Gateway runs
// inside its own process, from gateway.yaml's `node` while embedded_agent
// is true. A node agent may not enroll or connect for that node, because one
// node never has two executors (docs/architecture.md §8.1). An empty ID is
// the `embedded_agent: false` layout: the host node enrolls and connects
// like any other node.
func WithEmbeddedNode(nodeID string) Option {
	return func(s *server) { s.embeddedNode = nodeID }
}

// joinInput is the body of POST /v1/agent/join (docs/agent-protocol.md §2.2).
type joinInput struct {
	Protocol     string `json:"protocol"`
	JoinToken    string `json:"join_token"`
	AgentVersion string `json:"agent_version"`
	OS           string `json:"os"`
	Arch         string `json:"arch"`
}

// joinResponse is the body of a successful join (docs/agent-protocol.md
// §2.3). The credential appears here and nowhere else, so the agent either
// stores it or enrolls its node again.
type joinResponse struct {
	Node       string `json:"node"`
	Credential string `json:"credential"`
}

// joinAgent answers POST /v1/agent/join. The join token in the body
// authenticates the request, so this route is served without the user
// bearer-token middleware that guards the rest of the API: a node agent
// holds no API token. A token that is unknown, used, or expired gets the
// same 401 answer (docs/agent-protocol.md §2.3).
func (s *server) joinAgent(w http.ResponseWriter, r *http.Request) {
	in, ok := decodeJoin(w, r)
	if !ok {
		return
	}
	if in.Protocol != AgentProtocolV1 {
		writeError(w, herrors.New(herrors.InvalidRequest,
			"the agent protocol version is not supported",
			map[string]any{"protocol": in.Protocol}))
		return
	}
	if s.enr == nil {
		writeError(w, herrors.New(herrors.Internal,
			"the enrollment service is not available", nil))
		return
	}
	if s.reg == nil {
		writeError(w, errNoRegistry())
		return
	}
	// The node check runs inside Join, before the token is marked used, so
	// a mismatched agent keeps its token for another attempt.
	out, err := s.enr.Join(in.JoinToken, func(nodeID string) error {
		return s.checkNodeEntry(nodeID, in.OS, in.Arch)
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, joinResponse{Node: out.Node, Credential: out.Credential})
}

// checkNodeEntry requires the node to exist in nodes.yaml, to not be the
// node the Gateway runs itself, and the agent's OS and architecture to match
// it. A mismatch is the NodeMismatch code, HTTP 409 (docs/agent-protocol.md
// §2.3). A token of the embedded node is refused with the Forbidden code,
// HTTP 403: the Gateway host already runs its handlers in process, so it
// must never gain a second executor.
func (s *server) checkNodeEntry(nodeID, osName, arch string) error {
	if s.embeddedNode != "" && nodeID == s.embeddedNode {
		return herrors.New(herrors.Forbidden,
			fmt.Sprintf("node %q is the Gateway host and runs its handlers in the Gateway's embedded agent; set embedded_agent: false in gateway.yaml to enroll it as a node agent",
				nodeID),
			map[string]any{"node": nodeID})
	}
	view, ok := s.reg.Nodes.Get(nodeID)
	if !ok {
		// The node was removed from nodes.yaml after its token was issued.
		// The token names a node that no longer exists, which is the same
		// conflict as a changed entry.
		return herrors.New(herrors.NodeMismatch,
			fmt.Sprintf("node %q is not in nodes.yaml", nodeID),
			map[string]any{"node": nodeID})
	}
	if view.OS != osName || view.Arch != arch {
		return herrors.New(herrors.NodeMismatch,
			"the agent's OS or architecture differs from its node entry",
			map[string]any{"node": nodeID, "os": view.OS, "arch": view.Arch})
	}
	return nil
}

// decodeJoin reads one join body. A malformed body is the InvalidRequest
// code, HTTP 400 (docs/architecture.md §14): a body that is not a JSON
// object, trailing data after the object, an unknown top-level field, or a
// body larger than maxRequestBody.
func decodeJoin(w http.ResponseWriter, r *http.Request) (joinInput, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var body joinInput
	if err := dec.Decode(&body); err != nil {
		writeInvalidRequest(w, err)
		return joinInput{}, false
	}
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		writeInvalidRequest(w, errors.New("trailing data after the request object"))
		return joinInput{}, false
	}
	return body, true
}
