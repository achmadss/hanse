package client

import (
	"context"
	"net/http"
)

// ReloadWarning is one non-fatal problem a reload reported, such as a
// capability that routes.yaml does not mention.
type ReloadWarning struct {
	Capability string `json:"capability"`
	Message    string `json:"message"`
}

// Reload is the body of POST /v1/capabilities/reload: the capability set
// loaded from disk and the warnings of the reload.
type Reload struct {
	Capabilities []Capability    `json:"capabilities"`
	Warnings     []ReloadWarning `json:"warnings"`
}

// ReloadCapabilities calls POST /v1/capabilities/reload. Only a token
// without an allow list may call it; a restricted token answers Forbidden.
// The request has no body: the endpoint reloads from disk.
func (c *Client) ReloadCapabilities(ctx context.Context) (Reload, error) {
	var out Reload
	if err := c.do(ctx, http.MethodPost, &out, nil, nil, "v1", "capabilities", "reload"); err != nil {
		return Reload{}, err
	}
	normalize(&out.Capabilities)
	for i := range out.Capabilities {
		normalizeCapability(&out.Capabilities[i])
	}
	normalize(&out.Warnings)
	return out, nil
}
