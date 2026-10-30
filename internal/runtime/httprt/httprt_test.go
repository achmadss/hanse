package httprt_test

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
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	herrors "github.com/achmadss/hanse/internal/errors"
	"github.com/achmadss/hanse/internal/runtime"
	"github.com/achmadss/hanse/internal/runtime/httprt"
)

const (
	credentialValue = "placeholder-credential-value"
	operationID     = "op_0123456789abcdef"
	optionsID       = "opt_0123456789abcdef"
)

// newRequest builds a complete request envelope of the fixture
// capability. It uses only documentation placeholder values.
func newRequest(kind string) *runtime.Request {
	req := &runtime.Request{
		Contract:   runtime.ContractV1,
		Kind:       kind,
		ID:         operationID,
		Capability: "remote",
		Operation:  "ping",
		Arguments:  json.RawMessage(`{"node":"node-c","nested":{"list":[1,"two"]}}`),
		Nodes: map[string]runtime.Node{
			"node-c": {
				ID:         "node-c",
				Name:       "Workstation",
				OS:         "windows",
				Arch:       "amd64",
				Address:    "192.0.2.20",
				Attributes: map[string]any{"mac": "00:00:5e:00:53:01"},
			},
		},
		Executor: runtime.Node{
			ID:         "node-a",
			Name:       "Home server",
			OS:         "linux",
			Arch:       "arm64",
			Address:    "192.0.2.10",
			Attributes: map[string]any{},
		},
		Secrets:  map[string]string{"api_token": "secret-in-envelope"},
		Deadline: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	if kind == runtime.KindOptions {
		req.ID = optionsID
		req.Field = "profile"
	}
	return req
}

// writeCredential writes one credential file with safe permissions.
func writeCredential(t *testing.T, dir, capability, name, value string) {
	t.Helper()
	capDir := filepath.Join(dir, capability)
	if err := os.MkdirAll(capDir, 0o700); err != nil {
		t.Fatalf("create credential directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(capDir, name), []byte(value), 0o600); err != nil {
		t.Fatalf("write credential: %v", err)
	}
}

// newRuntime returns a runtime whose credentials live in dir.
func newRuntime(dir string) *httprt.Runtime {
	return &httprt.Runtime{Credentials: httprt.Credentials{Dir: dir}}
}

// captured is one request the test server received.
type captured struct {
	method        string
	path          string
	contentType   string
	contract      string
	callID        string
	authorization string
	headerValue   string
	body          []byte
}

// captureServer answers every request with body after reporting it on the
// returned channel. It reads the whole request body first, so the test can
// inspect the envelope.
func captureServer(t *testing.T, status int, body string) (*httptest.Server, chan captured) {
	t.Helper()
	ch := make(chan captured, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
		}
		ch <- captured{
			method:        r.Method,
			path:          r.URL.Path,
			contentType:   r.Header.Get("Content-Type"),
			contract:      r.Header.Get("Hanse-Contract"),
			callID:        r.Header.Get("Hanse-Call-Id"),
			authorization: r.Header.Get("Authorization"),
			headerValue:   r.Header.Get("X-Api-Key"),
			body:          data,
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, ch
}

func TestRunPostsTheRequestEnvelope(t *testing.T) {
	srv, requests := captureServer(t, http.StatusOK,
		`{"contract":"hanse.handler/v1","status":"ok","result":{"state":"online"},"summary":"node-c is online","future_field":true}`)
	credDir := t.TempDir()
	writeCredential(t, credDir, "remote", "api_token", credentialValue+"\n")

	resp, err := newRuntime(credDir).Run(context.Background(), httprt.Handler{
		URL:  srv.URL + "/hanse/v1",
		Auth: httprt.Auth{Type: httprt.AuthBearer, Credential: "api_token"},
	}, newRequest(runtime.KindOperation))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	got := <-requests
	if got.method != http.MethodPost {
		t.Errorf("method = %q, want POST", got.method)
	}
	if got.path != "/hanse/v1" {
		t.Errorf("path = %q, want /hanse/v1", got.path)
	}
	if got.contentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got.contentType)
	}
	if got.contract != runtime.ContractV1 {
		t.Errorf("Hanse-Contract = %q, want %q", got.contract, runtime.ContractV1)
	}
	if got.callID != operationID {
		t.Errorf("Hanse-Call-Id = %q, want %q", got.callID, operationID)
	}
	if got.authorization != "Bearer "+credentialValue {
		t.Errorf("Authorization = %q, want the bearer credential", got.authorization)
	}

	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(got.body, &envelope); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	want := map[string]string{
		"contract":   `"` + runtime.ContractV1 + `"`,
		"kind":       `"operation"`,
		"id":         `"` + operationID + `"`,
		"capability": `"remote"`,
		"operation":  `"ping"`,
		"arguments":  `{"node":"node-c","nested":{"list":[1,"two"]}}`,
		"secrets":    `{"api_token":"secret-in-envelope"}`,
		"deadline":   `"2030-01-01T00:00:00Z"`,
	}
	for name, value := range want {
		if string(envelope[name]) != value {
			t.Errorf("envelope %s = %s, want %s", name, envelope[name], value)
		}
	}
	if _, ok := envelope["field"]; ok {
		t.Error("an operation envelope must not carry a field")
	}
	executor := string(envelope["executor"])
	for _, fragment := range []string{`"id":"node-a"`, `"name":"Home server"`, `"os":"linux"`, `"arch":"arm64"`, `"address":"192.0.2.10"`} {
		if !strings.Contains(executor, fragment) {
			t.Errorf("executor %s is missing %s", executor, fragment)
		}
	}
	nodes := string(envelope["nodes"])
	if !strings.Contains(nodes, `"node-c"`) || !strings.Contains(nodes, `"mac":"00:00:5e:00:53:01"`) {
		t.Errorf("nodes = %s, want the node object of node-c", nodes)
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
	if resp.Err != nil {
		t.Errorf("error = %+v, want none", resp.Err)
	}
}

func TestRunSendsHeaderCredential(t *testing.T) {
	srv, requests := captureServer(t, http.StatusOK, `{"status":"ok"}`)
	credDir := t.TempDir()
	writeCredential(t, credDir, "remote", "api_token", credentialValue+"\n")

	_, err := newRuntime(credDir).Run(context.Background(), httprt.Handler{
		URL:  srv.URL,
		Auth: httprt.Auth{Type: httprt.AuthHeader, Header: "X-Api-Key", Credential: "api_token"},
	}, newRequest(runtime.KindOperation))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	got := <-requests
	if got.headerValue != credentialValue {
		t.Errorf("X-Api-Key = %q, want the credential with one trailing newline removed", got.headerValue)
	}
	if got.authorization != "" {
		t.Errorf("Authorization = %q, want none for header authentication", got.authorization)
	}
}

func TestRunWithoutAuthSendsNoCredential(t *testing.T) {
	srv, requests := captureServer(t, http.StatusOK, `{"status":"ok"}`)
	if _, err := newRuntime(t.TempDir()).Run(context.Background(),
		httprt.Handler{URL: srv.URL}, newRequest(runtime.KindOperation)); err != nil {
		t.Fatalf("run: %v", err)
	}
	got := <-requests
	if got.authorization != "" || got.headerValue != "" {
		t.Errorf("authorization = %q, X-Api-Key = %q, want neither", got.authorization, got.headerValue)
	}
}

func TestRunVerifiesTLSCertificatesByDefault(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	}))
	defer srv.Close()

	_, err := newRuntime(t.TempDir()).Run(context.Background(),
		httprt.Handler{URL: srv.URL}, newRequest(runtime.KindOperation))
	if err == nil {
		t.Fatal("run over a TLS server with an unknown certificate succeeded, want a verification failure")
	}
	if !errors.Is(err, herrors.HandlerFailed) {
		t.Errorf("error = %v, want the handler_failed code", err)
	}
	if !strings.Contains(err.Error(), "certificate") {
		t.Errorf("error = %v, want a certificate verification failure", err)
	}
}

