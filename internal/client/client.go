// Package client is the HTTP client for the Hanse Gateway API. It sends
// bearer-token requests to the discovery endpoints of
// docs/architecture.md section 11 and maps the Gateway's error objects to
// the shared codes of internal/errors.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	herrors "github.com/achmadss/hanse/internal/errors"
)

// maxResponseBytes bounds a discovery response. Discovery bodies are small
// and lists are bounded by the Gateway's configuration; the limit stops a
// broken server from exhausting the CLI's memory.
const maxResponseBytes = 8 << 20

// defaultTimeout bounds one request. A discovery request is a single small
// read, so a slow answer is a failure rather than something to wait out.
const defaultTimeout = 30 * time.Second

// Client sends requests to one Gateway. It is safe for concurrent use.
type Client struct {
	baseURL *url.URL
	token   string
	http    *http.Client
}

// Config describes a client. BaseURL is the Gateway root, such as
// "https://hanse.example"; Token is the bearer token. HTTPClient is
// optional and defaults to a client with a 30 second timeout.
type Config struct {
	BaseURL    string
	Token      string
	HTTPClient *http.Client
}

// New returns a client for one Gateway. The URL must be absolute and use
// HTTP or HTTPS.
func New(cfg Config) (*Client, error) {
	raw := strings.TrimSpace(cfg.BaseURL)
	if raw == "" {
		return nil, fmt.Errorf("the Gateway URL is empty")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("the Gateway URL %q is not valid: %w", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("the Gateway URL %q must use http or https", raw)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("the Gateway URL %q has no host", raw)
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = ""
	u.RawQuery = ""
	u.Fragment = ""

	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: defaultTimeout}
	}
	return &Client{baseURL: u, token: cfg.Token, http: cfg.HTTPClient}, nil
}

// BaseURL returns the Gateway root the client talks to.
func (c *Client) BaseURL() string { return c.baseURL.String() }

// Version returns GET /v1/version.
func (c *Client) Version(ctx context.Context) (Version, error) {
	var out Version
	err := c.get(ctx, &out, "v1", "version")
	return out, err
}

// Capabilities returns GET /v1/capabilities.
func (c *Client) Capabilities(ctx context.Context) (Capabilities, error) {
	var out Capabilities
	err := c.get(ctx, &out, "v1", "capabilities")
	normalize(&out.Capabilities)
	return out, err
}

// Capability returns GET /v1/capabilities/{id}.
func (c *Client) Capability(ctx context.Context, id string) (Capability, error) {
	var out CapabilityEnvelope
	err := c.get(ctx, &out, "v1", "capabilities", id)
	normalize(&out.Capability.Operations)
	return out.Capability, err
}

// Nodes returns GET /v1/nodes.
func (c *Client) Nodes(ctx context.Context) (Nodes, error) {
	var out Nodes
	err := c.get(ctx, &out, "v1", "nodes")
	normalize(&out.Nodes)
	return out, err
}

// Node returns GET /v1/nodes/{id}.
func (c *Client) Node(ctx context.Context, id string) (Node, error) {
	var out NodeEnvelope
	err := c.get(ctx, &out, "v1", "nodes", id)
	normalize(&out.Node.Roles)
	return out.Node, err
}

// NodeCapabilities returns GET /v1/nodes/{id}/capabilities.
func (c *Client) NodeCapabilities(ctx context.Context, id string) (NodeCapabilities, error) {
	var out NodeCapabilities
	err := c.get(ctx, &out, "v1", "nodes", id, "capabilities")
	normalize(&out.Capabilities)
	for i := range out.Capabilities {
		normalize(&out.Capabilities[i].Operations)
	}
	return out, err
}

// Schema returns GET /v1/operations/{capability}/{operation}/schema. The
// schema is returned as decoded JSON rather than a fixed type: the Gateway
// serves whatever the manifest declared, and the CLI prints it unchanged.
// Numbers stay json.Number so a large integer does not lose digits.
func (c *Client) Schema(ctx context.Context, capability, operation string) (any, error) {
	var out any
	err := c.get(ctx, &out, "v1", "operations", capability, operation, "schema")
	return out, err
}

// Options returns
// GET /v1/operations/{capability}/{operation}/options/{field}.
func (c *Client) Options(ctx context.Context, capability, operation, field string) (Options, error) {
	var out Options
	err := c.get(ctx, &out, "v1", "operations", capability, operation, "options", field)
	normalize(&out.Options)
	return out, err
}

// get performs one GET request and decodes its JSON body into out.
func (c *Client) get(ctx context.Context, out any, segments ...string) error {
	endpoint := c.baseURL.JoinPath(segments...)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
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
			fmt.Sprintf("the response from %s could not be read: %v", endpoint.Redacted(), err), nil)
	}
	if len(body) > maxResponseBytes {
		return herrors.New(herrors.Internal,
			fmt.Sprintf("the response from %s is larger than %d bytes", endpoint.Redacted(), maxResponseBytes), nil)
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
	return nil
}

// wireError is the Gateway error object of docs/architecture.md section 14.
type wireError struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details"`
}

// errorFromResponse turns a non-2xx response into the shared error type.
// A body that is not the documented object maps to the Internal code but
// keeps the first part of the body so the message stays useful.
func errorFromResponse(status int, body []byte) error {
	var wire wireError
	if err := json.Unmarshal(body, &wire); err == nil && wire.Code != "" {
		code, ok := codesByWire[wire.Code]
		if !ok {
			message := wire.Message
			if message == "" {
				message = fmt.Sprintf("the Gateway answered HTTP %d", status)
			}
			return herrors.New(herrors.Internal,
				fmt.Sprintf("%s (unknown error code %q)", message, wire.Code), wire.Details)
		}
		message := wire.Message
		if message == "" {
			message = fmt.Sprintf("the Gateway answered HTTP %d", status)
		}
		return herrors.New(code, message, wire.Details)
	}

	snippet := strings.TrimSpace(string(body))
	const maxSnippet = 200
	if len(snippet) > maxSnippet {
		snippet = snippet[:maxSnippet] + "..."
	}
	if snippet == "" {
		snippet = http.StatusText(status)
	}
	return herrors.New(herrors.Internal,
		fmt.Sprintf("the Gateway answered HTTP %d: %s", status, snippet), nil)
}

// codesByWire maps the wire value of every shared code to the code.
var codesByWire = func() map[string]*herrors.Code {
	codes := herrors.All()
	out := make(map[string]*herrors.Code, len(codes))
	for _, code := range codes {
		out[code.Name()] = code
	}
	return out
}()

// normalize replaces a nil slice with an empty one, so a client always
// sees the same JSON type and the CLI prints [] rather than null.
func normalize[T any](items *[]T) {
	if *items == nil {
		*items = []T{}
	}
}
