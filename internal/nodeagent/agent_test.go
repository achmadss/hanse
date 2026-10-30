package nodeagent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/achmadss/hanse/internal/agentproto"
)

// fakeGateway is a minimal Gateway side: it upgrades, reads hello, answers
// welcome, and then runs script.
type fakeGateway struct {
	server *httptest.Server
	conns  atomic.Int32
}

// newFakeGateway starts one Gateway stand-in. script receives the accepted
// connection after welcome.
func newFakeGateway(t *testing.T, script func(*websocket.Conn)) *fakeGateway {
	t.Helper()
	gateway := &fakeGateway{}
	upgrader := websocket.Upgrader{Subprotocols: []string{agentproto.SubprotocolV1}}
	gateway.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = ws.Close() }()
		gateway.conns.Add(1)
		if _, _, err := ws.ReadMessage(); err != nil {
			return
		}
		data, err := agentproto.Encode(&agentproto.Welcome{
			Type:              agentproto.TypeWelcome,
			Node:              "node-a",
			GatewayVersion:    "1.0.0",
			HeartbeatInterval: 1,
			HeartbeatTimeout:  5,
			Runs:              []agentproto.WelcomeRun{},
		})
		if err != nil {
			return
		}
		_ = ws.SetWriteDeadline(time.Now().Add(time.Second))
		if err := ws.WriteMessage(websocket.TextMessage, data); err != nil {
			return
		}
		if script != nil {
			script(ws)
		}
	}))
	t.Cleanup(gateway.server.Close)
	return gateway
}

// url returns the Gateway URL of the stand-in.
func (g *fakeGateway) url() string { return g.server.URL }

// newTestAgent builds an agent for a fake Gateway with test timings.
func newTestAgent(t *testing.T, gatewayURL string, configure func(*Config)) *Agent {
	t.Helper()
	cfg := Config{
		GatewayURL:        gatewayURL,
		Credential:        "hnc_test-credential",
		StateDir:          t.TempDir(),
		MaxConcurrentRuns: 2,
		ReconnectBase:     10 * time.Millisecond,
		ReconnectMax:      40 * time.Millisecond,
		HourlyRetry:       40 * time.Millisecond,
		HelloTimeout:      time.Second,
		Logf:              func(string, ...any) {},
	}
	if configure != nil {
		configure(&cfg)
	}
	agent, err := New(cfg)
	if err != nil {
		t.Fatalf("new agent: %v", err)
	}
	return agent
}

// TestReconnectDelayBounds covers the doubling and the jitter bound of
// docs/agent-protocol.md section 3.4.
func TestReconnectDelayBounds(t *testing.T) {
	base := time.Second
	maximum := 60 * time.Second
	for attempt := 0; attempt < 10; attempt++ {
		wantBase := base
		for i := 0; i < attempt && wantBase < maximum; i++ {
			wantBase *= 2
		}
		if wantBase > maximum {
			wantBase = maximum
		}
		low := reconnectDelay(attempt, base, maximum, func() float64 { return 0 })
		if low != wantBase {
			t.Fatalf("attempt %d delay %s, want %s", attempt, low, wantBase)
		}
		high := reconnectDelay(attempt, base, maximum, func() float64 { return 1 })
		limit := wantBase + wantBase/5
		if high > limit+time.Millisecond {
			t.Fatalf("attempt %d jittered delay %s, want at most %s", attempt, high, limit)
		}
		if high < wantBase {
			t.Fatalf("attempt %d jittered delay %s, want at least %s", attempt, high, wantBase)
		}
	}
	if got := reconnectDelay(20, base, maximum, func() float64 { return 0 }); got != maximum {
		t.Errorf("capped delay %s, want %s", got, maximum)
	}
}

// TestClassifyCloseCodes covers the table of docs/agent-protocol.md
// section 10.
func TestClassifyCloseCodes(t *testing.T) {
	cases := []struct {
		code     int
		terminal bool
		hourly   bool
	}{
		{closeNormal, false, false},
		{closeProtocolError, false, false},
		{closeRevoked, false, true},
		{closeReplaced, true, false},
		{closeUnsupportedVersion, false, true},
		{closeNodeMismatch, false, true},
		{closeNodeDisabled, false, false},
	}
	for _, c := range cases {
		got := classify(c.code, true, nil)
		if got.terminal != c.terminal || got.hourly != c.hourly {
			t.Errorf("close %d: terminal=%v hourly=%v, want %v/%v",
				c.code, got.terminal, got.hourly, c.terminal, c.hourly)
		}
	}
}

// TestReplacedConnectionStopsAgent proves that 4002 ends the process
// instead of reconnecting.
func TestReplacedConnectionStopsAgent(t *testing.T) {
	gateway := newFakeGateway(t, func(ws *websocket.Conn) {
		_ = ws.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(closeReplaced, "replaced"), time.Now().Add(time.Second))
	})
	agent := newTestAgent(t, gateway.url(), nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- agent.Run(ctx) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the agent did not stop after 4002")
	}
	if got := gateway.conns.Load(); got != 1 {
		t.Errorf("connections %d, want 1", got)
	}
}

