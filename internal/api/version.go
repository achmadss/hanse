package api

import (
	"net/http"

	"github.com/achmadss/hanse/internal/registry"
	"github.com/achmadss/hanse/internal/version"
)

// APIVersion is the version of the HTTP API served under /v1. A client
// compares it with its own to decide whether it can talk to the Gateway
// (docs/architecture.md section 12).
const APIVersion = "v1"

// AgentProtocolV1 is the agent protocol version the Gateway speaks
// (docs/agent-protocol.md section 12). internal/agentproto will own the
// constant when it defines the protocol.
const AgentProtocolV1 = "hanse.agent/v1"

// versionResponse is the body of GET /v1/version. The handler and agent
// protocol versions are lists because the docs describe the endpoint as
// listing every supported version (handler contract section 13, agent
// protocol section 12).
type versionResponse struct {
	GatewayVersion   string   `json:"gateway_version"`
	APIVersion       string   `json:"api_version"`
	HandlerContracts []string `json:"handler_contracts"`
	AgentProtocols   []string `json:"agent_protocols"`
}

// getVersion answers GET /v1/version.
func (s *server) getVersion(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, versionResponse{
		GatewayVersion:   version.Version,
		APIVersion:       APIVersion,
		HandlerContracts: []string{registry.ContractV1},
		AgentProtocols:   []string{AgentProtocolV1},
	})
}
