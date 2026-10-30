package httprt

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"time"

	"github.com/achmadss/hanse/internal/config"
	herrors "github.com/achmadss/hanse/internal/errors"
	"github.com/achmadss/hanse/internal/runtime"
)

// Authentication types of an http handler, docs/configuration.md section
// 6.1. An empty type means the endpoint takes no credential.
const (
	// AuthNone is an endpoint that needs no credential.
	AuthNone = ""
	// AuthBearer sends the credential as `Authorization: Bearer <value>`.
	AuthBearer = "bearer"
	// AuthHeader sends the credential in the header named by Auth.Header.
	AuthHeader = "header"
)

// DefaultResponseLimit is the documented default size of one handler
// response, 1 MiB, docs/handler-contract.md section 11.
const DefaultResponseLimit int64 = 1 << 20

// unsafeGroupOther is the permission mask that makes a credential file
// readable by users other than its owner, docs/configuration.md section
// 8.1.
const unsafeGroupOther = 0o044

// Auth is how the Gateway authenticates to one http handler. Credential is
// the credential reference from the manifest, never the value; the value
// comes from Credentials.
type Auth struct {
	// Type is AuthNone, AuthBearer, or AuthHeader.
	Type string
	// Header is the header name for AuthHeader.
	Header string
	// Credential is the reference of the credential file.
	Credential string
}

// Handler is one resolved http endpoint: the effective URL of the call,
// its authentication, and an optional extra CA certificate.
type Handler struct {
	// URL is the endpoint the request envelope is posted to.
	URL string
	// Auth is the endpoint's authentication, docs/configuration.md 6.1.
	Auth Auth
	// CAFile is an extra CA certificate to trust, in PEM format. The
	// system roots always stay trusted, and verification always runs.
	CAFile string
}

// Runtime sends request envelopes to http handlers.
type Runtime struct {
	// Credentials resolves and reads credential references.
	Credentials Credentials
	// ResponseLimit is the most bytes of a response body that the
	// runtime reads; zero or less means DefaultResponseLimit.
	ResponseLimit int64
	// OptionsTimeout is the timeout of an option provider call whose
	// envelope carries no deadline; zero or less means the documented
	// default of 10 seconds, docs/configuration.md section 3.1.
	OptionsTimeout time.Duration
}

// Run performs one http handler call: it posts the request envelope and
// parses the response envelope, docs/handler-contract.md section 9. The
// context carries the call's cancellation. The envelope deadline bounds
// the call; an option provider call without one gets the documented
// default timeout, and an operation call without one is refused.
//
// The endpoint must use https unless its host is a loopback address,
// because the Gateway always verifies the server's certificate.
//
// A returned response with status error is the handler's own error. A Go
// error means the call produced no protocol response: an unreachable
// endpoint, a timeout, a cancellation, an unexpected HTTP status, or a
// malformed body.
func (r *Runtime) Run(ctx context.Context, handler Handler, req *runtime.Request) (runtime.Response, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	body, err := encodeRequest(req)
	if err != nil {
		return runtime.Response{}, err
	}
	endpoint := strings.TrimSpace(handler.URL)
	if endpoint == "" {
		return runtime.Response{}, herrors.New(herrors.Internal,
			"the http handler call has no URL", nil)
	}
	if err := checkEndpoint(endpoint); err != nil {
		return runtime.Response{}, err
	}
	credential, err := r.credential(req.Capability, handler.Auth)
	if err != nil {
		return runtime.Response{}, err
	}
	transport, err := transportFor(handler.CAFile)
	if err != nil {
		return runtime.Response{}, err
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport: transport,
		// The Gateway does not follow redirects, section 9.1. The 3xx
		// response goes through the status mapping below instead.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	ctx, cancel, err := r.callContext(ctx, req)
	if err != nil {
		return runtime.Response{}, err
	}
	defer cancel()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return runtime.Response{}, herrors.Redact(herrors.New(herrors.HandlerFailed,
			fmt.Sprintf("the http handler URL is not usable: %s", err), nil), credential)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Hanse-Contract", runtime.ContractV1)
	httpReq.Header.Set("Hanse-Call-Id", req.ID)
	if err := applyAuth(httpReq, handler.Auth, credential); err != nil {
		return runtime.Response{}, err
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return runtime.Response{}, callError(ctx, err, credential)
	}
	defer func() { _ = resp.Body.Close() }()
	limit := r.ResponseLimit
	if limit <= 0 {
		limit = DefaultResponseLimit
	}
	payload, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return runtime.Response{}, callError(ctx, err, credential)
	}
	if int64(len(payload)) > limit {
		return runtime.Response{}, herrors.Redact(herrors.New(herrors.HandlerOutputTooLarge,
			fmt.Sprintf("the http handler response exceeded the %d byte limit", limit),
			map[string]any{"limit": limit}), credential)
	}
	if resp.StatusCode != http.StatusOK {
		parsed, parseErr := decodeResponse(payload)
		if parseErr == nil && parsed.Status == runtime.StatusError {
			return parsed, nil
		}
		return runtime.Response{}, statusError(resp.StatusCode, credential)
	}
	parsed, err := decodeResponse(payload)
	if err != nil {
		return runtime.Response{}, herrors.Redact(err, credential)
	}
	if req.Kind == runtime.KindOptions && parsed.Status == runtime.StatusOK {
		if problem := optionsProblem(parsed.Result); problem != "" {
			return runtime.Response{}, herrors.Redact(invalidOutput(
				"the http handler returned an invalid options result: "+problem), credential)
		}
	}
	return parsed, nil
}

