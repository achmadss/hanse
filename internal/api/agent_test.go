package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/achmadss/hanse/internal/enroll"
)

// joinFixture returns a handler wired with a fresh enrollment store and a
// clock the test can move forward.
func joinFixture(t *testing.T) (http.Handler, *enroll.Service, func(time.Duration)) {
	t.Helper()
	return joinFixtureWith(t)
}

// joinFixtureWith is joinFixture with extra API options, such as the
// embedded node the Gateway runs itself.
func joinFixtureWith(t *testing.T, options ...Option) (http.Handler, *enroll.Service, func(time.Duration)) {
	t.Helper()
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	service, err := enroll.Open(enroll.Options{
		StateDir: t.TempDir(),
		Now:      func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("open enrollment store: %v", err)
	}
	t.Cleanup(func() { _ = service.Close() })
	advance := func(d time.Duration) { now = now.Add(d) }
	return New(newFixture(t), nil, service, options...), service, advance
}

// postJoin sends one join request.
func postJoin(t *testing.T, handler http.Handler, in joinInput) (*httptest.ResponseRecorder, []byte) {
	t.Helper()
	body, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("encode the join request: %v", err)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/agent/join", bytes.NewReader(body)))
	return rec, rec.Body.Bytes()
}

// joinToken issues a join token for nodeID.
func joinToken(t *testing.T, service *enroll.Service, nodeID string) string {
	t.Helper()
	token, err := service.IssueJoinToken(nodeID)
	if err != nil {
		t.Fatalf("issue join token for %s: %v", nodeID, err)
	}
	return token.Plain
}

// codeOf reads the error code of a response body.
func codeOf(t *testing.T, body []byte) string {
	t.Helper()
	var e struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("decode the error body %s: %v", body, err)
	}
	return e.Code
}

// TestJoinAgent enrolls node-a and verifies the returned credential.
func TestJoinAgent(t *testing.T) {
	handler, service, _ := joinFixture(t)
	rec, body := postJoin(t, handler, joinInput{
		Protocol:     AgentProtocolV1,
		JoinToken:    joinToken(t, service, "node-a"),
		AgentVersion: "1.0.0",
		OS:           "linux",
		Arch:         "arm64",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("join status = %d, body %s", rec.Code, body)
	}
	var out joinResponse
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode the join response: %v", err)
	}
	if out.Node != "node-a" {
		t.Errorf("node = %q, want node-a", out.Node)
	}
	if node, err := service.Verify(out.Credential); err != nil || node != "node-a" {
		t.Errorf("verify the returned credential = %q, %v, want node-a, nil", node, err)
	}
}

// TestJoinAgentReusedToken proves the second exchange gets the documented
// 401, indistinguishably from an unknown or expired token.
func TestJoinAgentReusedToken(t *testing.T) {
	handler, service, _ := joinFixture(t)
	in := joinInput{
		Protocol:     AgentProtocolV1,
		JoinToken:    joinToken(t, service, "node-a"),
		AgentVersion: "1.0.0",
		OS:           "linux",
		Arch:         "arm64",
	}
	if rec, body := postJoin(t, handler, in); rec.Code != http.StatusOK {
		t.Fatalf("first join status = %d, body %s", rec.Code, body)
	}
	rec, body := postJoin(t, handler, in)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("reused token status = %d, want 401, body %s", rec.Code, body)
	}
	if code := codeOf(t, body); code != "join_token_invalid" {
		t.Errorf("reused token code = %q, want join_token_invalid", code)
	}
}

// TestJoinAgentExpiredToken moves the injected clock past the default
// lifetime and requires the same 401.
func TestJoinAgentExpiredToken(t *testing.T) {
	handler, service, advance := joinFixture(t)
	in := joinInput{
		Protocol:     AgentProtocolV1,
		JoinToken:    joinToken(t, service, "node-a"),
		AgentVersion: "1.0.0",
		OS:           "linux",
		Arch:         "arm64",
	}
	advance(15 * time.Minute)
	rec, body := postJoin(t, handler, in)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expired token status = %d, want 401, body %s", rec.Code, body)
	}
	if code := codeOf(t, body); code != "join_token_invalid" {
		t.Errorf("expired token code = %q, want join_token_invalid", code)
	}
}

// TestJoinAgentRefusesEmbeddedNode proves the Gateway refuses a join token
// for the node it runs itself: that node already has the embedded agent as
// its executor. The token stays unused, so the operator can enroll the node
// after removing `node` from gateway.yaml.
func TestJoinAgentRefusesEmbeddedNode(t *testing.T) {
	handler, service, _ := joinFixtureWith(t, WithEmbeddedNode("gateway"))
	in := joinInput{
		Protocol:     AgentProtocolV1,
		JoinToken:    joinToken(t, service, "gateway"),
		AgentVersion: "1.0.0",
		OS:           "linux",
		Arch:         "amd64",
	}
	rec, body := postJoin(t, handler, in)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("embedded node status = %d, want 403, body %s", rec.Code, body)
	}
	if code := codeOf(t, body); code != "forbidden" {
		t.Errorf("embedded node code = %q, want forbidden", code)
	}
	if !strings.Contains(string(body), "embedded agent") {
		t.Errorf("body = %s, want it to name the embedded agent", body)
	}
	// A Gateway without `node` accepts the same token afterwards: the
	// refusal consumed nothing.
	open := New(newFixture(t), nil, service)
	if rec, body := postJoin(t, open, in); rec.Code != http.StatusOK {
		t.Fatalf("join after the refusal status = %d, want 200, body %s", rec.Code, body)
	}
}