// TestRevokedCredentialRetriesHourly proves that 4001 retries on the
// hourly path instead of the backoff.
func TestRevokedCredentialRetriesHourly(t *testing.T) {
	gateway := newFakeGateway(t, func(ws *websocket.Conn) {
		_ = ws.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(closeRevoked, "revoked"), time.Now().Add(time.Second))
	})
	agent := newTestAgent(t, gateway.url(), func(cfg *Config) {
		cfg.ReconnectBase = 5 * time.Second
		cfg.HourlyRetry = 20 * time.Millisecond
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = agent.Run(ctx) }()
	deadline := time.Now().Add(2 * time.Second)
	for gateway.conns.Load() < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("connections %d, want at least 2", gateway.conns.Load())
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
}

// TestHeartbeatIsSent proves the agent sends a heartbeat when it has sent
// nothing else for the interval.
func TestHeartbeatIsSent(t *testing.T) {
	heartbeat := make(chan struct{}, 1)
	gateway := newFakeGateway(t, func(ws *websocket.Conn) {
		_ = ws.SetReadDeadline(time.Now().Add(3 * time.Second))
		for {
			_, data, err := ws.ReadMessage()
			if err != nil {
				return
			}
			msg, err := agentproto.Decode(data)
			if err != nil {
				continue
			}
			if msg.MessageType() == agentproto.TypeHeartbeat {
				select {
				case heartbeat <- struct{}{}:
				default:
				}
				return
			}
		}
	})
	agent := newTestAgent(t, gateway.url(), nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = agent.Run(ctx) }()
	select {
	case <-heartbeat:
	case <-time.After(3 * time.Second):
		t.Fatal("the agent sent no heartbeat")
	}
}

// TestJournalReportsLostAfterRestart proves a run the agent accepted is
// reported lost instead of being run again.
func TestJournalReportsLostAfterRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.json")
	first, err := openJournal(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := first.add("op_1"); err != nil {
		t.Fatalf("add: %v", err)
	}
	second, err := openJournal(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	lost := second.lost()
	if len(lost) != 1 || lost[0] != "op_1" {
		t.Fatalf("lost %v, want [op_1]", lost)
	}
	if err := second.remove("op_1"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	third, err := openJournal(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if len(third.lost()) != 0 {
		t.Fatalf("lost %v, want none", third.lost())
	}
}

// TestHelloReportsLostRun proves hello names a journaled run as lost.
func TestHelloReportsLostRun(t *testing.T) {
	agent := newTestAgent(t, "http://127.0.0.1:1", nil)
	if err := agent.journal.add("op_lost"); err != nil {
		t.Fatalf("add: %v", err)
	}
	hello := agent.hello()
	if len(hello.Runs) != 1 {
		t.Fatalf("runs %+v, want one", hello.Runs)
	}
	if hello.Runs[0].State != agentproto.RunStateLost || hello.Runs[0].LastSeq != 0 {
		t.Errorf("run %+v, want lost with last_seq 0", hello.Runs[0])
	}
}

// TestBufferDropsOldestLogs proves the buffer keeps its limit, drops log
// lines only, and adds one system line that counts them.
func TestBufferDropsOldestLogs(t *testing.T) {
	agent := &Agent{
		cfg:  Config{BufferLimit: 40, Now: time.Now},
		runs: map[string]*run{},
	}
	r := &run{id: "op_1"}
	agent.runs[r.id] = r
	for i := 0; i < 6; i++ {
		agent.emitLog(r, agentproto.StreamStderr, strings.Repeat("x", 10), 0)
	}
	if agent.buffered > agent.cfg.BufferLimit {
		t.Errorf("buffered %d, want at most %d", agent.buffered, agent.cfg.BufferLimit)
	}
	foundSystem := false
	for _, pm := range r.pending {
		if logMessage, ok := pm.message.(*agentproto.Log); ok && logMessage.Stream == agentproto.StreamSystem {
			foundSystem = true
			if !strings.Contains(logMessage.Line, "dropped") {
				t.Errorf("system line %q does not count dropped lines", logMessage.Line)
			}
		}
	}
	if !foundSystem {
		t.Error("the buffer drop added no system line")
	}
}

// TestSplitLineKeepsUTF8 covers the log line split.
func TestSplitLineKeepsUTF8(t *testing.T) {
	chunks := splitLine("aaéébb", 3)
	if got := strings.Join(chunks, ""); got != "aaéébb" {
		t.Errorf("joined chunks %q", got)
	}
	for _, chunk := range chunks {
		if len(chunk) > 3 {
			t.Errorf("chunk %q is over the limit", chunk)
		}
	}
}

// TestStateDirHoldsJournal keeps the journal inside the state directory.
func TestStateDirHoldsJournal(t *testing.T) {
	state := t.TempDir()
	agent, err := New(Config{
		GatewayURL: "http://127.0.0.1:1",
		Credential: "hnc_test-credential",
		StateDir:   state,
		Logf:       func(string, ...any) {},
	})
	if err != nil {
		t.Fatalf("new agent: %v", err)
	}
	if _, err := os.Stat(filepath.Join(state, journalFile)); !os.IsNotExist(err) {
		t.Errorf("a fresh state directory must have no journal yet: %v", err)
	}
	if err := agent.journal.add("op_1"); err != nil {
		t.Fatalf("add: %v", err)
	}
	if _, err := os.Stat(filepath.Join(state, journalFile)); err != nil {
		t.Errorf("journal file missing: %v", err)
	}
}