// callContext applies the call's timeout to the request context, handler
// contract section 9.3. An operation call always carries a deadline: the
// Gateway sets it from the operation's timeout. An option provider call
// may leave it out, and then the documented 10 second default applies,
// section 11.
func (r *Runtime) callContext(parent context.Context, req *runtime.Request) (context.Context, context.CancelFunc, error) {
	if req.Kind == runtime.KindOptions {
		if req.Deadline.IsZero() {
			timeout := r.OptionsTimeout
			if timeout <= 0 {
				timeout = config.DefaultTimeouts().Options
			}
			ctx, cancel := context.WithTimeout(parent, timeout)
			return ctx, cancel, nil
		}
		ctx, cancel := context.WithDeadline(parent, req.Deadline)
		return ctx, cancel, nil
	}
	if req.Deadline.IsZero() {
		return nil, nil, herrors.New(herrors.Internal,
			"an operation call needs a deadline", nil)
	}
	ctx, cancel := context.WithDeadline(parent, req.Deadline)
	return ctx, cancel, nil
}

// checkEndpoint applies the TLS rule of handler contract section 9.1: the
// Gateway always verifies the server's certificate, so a plain http URL is
// allowed only on a loopback address. This is the same rule that
// gateway.yaml's public_url and agent.yaml's gateway_url follow,
// docs/configuration.md sections 3.2 and 7.1. The error names the host,
// never the credential or the URL's user information.
func checkEndpoint(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return herrors.New(herrors.HandlerFailed,
			"the http handler URL is not a usable URL with a host", nil)
	}
	switch strings.ToLower(parsed.Scheme) {
	case "https":
		return nil
	case "http":
		if config.IsLoopbackHost(parsed.Hostname()) {
			return nil
		}
		return herrors.New(herrors.HandlerFailed,
			fmt.Sprintf("the http handler URL host %q must use https; plain http is allowed only on a loopback address",
				parsed.Hostname()), nil)
	default:
		return herrors.New(herrors.HandlerFailed,
			fmt.Sprintf("the http handler URL scheme %q must be https, or http on a loopback address",
				parsed.Scheme), nil)
	}
}

// credential loads the credential of one call, or "" when the endpoint
// takes none.
func (r *Runtime) credential(capability string, auth Auth) (string, error) {
	switch auth.Type {
	case AuthNone:
		if auth.Credential != "" {
			return "", herrors.New(herrors.Internal,
				"the http handler has a credential but no authentication type", nil)
		}
		return "", nil
	case AuthBearer, AuthHeader:
		if auth.Credential == "" {
			return "", herrors.New(herrors.Internal,
				fmt.Sprintf("the http handler's %s authentication has no credential reference", auth.Type), nil)
		}
		return r.Credentials.Load(capability, auth.Credential)
	default:
		return "", herrors.New(herrors.Internal,
			fmt.Sprintf("the http handler authentication type %q is not %q or %q",
				auth.Type, AuthBearer, AuthHeader), nil)
	}
}

// applyAuth adds the credential to the request headers. A header value
// that net/http rejects fails the call, never the credential check.
func applyAuth(req *http.Request, auth Auth, credential string) error {
	switch auth.Type {
	case AuthNone:
		return nil
	case AuthBearer:
		req.Header.Set("Authorization", "Bearer "+credential)
		return nil
	case AuthHeader:
		if auth.Header == "" {
			return herrors.New(herrors.Internal,
				"the http handler's header authentication has no header name", nil)
		}
		req.Header.Set(auth.Header, credential)
		return nil
	default:
		return herrors.New(herrors.Internal,
			fmt.Sprintf("the http handler authentication type %q is unknown", auth.Type), nil)
	}
}