func TestRunTrustsTheConfiguredCAFile(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"status":"ok","result":{"tls":true}}`)
	}))
	defer srv.Close()

	caFile := filepath.Join(t.TempDir(), "ca.pem")
	certificate := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	if err := os.WriteFile(caFile, certificate, 0o644); err != nil {
		t.Fatalf("write CA file: %v", err)
	}
	resp, err := newRuntime(t.TempDir()).Run(context.Background(),
		httprt.Handler{URL: srv.URL, CAFile: caFile}, newRequest(runtime.KindOperation))
	if err != nil {
		t.Fatalf("run with the test CA: %v", err)
	}
	if resp.Status != runtime.StatusOK {
		t.Errorf("status = %q, want ok", resp.Status)
	}
}

func TestRunEnforcesTheDeadline(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		time.Sleep(2 * time.Second)
	}))
	defer srv.Close()

	req := newRequest(runtime.KindOperation)
	req.Deadline = time.Now().Add(80 * time.Millisecond)
	start := time.Now()
	_, err := newRuntime(t.TempDir()).Run(context.Background(),
		httprt.Handler{URL: srv.URL}, req)
	elapsed := time.Since(start)
	if !errors.Is(err, herrors.Timeout) {
		t.Fatalf("error = %v, want the timeout code", err)
	}
	if elapsed > 5*time.Second {
		t.Errorf("the call took %v, want a prompt timeout", elapsed)
	}
}

func TestRunStopsOnContextCancellation(t *testing.T) {
	started := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		started <- struct{}{}
		time.Sleep(2 * time.Second)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-started
		cancel()
	}()
	req := newRequest(runtime.KindOperation)
	req.Deadline = time.Now().Add(10 * time.Second)
	_, err := newRuntime(t.TempDir()).Run(ctx, httprt.Handler{URL: srv.URL}, req)
	if !errors.Is(err, herrors.Cancelled) {
		t.Fatalf("error = %v, want the cancelled code", err)
	}
}

func TestRunMapsNon200Statuses(t *testing.T) {
	const handlerError = `{"status":"error","error":{"code":"target_unreachable","message":"node-c did not answer","details":{"waited_seconds":120}}}`
	tests := []struct {
		name       string
		status     int
		body       string
		wantCode   *herrors.Code
		wantStatus int
	}{
		{name: "error envelope", status: http.StatusInternalServerError, body: handlerError},
		{name: "ok envelope", status: http.StatusInternalServerError, body: `{"status":"ok"}`,
			wantCode: herrors.HandlerHTTPStatus, wantStatus: http.StatusInternalServerError},
		{name: "plain text", status: http.StatusBadGateway, body: "bad gateway",
			wantCode: herrors.HandlerHTTPStatus, wantStatus: http.StatusBadGateway},
		{name: "empty body", status: http.StatusServiceUnavailable, body: "",
			wantCode: herrors.HandlerHTTPStatus, wantStatus: http.StatusServiceUnavailable},
		{name: "unexpected success status", status: http.StatusCreated, body: `{"status":"ok"}`,
			wantCode: herrors.HandlerHTTPStatus, wantStatus: http.StatusCreated},
		{name: "not modified", status: http.StatusNotModified, body: `{"status":"ok"}`,
			wantCode: herrors.HandlerHTTPStatus, wantStatus: http.StatusNotModified},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := captureServer(t, tc.status, tc.body)
			resp, err := newRuntime(t.TempDir()).Run(context.Background(),
				httprt.Handler{URL: srv.URL}, newRequest(runtime.KindOperation))
			if tc.wantCode == nil {
				if err != nil {
					t.Fatalf("run: %v", err)
				}
				if resp.Status != runtime.StatusError || resp.Err == nil {
					t.Fatalf("response = %+v, want the handler's error", resp)
				}
				if resp.Err.Code != "target_unreachable" {
					t.Errorf("code = %q, want target_unreachable", resp.Err.Code)
				}
				if resp.Err.Message != "node-c did not answer" {
					t.Errorf("message = %q", resp.Err.Message)
				}
				if resp.Err.Details["waited_seconds"] != float64(120) {
					t.Errorf("details = %v, want waited_seconds 120", resp.Err.Details)
				}
				return
			}
			if !errors.Is(err, tc.wantCode) {
				t.Fatalf("error = %v, want %s", err, tc.wantCode.Name())
			}
			details := herrors.From(err).Details
			if details["status_code"] != tc.wantStatus {
				t.Errorf("status_code = %v, want %d", details["status_code"], tc.wantStatus)
			}
		})
	}
}

func TestRunRejectsMalformedResponseBodies(t *testing.T) {
	bodies := []string{
		"",
		"not json",
		`{"status":"ok"} {"status":"ok"}`,
		`{"status":"ok","result":[]}`,
		`{"status":"ok","result":"text"}`,
		`{"status":"nope"}`,
		`{"status":"error"}`,
		`{"status":"error","error":{"message":"no code"}}`,
		`{"status":"error","error":{"code":"BAD","message":"c"}}`,
		`{"status":"error","error":{"code":"toolongcode` + strings.Repeat("a", 64) + `","message":"c"}}`,
		`{"contract":"hanse.handler/v2","status":"ok"}`,
		`{"status":"ok","error":{"code":"x","message":"y"}}`,
		`{"status":"ok","summary":5}`,
		`[{"status":"ok"}]`,
	}
	for _, body := range bodies {
		t.Run(fmt.Sprintf("%.40q", body), func(t *testing.T) {
			srv, _ := captureServer(t, http.StatusOK, body)
			_, err := newRuntime(t.TempDir()).Run(context.Background(),
				httprt.Handler{URL: srv.URL}, newRequest(runtime.KindOperation))
			if !errors.Is(err, herrors.HandlerOutputInvalid) {
				t.Fatalf("body %q: error = %v, want handler_output_invalid", body, err)
			}
		})
	}
}

func TestRunAcceptsCancelledResponse(t *testing.T) {
	srv, _ := captureServer(t, http.StatusOK,
		`{"status":"cancelled","result":{"counted":3},"summary":"stopped"}`)
	resp, err := newRuntime(t.TempDir()).Run(context.Background(),
		httprt.Handler{URL: srv.URL}, newRequest(runtime.KindOperation))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if resp.Status != runtime.StatusCancelled {
		t.Errorf("status = %q, want cancelled", resp.Status)
	}
	if string(resp.Result) != `{"counted":3}` {
		t.Errorf("result = %s", resp.Result)
	}
}

func TestRunRejectsOversizedResponseBody(t *testing.T) {
	body := `{"status":"ok","result":{"pad":"` + strings.Repeat("x", 200) + `"}}`
	srv, _ := captureServer(t, http.StatusOK, body)
	rt := newRuntime(t.TempDir())
	rt.ResponseLimit = 64
	_, err := rt.Run(context.Background(), httprt.Handler{URL: srv.URL}, newRequest(runtime.KindOperation))
	if !errors.Is(err, herrors.HandlerOutputInvalid) {
		t.Fatalf("error = %v, want handler_output_invalid", err)
	}
	if herrors.From(err).Details["limit"] != int64(64) {
		t.Errorf("details = %v, want the limit", herrors.From(err).Details)
	}
}

func TestRunDoesNotFollowRedirects(t *testing.T) {
	var reachedTarget bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reachedTarget = true
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	}))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer redirect.Close()

	_, err := newRuntime(t.TempDir()).Run(context.Background(),
		httprt.Handler{URL: redirect.URL}, newRequest(runtime.KindOperation))
	if !errors.Is(err, herrors.HandlerHTTPStatus) {
		t.Fatalf("error = %v, want http_status", err)
	}
	if herrors.From(err).Details["status_code"] != http.StatusFound {
		t.Errorf("details = %v, want status_code 302", herrors.From(err).Details)
	}
	if reachedTarget {
		t.Error("the runtime followed the redirect")
	}
}

func TestRunSendsAndValidatesOptions(t *testing.T) {
	valid := `{"status":"ok","result":{"options":[{"value":"default","label":"Default profile"},{"value":"fast","description":"Lower quality"}]}}`
	srv, requests := captureServer(t, http.StatusOK, valid)
	resp, err := newRuntime(t.TempDir()).Run(context.Background(),
		httprt.Handler{URL: srv.URL}, newRequest(runtime.KindOptions))
	if err != nil {
		t.Fatalf("run options: %v", err)
	}
	if resp.Status != runtime.StatusOK {
		t.Errorf("status = %q, want ok", resp.Status)
	}
	got := <-requests

	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(got.body, &envelope); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if string(envelope["kind"]) != `"options"` {
		t.Errorf("kind = %s, want options", envelope["kind"])
	}
	if string(envelope["id"]) != `"`+optionsID+`"` {
		t.Errorf("id = %s, want the opt_ ID", envelope["id"])
	}
	if string(envelope["field"]) != `"profile"` {
		t.Errorf("field = %s, want profile", envelope["field"])
	}

	invalid := []string{
		`{"status":"ok"}`,
		`{"status":"ok","result":null}`,
		`{"status":"ok","result":{"options":null}}`,
		`{"status":"ok","result":{"options":{}}}`,
		`{"status":"ok","result":{"options":[{"label":"no value"}]}}`,
		`{"status":"ok","result":{"options":[{"value":1,"label":5}]}}`,
	}
	for _, body := range invalid {
		srv, _ := captureServer(t, http.StatusOK, body)
		_, err := newRuntime(t.TempDir()).Run(context.Background(),
			httprt.Handler{URL: srv.URL}, newRequest(runtime.KindOptions))
		if !errors.Is(err, herrors.HandlerOutputInvalid) {
			t.Errorf("body %q: error = %v, want handler_output_invalid", body, err)
		}
	}
}

func TestRunRejectsBrokenRequestEnvelopes(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*runtime.Request)
		want   *herrors.Code
	}{
		{name: "wrong contract", mutate: func(r *runtime.Request) { r.Contract = "hanse.handler/v2" }, want: herrors.UnsupportedContract},
		{name: "unknown kind", mutate: func(r *runtime.Request) { r.Kind = "batch" }, want: herrors.Internal},
		{name: "options without field", mutate: func(r *runtime.Request) { r.Kind = runtime.KindOptions }, want: herrors.Internal},
		{name: "missing id", mutate: func(r *runtime.Request) { r.ID = "" }, want: herrors.Internal},
		{name: "missing capability", mutate: func(r *runtime.Request) { r.Capability = "" }, want: herrors.Internal},
		{name: "array arguments", mutate: func(r *runtime.Request) { r.Arguments = json.RawMessage(`[]`) }, want: herrors.Internal},
		{name: "invalid argument json", mutate: func(r *runtime.Request) { r.Arguments = json.RawMessage(`{`) }, want: herrors.Internal},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv, requests := captureServer(t, http.StatusOK, `{"status":"ok"}`)
			req := newRequest(runtime.KindOperation)
			tc.mutate(req)
			_, err := newRuntime(t.TempDir()).Run(context.Background(),
				httprt.Handler{URL: srv.URL}, req)
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %s", err, tc.want.Name())
			}
			select {
			case <-requests:
				t.Error("the runtime sent a malformed envelope")
			default:
			}
		})
	}
	if _, err := newRuntime(t.TempDir()).Run(context.Background(), httprt.Handler{URL: "https://192.0.2.1/"}, nil); !errors.Is(err, herrors.Internal) {
		t.Errorf("nil request error = %v, want internal", err)
	}
	if _, err := newRuntime(t.TempDir()).Run(context.Background(), httprt.Handler{}, newRequest(runtime.KindOperation)); !errors.Is(err, herrors.Internal) {
		t.Errorf("empty URL error = %v, want internal", err)
	}
}

func TestCredentialErrorsNeverContainTheValue(t *testing.T) {
	credDir := t.TempDir()
	writeCredential(t, credDir, "remote", "api_token", credentialValue)
	auth := httprt.Auth{Type: httprt.AuthBearer, Credential: "api_token"}

	closed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	closedURL := closed.URL
	closed.Close()
	tlsServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	}))
	defer tlsServer.Close()
	badStatus, _ := captureServer(t, http.StatusInternalServerError, "handler exploded")
	malformed, _ := captureServer(t, http.StatusOK, `{"status":"ok","result":[]}`)

	tests := []struct {
		name    string
		handler httprt.Handler
	}{
		{name: "connection refused", handler: httprt.Handler{URL: closedURL, Auth: auth}},
		{name: "untrusted certificate", handler: httprt.Handler{URL: tlsServer.URL, Auth: auth}},
		{name: "unexpected status", handler: httprt.Handler{URL: badStatus.URL, Auth: auth}},
		{name: "malformed body", handler: httprt.Handler{URL: malformed.URL, Auth: auth}},
		{name: "missing CA file", handler: httprt.Handler{URL: badStatus.URL, Auth: auth, CAFile: filepath.Join(t.TempDir(), "absent.pem")}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := newRuntime(credDir).Run(context.Background(), tc.handler, newRequest(runtime.KindOperation))
			if err == nil {
				t.Fatal("run succeeded, want an error")
			}
			details, marshalErr := json.Marshal(herrors.From(err).Details)
			if marshalErr != nil {
				t.Fatalf("encode details: %v", marshalErr)
			}
			if strings.Contains(err.Error(), credentialValue) {
				t.Errorf("error %q holds the credential value", err)
			}
			if strings.Contains(string(details), credentialValue) {
				t.Errorf("details %s hold the credential value", details)
			}
		})
	}
}

func TestCredentialLoadErrors(t *testing.T) {
	t.Run("missing file", func(t *testing.T) {
		_, err := newRuntime(t.TempDir()).Run(context.Background(), httprt.Handler{
			URL:  "https://192.0.2.1/",
			Auth: httprt.Auth{Type: httprt.AuthBearer, Credential: "api_token"},
		}, newRequest(runtime.KindOperation))
		if !errors.Is(err, herrors.HandlerFailed) {
			t.Fatalf("error = %v, want handler_failed", err)
		}
		if !strings.Contains(err.Error(), "api_token") {
			t.Errorf("error %q does not name the credential", err)
		}
	})
	t.Run("not configured", func(t *testing.T) {
		_, err := newRuntime("").Run(context.Background(), httprt.Handler{
			URL:  "https://192.0.2.1/",
			Auth: httprt.Auth{Type: httprt.AuthBearer, Credential: "api_token"},
		}, newRequest(runtime.KindOperation))
		if !errors.Is(err, herrors.HandlerFailed) {
			t.Fatalf("error = %v, want handler_failed", err)
		}
	})
	t.Run("unsafe permissions", func(t *testing.T) {
		if goruntime.GOOS == "windows" {
			t.Skip("POSIX permission bits do not apply on Windows")
		}
		dir := t.TempDir()
		writeCredential(t, dir, "remote", "api_token", credentialValue)
		path := filepath.Join(dir, "remote", "api_token")
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatalf("chmod: %v", err)
		}
		_, err := newRuntime(dir).Run(context.Background(), httprt.Handler{
			URL:  "https://192.0.2.1/",
			Auth: httprt.Auth{Type: httprt.AuthBearer, Credential: "api_token"},
		}, newRequest(runtime.KindOperation))
		if !errors.Is(err, herrors.HandlerFailed) {
			t.Fatalf("error = %v, want handler_failed", err)
		}
		if strings.Contains(err.Error(), credentialValue) {
			t.Errorf("error %q holds the credential value", err)
		}
	})
}
