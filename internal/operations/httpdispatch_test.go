package operations_test

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	herrors "github.com/achmadss/hanse/internal/errors"
	"github.com/achmadss/hanse/internal/operations"
	"github.com/achmadss/hanse/internal/registry"
	"github.com/achmadss/hanse/internal/runtime"
)

const (
	httpCredentialValue = "placeholder-credential-value"
	httpGatewayNode     = "node-a"
)

// httpManifest renders the fixture http capability. The probe operation
// overrides the handler URL, and caFile adds a relative CA file when set.
func httpManifest(url, probeURL, caFile string) string {
	ca := ""
	if caFile != "" {
		ca = "\n  ca_file: " + caFile
	}
	return fmt.Sprintf(`version: 1
id: remote
name: Remote
description: Remote service capability
contract: hanse.handler/v1

handler:
  kind: http
  url: %s%s
  auth:
    type: bearer
    credential: api_token

operations:
  ping:
    description: Ping the service
    arguments:
      type: object
      additionalProperties: false
  probe:
    description: Probe the service on another URL
    url: %s
    arguments:
      type: object
      additionalProperties: false
`, url, ca, probeURL)
}

// execManifest is a capability of the other handler kind, so the
// dispatcher rejects it.
const execManifest = `version: 1
id: local
name: Local
description: Local command capability
contract: hanse.handler/v1

handler:
  kind: exec
  entry:
    linux: [sh, handler.sh]

operations:
  run:
    description: Run locally
    arguments:
      type: object
`

