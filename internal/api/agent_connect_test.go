package api

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/achmadss/hanse/internal/agentconn"
	"github.com/achmadss/hanse/internal/agentproto"
	"github.com/achmadss/hanse/internal/enroll"
	herrors "github.com/achmadss/hanse/internal/errors"
	"github.com/achmadss/hanse/internal/registry"
)

// agentFixture is one API server with an agent connection hub.
type agentFixture struct {
	handler http.Handler
	reg     *registry.Registry
	service *enroll.Service
	hub     *agentconn.Hub
	server  *httptest.Server
}

// newAgentFixture builds the API over the standard fixture registry and a
// fresh enrollment store. The store and the hub need each other, so one
// indirection breaks the cycle.
func newAgentFixture(t *testing.T) *agentFixture {
	t.Helper()
	return newAgentFixtureWith(t)
}

// newAgentFixtureWith is newAgentFixture with extra API options, such as the
// embedded node the Gateway runs itself.
func newAgentFixtureWith(t *testing.T, options ...Option) *agentFixture {
	t.Helper()
	reg := newFixture(t)
	var service *enroll.Service
	hub, err := agentconn.New(agentconn.Config{
		Registry:       reg,
		GatewayVersion: "1.0.0",
		Verify: func(credential string) (string, error) {
			if service == nil {
				return "", herrors.New(herrors.AuthenticationFailed, "", nil)
			}
			return service.Verify(credential)
		},
	})
	if err != nil {
		t.Fatalf("build hub: %v", err)
	}
	service, err = enroll.Open(enroll.Options{StateDir: t.TempDir(), Disconnect: hub})
	if err != nil {
		t.Fatalf("open enrollment store: %v", err)
	}
	t.Cleanup(func() { _ = service.Close() })
	handler := New(reg, nil, service, append([]Option{WithAgentHub(hub)}, options...)...)
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return &agentFixture{handler: handler, reg: reg, service: service, hub: hub, server: server}
}

// credential enrolls one node and returns its credential.
func (f *agentFixture) credential(t *testing.T, node string) string {
	t.Helper()
	token, err := f.service.IssueJoinToken(node)
	if err != nil {
		t.Fatalf("issue join token: %v", err)
	}
	outcome, err := f.service.Join(token.Plain, nil)
	if err != nil {
		t.Fatalf("join %s: %v", node, err)
	}
	return outcome.Credential
}

// helloFor returns a hello that matches the fixture node.
func helloFor(node string) *agentproto.Hello {
	switch node {
	case "node-a", "node-b":
		return &agentproto.Hello{
			Type: agentproto.TypeHello, Protocol: agentproto.ProtocolV1, AgentVersion: "1.2.3",
			OS: "linux", Arch: "arm64", Contracts: []string{"hanse.handler/v1"},
			MaxConcurrentRuns: 4,
		}
	case "node-c":
		return &agentproto.Hello{
			Type: agentproto.TypeHello, Protocol: agentproto.ProtocolV1, AgentVersion: "1.2.3",
			OS: "windows", Arch: "amd64", Contracts: []string{"hanse.handler/v1"},
			MaxConcurrentRuns: 4,
		}
	default:
		return &agentproto.Hello{
			Type: agentproto.TypeHello, Protocol: agentproto.ProtocolV1, AgentVersion: "1.2.3",
			OS: "linux", Arch: "amd64", Contracts: []string{"hanse.handler/v1"},
			MaxConcurrentRuns: 4,
		}
	}
}

// dialAgent opens a WebSocket to the connect route.
func (f *agentFixture) dialAgent(t *testing.T, credential string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	url := "ws" + strings.TrimPrefix(f.server.URL, "http") + "/v1/agent/connect"
	dialer := *websocket.DefaultDialer
	dialer.Subprotocols = []string{agentproto.SubprotocolV1}
	header := http.Header{"Authorization": []string{"Bearer " + credential}}
	ws, resp, err := dialer.Dial(url, header)
	if err == nil {
		t.Cleanup(func() { _ = ws.Close() })
	}
	return ws, resp, err
}

// TestAgentConnectRoute covers the connect route end to end and the node
// status and agent version that GET /v1/nodes reports afterwards.
func TestAgentConnectRoute(t *testing.T) {
	f := newAgentFixture(t)
	credential := f.credential(t, "node-a")
	ws, resp, err := f.dialAgent(t, credential)
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		t.Fatalf("dial: %v (status %d)", err, status)
	}
	data, err := agentproto.Encode(helloFor("node-a"))
	if err != nil {
		t.Fatalf("encode hello: %v", err)
	}
	_ = ws.SetWriteDeadline(time.Now().Add(3 * time.Second))
	if err := ws.WriteMessage(websocket.TextMessage, data); err != nil {
		t.Fatalf("write hello: %v", err)
	}
	_ = ws.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, welcomeData, err := ws.ReadMessage()
	if err != nil {
		t.Fatalf("read welcome: %v", err)
	}
	msg, err := agentproto.Decode(welcomeData)
	if err != nil {
		t.Fatalf("decode welcome: %v", err)
	}
	welcome, ok := msg.(*agentproto.Welcome)
	if !ok {
		t.Fatalf("first message is %T, want welcome", msg)
	}
	if welcome.Node != "node-a" {
		t.Errorf("welcome node %q, want node-a", welcome.Node)
	}

	_, body := get(t, f.handler, "/v1/nodes")
	var nodes nodesResponse
	if err := json.Unmarshal(body, &nodes); err != nil {
		t.Fatalf("decode nodes: %v", err)
	}
	var found *nodeView
	for i := range nodes.Nodes {
		if nodes.Nodes[i].ID == "node-a" {
			found = &nodes.Nodes[i]
		}
	}
	if found == nil {
		t.Fatal("node-a is missing from GET /v1/nodes")
	}
	if found.Status != string(registry.StatusOnline) {
		t.Errorf("node-a status %q, want online", found.Status)
	}
	if found.AgentVersion != "1.2.3" {
		t.Errorf("node-a agent version %q, want 1.2.3", found.AgentVersion)
	}
}