// TestJoinAgentNodeMismatch proves a differing OS or architecture gets 409
// and leaves the token unused, so the agent can retry after the operator
// fixes nodes.yaml.
func TestJoinAgentNodeMismatch(t *testing.T) {
	handler, service, _ := joinFixture(t)
	in := joinInput{
		Protocol:     AgentProtocolV1,
		JoinToken:    joinToken(t, service, "node-b"),
		AgentVersion: "1.0.0",
		OS:           "linux",
		Arch:         "amd64",
	}
	rec, body := postJoin(t, handler, in)
	if rec.Code != http.StatusConflict {
		t.Fatalf("mismatch status = %d, want 409, body %s", rec.Code, body)
	}
	if code := codeOf(t, body); code != "node_mismatch" {
		t.Errorf("mismatch code = %q, want node_mismatch", code)
	}
	in.Arch = "arm64"
	if rec, body := postJoin(t, handler, in); rec.Code != http.StatusOK {
		t.Fatalf("join after the mismatch status = %d, want 200: the token must stay unused, body %s", rec.Code, body)
	}
}

// TestJoinAgentUnknownNode proves a token for a node that nodes.yaml no
// longer holds conflicts instead of enrolling it.
func TestJoinAgentUnknownNode(t *testing.T) {
	handler, service, _ := joinFixture(t)
	in := joinInput{
		Protocol:     AgentProtocolV1,
		JoinToken:    joinToken(t, service, "node-z"),
		AgentVersion: "1.0.0",
		OS:           "linux",
		Arch:         "amd64",
	}
	rec, body := postJoin(t, handler, in)
	if rec.Code != http.StatusConflict {
		t.Fatalf("unknown node status = %d, want 409, body %s", rec.Code, body)
	}
	if code := codeOf(t, body); code != "node_mismatch" {
		t.Errorf("unknown node code = %q, want node_mismatch", code)
	}
	// Still a conflict, not a used-token 401: nothing was consumed.
	if rec, _ := postJoin(t, handler, in); rec.Code != http.StatusConflict {
		t.Errorf("second attempt status = %d, want 409", rec.Code)
	}
}

// TestJoinAgentBadProtocol proves an unsupported protocol version is the
// documented 400.
func TestJoinAgentBadProtocol(t *testing.T) {
	handler, service, _ := joinFixture(t)
	rec, body := postJoin(t, handler, joinInput{
		Protocol:  "hanse.agent/v2",
		JoinToken: joinToken(t, service, "node-a"),
		OS:        "linux",
		Arch:      "arm64",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad protocol status = %d, want 400, body %s", rec.Code, body)
	}
	if code := codeOf(t, body); code != "invalid_request" {
		t.Errorf("bad protocol code = %q, want invalid_request", code)
	}
}

// TestJoinAgentMalformedBody proves a body that is not one JSON object is
// the documented 400.
func TestJoinAgentMalformedBody(t *testing.T) {
	handler, _, _ := joinFixture(t)
	for _, body := range []string{
		`{"protocol":"hanse.agent/v1"`,
		`{"protocol":"hanse.agent/v1","join_token":"x","os":"linux","arch":"arm64","unknown":true}`,
	} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/agent/join", bytes.NewReader([]byte(body))))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body %s: status = %d, want 400", body, rec.Code)
		}
		if code := codeOf(t, rec.Body.Bytes()); code != "invalid_request" {
			t.Errorf("body %s: code = %q, want invalid_request", body, code)
		}
	}
}

// TestJoinAgentWithoutStore proves a Gateway without an enrollment store
// never pretends to enroll a node.
func TestJoinAgentWithoutStore(t *testing.T) {
	handler := New(newFixture(t), nil, nil)
	rec, body := postJoin(t, handler, joinInput{
		Protocol:  AgentProtocolV1,
		JoinToken: "hjt_" + "A",
		OS:        "linux",
		Arch:      "arm64",
	})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500, body %s", rec.Code, body)
	}
	if code := codeOf(t, body); code != "internal" {
		t.Errorf("code = %q, want internal", code)
	}
}

// TestJoinAgentCredentialsArePerNode proves that credentials for two nodes
// stay bound to their own node IDs.
func TestJoinAgentCredentialsArePerNode(t *testing.T) {
	handler, service, _ := joinFixture(t)
	join := func(nodeID, arch string) string {
		rec, body := postJoin(t, handler, joinInput{
			Protocol:  AgentProtocolV1,
			JoinToken: joinToken(t, service, nodeID),
			OS:        "linux",
			Arch:      arch,
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("join %s status = %d, body %s", nodeID, rec.Code, body)
		}
		var out joinResponse
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatalf("decode the %s response: %v", nodeID, err)
		}
		return out.Credential
	}
	a := join("node-a", "arm64")
	b := join("node-b", "arm64")
	nodeA, err := service.Verify(a)
	if err != nil || nodeA != "node-a" {
		t.Fatalf("verify node-a's credential = %q, %v, want node-a, nil", nodeA, err)
	}
	nodeB, err := service.Verify(b)
	if err != nil || nodeB != "node-b" {
		t.Fatalf("verify node-b's credential = %q, %v, want node-b, nil", nodeB, err)
	}
	if a == b {
		t.Error("both nodes received the same credential")
	}
}