// transportFor builds the HTTP transport of one call. TLS verification is
// on unless the extra CA file is broken; the CA file only adds a trusted
// root, it never disables verification.
func transportFor(caFile string) (*http.Transport, error) {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		base = &http.Transport{}
	}
	transport := base.Clone()
	config := &tls.Config{MinVersion: tls.VersionTLS12}
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, herrors.New(herrors.HandlerFailed,
				fmt.Sprintf("the http handler CA file %q cannot be read: %s", caFile, err), nil)
		}
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, herrors.New(herrors.HandlerFailed,
				fmt.Sprintf("the http handler CA file %q holds no PEM certificate", caFile), nil)
		}
		config.RootCAs = pool
	}
	transport.TLSClientConfig = config
	return transport, nil
}

// statusError maps a response whose status is not 200 to the handler
// contract's http_status code, section 9.2, with the status in the
// details. A valid error envelope never reaches it: that is the handler's
// own error.
func statusError(status int, credential string) error {
	return herrors.Redact(herrors.New(herrors.HandlerHTTPStatus,
		fmt.Sprintf("the http handler returned HTTP status %d without a valid error envelope", status),
		map[string]any{"status_code": status}), credential)
}

// callError maps a transport failure to the shared codes. A deadline that
// expired and a cancellation keep their own codes; everything else is a
// handler failure. The credential is redacted from the message.
func callError(ctx context.Context, err error, credential string) error {
	var mapped *herrors.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.Is(ctx.Err(), context.DeadlineExceeded):
		mapped = herrors.New(herrors.Timeout,
			"the http handler did not answer before the deadline", nil)
	case errors.Is(err, context.Canceled), errors.Is(ctx.Err(), context.Canceled):
		mapped = herrors.New(herrors.Cancelled,
			"the http handler call was cancelled", nil)
	default:
		mapped = herrors.New(herrors.HandlerFailed,
			fmt.Sprintf("the http handler call failed: %s", err), nil)
	}
	return herrors.Redact(mapped, credential)
}

// Credentials reads credential files. A reference resolves to
// Dir/<capability>/<name>, docs/configuration.md section 8.1.
type Credentials struct {
	// Dir is gateway.yaml's paths.credentials_dir. Empty means no
	// credential can be read.
	Dir string
}

// Load reads one credential and removes one trailing newline, docs
// /configuration.md section 8.1. Every error names the credential, never
// its value, and a value never appears in one.
func (c Credentials) Load(capability, name string) (string, error) {
	if name == "" || name == "." || name == ".." ||
		strings.ContainsAny(name, `/\`) || strings.Contains(name, ":") {
		return "", herrors.New(herrors.HandlerFailed,
			fmt.Sprintf("credential %q is not a plain credential name", name), nil)
	}
	if c.Dir == "" {
		return "", herrors.New(herrors.HandlerFailed,
			fmt.Sprintf("credential %q cannot be read: the credentials directory is not configured", name), nil)
	}
	if capability == "" {
		return "", herrors.New(herrors.HandlerFailed,
			fmt.Sprintf("credential %q cannot be read without a capability", name), nil)
	}
	path := filepath.Join(c.Dir, capability, name)
	info, err := os.Stat(path)
	if err != nil {
		return "", herrors.New(herrors.HandlerFailed,
			fmt.Sprintf("credential %q of capability %q cannot be read", name, capability), nil)
	}
	if !info.Mode().IsRegular() {
		return "", herrors.New(herrors.HandlerFailed,
			fmt.Sprintf("credential %q of capability %q is not a regular file", name, capability), nil)
	}
	if goruntime.GOOS != "windows" && info.Mode().Perm()&unsafeGroupOther != 0 {
		return "", herrors.New(herrors.HandlerFailed,
			fmt.Sprintf("credential %q of capability %q is readable by users other than its owner", name, capability), nil)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", herrors.New(herrors.HandlerFailed,
			fmt.Sprintf("credential %q of capability %q cannot be read", name, capability), nil)
	}
	value := string(data)
	if strings.HasSuffix(value, "\r\n") {
		value = value[:len(value)-2]
	} else {
		value = strings.TrimSuffix(value, "\n")
	}
	return value, nil
}