// newHTTPDispatcherFixture loads the fixture capabilities and returns a
// dispatcher over them. When withCredential is false the credential file
// is missing.
func newHTTPDispatcherFixture(t *testing.T, url, probeURL, caFile, caPEM string, withCredential bool) *operations.HTTPDispatcher {
	t.Helper()
	dir := t.TempDir()
	capRoot := filepath.Join(dir, "capabilities")
	if err := os.MkdirAll(filepath.Join(capRoot, "remote"), 0o755); err != nil {
		t.Fatalf("create capability directory: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(capRoot, "local"), 0o755); err != nil {
		t.Fatalf("create capability directory: %v", err)
	}
	files := map[string]string{
		filepath.Join(capRoot, "remote", "manifest.yaml"): httpManifest(url, probeURL, caFile),
		filepath.Join(capRoot, "local", "manifest.yaml"):  execManifest,
	}
	if caFile != "" && caPEM != "" {
		files[filepath.Join(capRoot, "remote", caFile)] = caPEM
	}
	for path, content := range files {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	reg, warnings, err := registry.Load(registry.Options{
		Dirs: []string{capRoot},
		Input: registry.Input{
			GatewayHost: httpGatewayNode,
			Nodes: []registry.Node{
				{ID: httpGatewayNode, OS: "linux", Arch: "amd64", Enabled: true},
			},
			Routes: registry.Routes{Capabilities: map[string]registry.CapabilityRoute{
				"remote": {Executors: []string{httpGatewayNode}, DefaultExecutor: httpGatewayNode},
				"local":  {Executors: []string{httpGatewayNode}, DefaultExecutor: httpGatewayNode},
			}},
		},
	})
	if err != nil {
		t.Fatalf("load fixture: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("load fixture: unexpected warnings: %+v", warnings)
	}
	credDir := filepath.Join(dir, "credentials")
	if withCredential {
		if err := os.MkdirAll(filepath.Join(credDir, "remote"), 0o700); err != nil {
			t.Fatalf("create credentials directory: %v", err)
		}
		if err := os.WriteFile(filepath.Join(credDir, "remote", "api_token"),
			[]byte(httpCredentialValue+"\n"), 0o600); err != nil {
			t.Fatalf("write credential: %v", err)
		}
	}
	dispatcher, err := operations.NewHTTPDispatcher(operations.HTTPConfig{
		Registry:       reg,
		CredentialsDir: credDir,
		ResponseSize:   1 << 20,
	})
	if err != nil {
		t.Fatalf("build dispatcher: %v", err)
	}
	return dispatcher
}

// httpCall builds a request envelope for one fixture call.
func httpCall(capability, operation, kind string) *operations.Call {
	req := &runtime.Request{
		Contract:   runtime.ContractV1,
		Kind:       kind,
		ID:         "op_0123456789abcdef",
		Capability: capability,
		Operation:  operation,
		Arguments:  json.RawMessage(`{}`),
		Deadline:   time.Now().Add(10 * time.Second),
	}
	if kind == runtime.KindOptions {
		req.ID = "opt_0123456789abcdef"
		req.Field = "profile"
	}
	return &operations.Call{Request: req}
}

// received is one request the test server saw.
type received struct {
	method        string
	path          string
	authorization string
	callID        string
	body          []byte
}

// replyServer answers every request with status and body after sending
// what it saw on the returned channel.
func replyServer(t *testing.T, status int, body string) (*httptest.Server, chan received) {
	t.Helper()
	seen := make(chan received, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
		}
		seen <- received{
			method:        r.Method,
			path:          r.URL.Path,
			authorization: r.Header.Get("Authorization"),
			callID:        r.Header.Get("Hanse-Call-Id"),
			body:          data,
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, seen
}

func TestHTTPDispatcherPostsTheEnvelope(t *testing.T) {
	srv, seen := replyServer(t, http.StatusOK,
		`{"status":"ok","result":{"state":"online"},"summary":"node-c is online"}`)
	dispatcher := newHTTPDispatcherFixture(t, srv.URL+"/hanse/v1", srv.URL+"/probe", "", "", true)

	resp, err := dispatcher.Execute(context.Background(), httpCall("remote", "ping", runtime.KindOperation))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	req := <-seen
	if req.method != http.MethodPost {
		t.Errorf("method = %q, want POST", req.method)
	}
	if req.path != "/hanse/v1" {
		t.Errorf("path = %q, want /hanse/v1", req.path)
	}
	if req.authorization != "Bearer "+httpCredentialValue {
		t.Errorf("Authorization = %q, want the credential from the credentials directory", req.authorization)
	}
	if req.callID != "op_0123456789abcdef" {
		t.Errorf("Hanse-Call-Id = %q", req.callID)
	}
	if resp.Status != runtime.StatusOK {
		t.Errorf("status = %q, want ok", resp.Status)
	}
	if string(resp.Result) != `{"state":"online"}` {
		t.Errorf("result = %s", resp.Result)
	}
	if resp.Summary != "node-c is online" {
		t.Errorf("summary = %q", resp.Summary)
	}
}

func TestHTTPDispatcherUsesTheOperationURL(t *testing.T) {
	defaultSrv, defaultSeen := replyServer(t, http.StatusOK, `{"status":"ok","result":{"handler":"default"}}`)
	probeSrv, probeSeen := replyServer(t, http.StatusOK, `{"status":"ok","result":{"handler":"probe"}}`)
	dispatcher := newHTTPDispatcherFixture(t, defaultSrv.URL+"/hanse/v1", probeSrv.URL+"/probe", "", "", true)

	resp, err := dispatcher.Execute(context.Background(), httpCall("remote", "probe", runtime.KindOperation))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	req := <-probeSeen
	if req.path != "/probe" {
		t.Errorf("path = %q, want /probe", req.path)
	}
	if string(resp.Result) != `{"handler":"probe"}` {
		t.Errorf("result = %s, want the operation URL's handler", resp.Result)
	}
	select {
	case <-defaultSeen:
		t.Error("the dispatcher used the handler URL instead of the operation URL")
	default:
	}
}

func TestHTTPDispatcherRunsOptionProviders(t *testing.T) {
	body := `{"status":"ok","result":{"options":[{"value":"default","label":"Default profile"}]}}`
	srv, seen := replyServer(t, http.StatusOK, body)
	dispatcher := newHTTPDispatcherFixture(t, srv.URL, srv.URL, "", "", true)

	resp, err := dispatcher.Execute(context.Background(), httpCall("remote", "ping", runtime.KindOptions))
	if err != nil {
		t.Fatalf("execute options: %v", err)
	}
	req := <-seen
	var envelope struct {
		Kind  string `json:"kind"`
		Field string `json:"field"`
	}
	if err := json.Unmarshal(req.body, &envelope); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if envelope.Kind != runtime.KindOptions || envelope.Field != "profile" {
		t.Errorf("envelope kind = %q, field = %q, want options and profile", envelope.Kind, envelope.Field)
	}
	if resp.Status != runtime.StatusOK {
		t.Errorf("status = %q, want ok", resp.Status)
	}
	if string(resp.Result) != `{"options":[{"value":"default","label":"Default profile"}]}` {
		t.Errorf("result = %s", resp.Result)
	}
}

func TestHTTPDispatcherReturnsHandlerErrors(t *testing.T) {
	srv, _ := replyServer(t, http.StatusInternalServerError,
		`{"status":"error","error":{"code":"target_unreachable","message":"node-c did not answer"}}`)
	dispatcher := newHTTPDispatcherFixture(t, srv.URL, srv.URL, "", "", true)

	resp, err := dispatcher.Execute(context.Background(), httpCall("remote", "ping", runtime.KindOperation))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if resp.Status != runtime.StatusError || resp.Err == nil {
		t.Fatalf("response = %+v, want the handler's error", resp)
	}
	if resp.Err.Code != "target_unreachable" {
		t.Errorf("code = %q, want target_unreachable", resp.Err.Code)
	}
}

func TestHTTPDispatcherRejectsUnknownCalls(t *testing.T) {
	srv, _ := replyServer(t, http.StatusOK, `{"status":"ok"}`)
	dispatcher := newHTTPDispatcherFixture(t, srv.URL, srv.URL, "", "", true)

	if _, err := dispatcher.Execute(context.Background(), nil); !errors.Is(err, herrors.Internal) {
		t.Errorf("nil call error = %v, want internal", err)
	}
	if _, err := dispatcher.Execute(context.Background(), &operations.Call{}); !errors.Is(err, herrors.Internal) {
		t.Errorf("nil request error = %v, want internal", err)
	}
	tests := []struct {
		name string
		call *operations.Call
		want *herrors.Code
	}{
		{name: "unknown capability", call: httpCall("missing", "ping", runtime.KindOperation), want: herrors.UnknownCapability},
		{name: "unknown operation", call: httpCall("remote", "missing", runtime.KindOperation), want: herrors.UnknownOperation},
		{name: "exec capability", call: httpCall("local", "run", runtime.KindOperation), want: herrors.Internal},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := dispatcher.Execute(context.Background(), tc.call)
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %s", err, tc.want.Name())
			}
		})
	}
	if _, err := operations.NewHTTPDispatcher(operations.HTTPConfig{}); !errors.Is(err, herrors.Internal) {
		t.Errorf("missing registry error = %v, want internal", err)
	}
}

func TestHTTPDispatcherMissingCredentialFails(t *testing.T) {
	srv, _ := replyServer(t, http.StatusOK, `{"status":"ok"}`)
	dispatcher := newHTTPDispatcherFixture(t, srv.URL, srv.URL, "", "", false)

	_, err := dispatcher.Execute(context.Background(), httpCall("remote", "ping", runtime.KindOperation))
	if !errors.Is(err, herrors.HandlerFailed) {
		t.Fatalf("error = %v, want handler_failed", err)
	}
	if strings.Contains(err.Error(), httpCredentialValue) {
		t.Errorf("error %q holds a credential value", err)
	}
}

func TestHTTPDispatcherResolvesRelativeCAFile(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"status":"ok","result":{"tls":true}}`)
	}))
	defer srv.Close()

	caPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}))
	dispatcher := newHTTPDispatcherFixture(t, srv.URL, srv.URL, "ca.pem", caPEM, true)
	resp, err := dispatcher.Execute(context.Background(), httpCall("remote", "ping", runtime.KindOperation))
	if err != nil {
		t.Fatalf("execute over TLS with a manifest CA file: %v", err)
	}
	if resp.Status != runtime.StatusOK {
		t.Errorf("status = %q, want ok", resp.Status)
	}
}
