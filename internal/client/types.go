package client

// The types in this file mirror the discovery response shapes of
// docs/architecture.md section 11.1. internal/api keeps its response bodies
// unexported, so the client owns the wire contract it decodes. Field order
// follows the documented key order, which also keeps the CLI's JSON output
// stable.

// Version is the body of GET /v1/version.
type Version struct {
	GatewayVersion   string   `json:"gateway_version"`
	APIVersion       string   `json:"api_version"`
	HandlerContracts []string `json:"handler_contracts"`
	AgentProtocols   []string `json:"agent_protocols"`
}

// Handler is a capability's handler kind, without its private settings.
type Handler struct {
	Kind string `json:"kind"`
}

// Operation is one operation inside a capability definition.
type Operation struct {
	ID          string `json:"id"`
	Description string `json:"description"`
	Mode        string `json:"mode"`
	Privileged  bool   `json:"privileged"`
}

// Capability is one capability's definition.
type Capability struct {
	ID          string      `json:"id"`
	Name        string      `json:"name"`
	Description string      `json:"description"`
	Contract    string      `json:"contract,omitempty"`
	Handler     Handler     `json:"handler"`
	Operations  []Operation `json:"operations"`
}

// Capabilities is the body of GET /v1/capabilities.
type Capabilities struct {
	Capabilities []Capability `json:"capabilities"`
}

// CapabilityEnvelope is the body of GET /v1/capabilities/{id}.
type CapabilityEnvelope struct {
	Capability Capability `json:"capability"`
}

// Node is one node with its configured values and current status.
type Node struct {
	ID         string            `json:"id"`
	OS         string            `json:"os"`
	Arch       string            `json:"arch"`
	Roles      []string          `json:"roles"`
	Attributes map[string]string `json:"attributes"`
	Enabled    bool              `json:"enabled"`
	Status     string            `json:"status"`
}

// Nodes is the body of GET /v1/nodes.
type Nodes struct {
	Nodes []Node `json:"nodes"`
}

// NodeEnvelope is the body of GET /v1/nodes/{id}.
type NodeEnvelope struct {
	Node Node `json:"node"`
}

// NodeCapability is one capability a node can execute.
type NodeCapability struct {
	ID         string   `json:"id"`
	Operations []string `json:"operations"`
}

// NodeCapabilities is the body of GET /v1/nodes/{id}/capabilities.
type NodeCapabilities struct {
	Node         string           `json:"node"`
	Capabilities []NodeCapability `json:"capabilities"`
}

// Option is one value a client may send for an argument.
type Option struct {
	Value       string `json:"value"`
	Label       string `json:"label,omitempty"`
	Description string `json:"description,omitempty"`
}

// Options is the body of
// GET /v1/operations/{capability}/{operation}/options/{field}.
type Options struct {
	Options []Option `json:"options"`
}
