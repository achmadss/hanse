package operations

import (
	"context"
	"fmt"
	"path/filepath"

	herrors "github.com/achmadss/hanse/internal/errors"
	"github.com/achmadss/hanse/internal/registry"
	"github.com/achmadss/hanse/internal/runtime"
	"github.com/achmadss/hanse/internal/runtime/httprt"
)

// HTTPConfig is what an HTTPDispatcher needs.
type HTTPConfig struct {
	// Registry supplies the capability manifests: the handler URL,
	// authentication, and CA file of every http capability. Required.
	Registry *registry.Registry
	// CredentialsDir is gateway.yaml's paths.credentials_dir, where
	// credential references resolve to files (docs/configuration.md 8.1).
	CredentialsDir string
	// ResponseSize is gateway.yaml's limits.response_size; zero uses the
	// documented 1 MiB default.
	ResponseSize int64
}

// HTTPDispatcher runs http handler calls on the Gateway host, handler
// contract section 9. The Gateway makes the call itself, so the executor
// of an http operation is always the Gateway host node; routes.yaml
// already restricts an http capability to that node, section 5.2.
//
// The dispatcher covers operation runs and option provider calls: both
// use the same envelope and the same POST, and the runtime validates the
// options result when the envelope's kind is options.
type HTTPDispatcher struct {
	registry *registry.Registry
	runtime  *httprt.Runtime
}

// NewHTTPDispatcher builds a dispatcher over the loaded capabilities.
func NewHTTPDispatcher(cfg HTTPConfig) (*HTTPDispatcher, error) {
	if cfg.Registry == nil {
		return nil, herrors.New(herrors.Internal,
			"the http dispatcher needs a capability registry", nil)
	}
	return &HTTPDispatcher{
		registry: cfg.Registry,
		runtime: &httprt.Runtime{
			Credentials:   httprt.Credentials{Dir: cfg.CredentialsDir},
			ResponseLimit: cfg.ResponseSize,
		},
	}, nil
}

// Execute posts one call's request envelope to its http endpoint and
// returns the parsed response. A response with status error is the
// handler's error; a Go error means the call produced no protocol
// response. Elevation is ignored: an http operation cannot be privileged,
// docs/configuration.md section 6.2.
func (d *HTTPDispatcher) Execute(ctx context.Context, call *Call) (runtime.Response, error) {
	if call == nil || call.Request == nil {
		return runtime.Response{}, herrors.New(herrors.Internal,
			"the http dispatcher received no request envelope", nil)
	}
	if d == nil || d.registry == nil || d.runtime == nil {
		return runtime.Response{}, herrors.New(herrors.Internal,
			"the http dispatcher is not configured", nil)
	}
	req := call.Request
	capability, ok := d.registry.Capability(req.Capability)
	if !ok {
		return runtime.Response{}, herrors.New(herrors.UnknownCapability,
			fmt.Sprintf("unknown capability %q", req.Capability), nil)
	}
	operation, ok := capability.Operation(req.Operation)
	if !ok {
		return runtime.Response{}, herrors.New(herrors.UnknownOperation,
			fmt.Sprintf("capability %q has no operation %q", req.Capability, req.Operation), nil)
	}
	if capability.Manifest == nil || capability.Manifest.Handler.Kind != registry.KindHTTP {
		return runtime.Response{}, herrors.New(herrors.Internal,
			fmt.Sprintf("capability %q is not an http capability", req.Capability), nil)
	}
	handler := httprt.Handler{
		URL:    operation.URL,
		CAFile: caFilePath(capability),
	}
	if auth := capability.Manifest.Handler.Auth; auth != nil {
		handler.Auth = httprt.Auth{
			Type:       auth.Type,
			Header:     auth.Header,
			Credential: auth.Credential,
		}
	}
	return d.runtime.Run(ctx, handler, req)
}

// caFilePath returns the capability's CA file with a relative path
// resolved from the capability directory, as every manifest path does
// (docs/configuration.md section 2).
func caFilePath(capability *registry.Capability) string {
	ca := capability.Manifest.Handler.CAFile
	if ca == "" || filepath.IsAbs(ca) {
		return ca
	}
	return filepath.Join(capability.Dir, ca)
}
