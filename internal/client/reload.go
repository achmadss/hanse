package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	herrors "github.com/achmadss/hanse/internal/errors"
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
func (c *Client) ReloadCapabilities(ctx context.Context) (Reload, error) {
	var out Reload
	if err := c.postReload(ctx, &out, "v1", "capabilities", "reload"); err != nil {
		return Reload{}, err
	}
	normalize(&out.Capabilities)
	for i := range out.Capabilities {
		normalizeCapability(&out.Capabilities[i])
	}
	normalize(&out.Warnings)
	return out, nil
}

// postReload performs one POST request and decodes its JSON body into out. The
// request has no body: the endpoint reloads from disk.
func (c *Client) postReload(ctx context.Context, out any, segments ...string) error {
	endpoint, err := c.endpoint(segments)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
	if err != nil {
		return herrors.New(herrors.Internal, "the request could not be built: "+err.Error(), nil)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.http.Do(req)
	if err != nil {
		return herrors.New(herrors.Internal,
			fmt.Sprintf("cannot reach the Gateway at %s: %v", c.baseURL.Redacted(), err), nil)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return herrors.New(herrors.Internal,
			fmt.Sprintf("the response from %s could not be read: %v", c.baseURL.Redacted(), err), nil)
	}
	if len(body) > maxResponseBytes {
		return herrors.New(herrors.Internal,
			fmt.Sprintf("the response from %s is larger than %d bytes", c.baseURL.Redacted(), maxResponseBytes), nil)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return errorFromResponse(resp.StatusCode, body)
	}

	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(out); err != nil {
		return herrors.New(herrors.Internal,
			fmt.Sprintf("the Gateway at %s returned an invalid JSON response: %v", c.baseURL.Redacted(), err), nil)
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		return herrors.New(herrors.Internal,
			fmt.Sprintf("the Gateway at %s returned an invalid JSON response: trailing data", c.baseURL.Redacted()), nil)
	}
	return nil
}