// TestAgentConnectRejectsBadCredential answers 401 before the upgrade.
func TestAgentConnectRejectsBadCredential(t *testing.T) {
	f := newAgentFixture(t)
	_, resp, err := f.dialAgent(t, "hnc_not-a-credential")
	if err == nil {
		t.Fatal("a wrong credential opened a connection")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status %v, want 401", resp)
	}
}

// TestAgentConnectRefusesEmbeddedNode proves the Gateway refuses a node
// credential for the node it runs itself: the embedded agent is already that
// node's executor, so a node agent would make two.
func TestAgentConnectRefusesEmbeddedNode(t *testing.T) {
	f := newAgentFixtureWith(t, WithEmbeddedNode("node-a"))
	credential := f.credential(t, "node-a")
	ws, resp, err := f.dialAgent(t, credential)
	if err == nil {
		_ = ws.Close()
		t.Fatal("the embedded node opened an agent connection")
	}
	if resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status %v, want 403", resp)
	}
	if body, readErr := io.ReadAll(resp.Body); readErr == nil {
		if code := codeOf(t, body); code != "forbidden" {
			t.Errorf("code = %q, want forbidden", code)
		}
		if !strings.Contains(string(body), "embedded_agent: false") {
			t.Errorf("body = %s, want it to name the setting that allows the connection", body)
		}
	}
	// The refusal happened before the upgrade, so the node never came
	// online.
	if f.hub.Online("node-a") {
		t.Error("node-a is online after a refused connection")
	}
}

// TestAgentBundle covers the authenticated bundle download and its
// eligibility check.
func TestAgentBundle(t *testing.T) {
	f := newAgentFixture(t)
	eligible := f.credential(t, "node-a")
	ineligible := f.credential(t, "node-e")
	capability, ok := f.reg.Capability("pc")
	if !ok {
		t.Fatal("the fixture has no pc capability")
	}
	path := "/v1/agent/bundles/" + capability.BundleHash

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+eligible)
	f.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("bundle status %d, body %s", rec.Code, rec.Body.Bytes())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/gzip" {
		t.Errorf("Content-Type %q, want application/gzip", got)
	}
	files := readTarGzip(t, rec.Body.Bytes())
	if _, ok := files["manifest.yaml"]; !ok {
		t.Errorf("bundle files %v, want manifest.yaml", keys(files))
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+ineligible)
	f.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("ineligible bundle status %d, want 403", rec.Code)
	}
	if code := codeOf(t, rec.Body.Bytes()); code != "forbidden" {
		t.Errorf("ineligible code %q, want forbidden", code)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/v1/agent/bundles/sha256:"+strings.Repeat("b", 64), nil)
	req.Header.Set("Authorization", "Bearer "+eligible)
	f.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown bundle status %d, want 400", rec.Code)
	}
	if code := codeOf(t, rec.Body.Bytes()); code != "invalid_request" {
		t.Errorf("unknown bundle code %q, want invalid_request", code)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, path, nil)
	f.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated bundle status %d, want 401", rec.Code)
	}
}

// TestAgentBundleAfterRevoke refuses the credential a revoke replaced.
func TestAgentBundleAfterRevoke(t *testing.T) {
	f := newAgentFixture(t)
	credential := f.credential(t, "node-a")
	if err := f.service.Revoke("node-a"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	capability, _ := f.reg.Capability("pc")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/agent/bundles/"+capability.BundleHash, nil)
	req.Header.Set("Authorization", "Bearer "+credential)
	f.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("revoked bundle status %d, want 401", rec.Code)
	}
}

// readTarGzip reads every regular file of a bundle archive.
func readTarGzip(t *testing.T, data []byte) map[string]string {
	t.Helper()
	gz, err := gzip.NewReader(strings.NewReader(string(data)))
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)
	out := map[string]string{}
	for {
		header, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatalf("tar: %v", err)
		}
		content, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("read %s: %v", header.Name, err)
		}
		out[header.Name] = string(content)
	}
}

// keys returns the map keys for a failure message.
func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for key := range m {
		out = append(out, key)
	}
	return out
}
