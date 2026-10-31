// Package nodeagent is the node side of the Hanse agent protocol: it
// connects out to the Gateway, receives run requests, runs handlers with
// internal/runner, and sends their logs and results back
// (docs/agent-protocol.md, docs/architecture.md section 8).
//
// The agent keeps handlers running while the connection is down, buffers
// their unacknowledged messages up to agent.yaml's limit, and resumes
// without gaps or duplicates after it reconnects. It enforces deadlines
// and cancellations on the node, so both work while it is disconnected,
// and it reports runs it lost in a restart as `lost` instead of running
// them again.
package nodeagent

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/achmadss/hanse/internal/agentproto"
	"github.com/achmadss/hanse/internal/bundle"
	"github.com/achmadss/hanse/internal/config"
	herrors "github.com/achmadss/hanse/internal/errors"
	"github.com/achmadss/hanse/internal/runner"
	"github.com/achmadss/hanse/internal/version"
)

// Defaults of the protocol and agent.yaml (docs/agent-protocol.md section
// 11, docs/configuration.md section 7.1).
const (
	// DefaultMaxConcurrentRuns is the most handlers that run at once.
	DefaultMaxConcurrentRuns = 16
	// DefaultBufferLimit is the most unacknowledged log data held while
	// disconnected.
	DefaultBufferLimit int64 = 10 << 20
	// DefaultReconnectBase is the first reconnect wait.
	DefaultReconnectBase = time.Second
	// DefaultReconnectMax is the longest reconnect wait.
	DefaultReconnectMax = 60 * time.Second
	// DefaultHourlyRetry is the wait after a refusal that an operator
	// must fix: a revoked credential, an unsupported protocol, or a node
	// mismatch.
	DefaultHourlyRetry = time.Hour
	// DefaultWriteTimeout bounds one WebSocket write.
	DefaultWriteTimeout = 10 * time.Second
)

// WebSocket close codes of the agent protocol (docs/agent-protocol.md
// section 10).
const (
	closeNormal             = 1000
	closeProtocolError      = 4000
	closeRevoked            = 4001
	closeReplaced           = 4002
	closeUnsupportedVersion = 4003
	closeNodeMismatch       = 4004
	closeNodeDisabled       = 4005
)

// ContractV1 is the handler contract version the agent runs.
const ContractV1 = "hanse.handler/v1"

// Config is what an Agent needs.
type Config struct {
	// GatewayURL is the Gateway's public URL. Required.
	GatewayURL string
	// Credential is the node credential. Required, and never logged.
	Credential string
	// CAFile is an extra CA certificate for the Gateway, in PEM format.
	CAFile string
	// StateDir holds the journal, the cache, and scratch directories.
	// Required.
	StateDir string
	// CacheDir is the handler cache. Empty means <state_dir>/cache.
	CacheDir string
	// MaxConcurrentRuns is the most handlers that run at once. Zero means
	// DefaultMaxConcurrentRuns.
	MaxConcurrentRuns int
	// BufferLimit is the most unacknowledged log data held across all
	// runs. Zero means DefaultBufferLimit.
	BufferLimit int64
	// EnvAllow lists extra environment variables passed to handlers.
	EnvAllow []string
	// Elevation holds the node's privileged-operation settings
	// (docs/configuration.md section 7.1).
	Elevation Elevation
	// VerifyCredential checks one elevation password. Nil uses the
	// platform check. Tests inject a fake so no real password is needed.
	VerifyCredential func(account, password string) error
	// SupervisorCommand is the command prefix that starts the root
	// supervisor of a privileged POSIX run. Nil means this executable
	// with the `supervise` subcommand. Windows ignores it.
	SupervisorCommand []string
	// AgentVersion is reported in hello. Empty means version.Version.
	AgentVersion string
	// OS and Arch are reported in hello. Empty means the running binary's
	// values.
	OS, Arch string
	// HelloTimeout bounds the hello and welcome exchange. Zero means
	// agentproto.HelloTimeout.
	HelloTimeout time.Duration
	// ReconnectBase and ReconnectMax bound the reconnect wait. Zero means
	// the documented defaults.
	ReconnectBase time.Duration
	ReconnectMax  time.Duration
	// HourlyRetry is the wait after close codes 4001, 4003, and 4004.
	// Zero means DefaultHourlyRetry.
	HourlyRetry time.Duration
	// WriteTimeout bounds one WebSocket write. Zero means
	// DefaultWriteTimeout.
	WriteTimeout time.Duration
	// MaxUnpackedBytes bounds one unpacked bundle. Zero means the bundle
	// package default.
	MaxUnpackedBytes int64
	// Logf receives the agent's own log lines. Nil discards them. It never
	// receives the credential or a secret.
	Logf func(format string, args ...any)
	// Now returns the current time. Nil means time.Now.
	Now func() time.Time
	// After waits on the agent's clock. Nil means time.After.
	After func(time.Duration) <-chan time.Time
	// Rand returns a value in [0,1) for the reconnect jitter. Nil means
	// math/rand.Float64.
	Rand func() float64
	// Dialer is the WebSocket dialer. Nil builds one from GatewayURL and
	// CAFile.
	Dialer *websocket.Dialer
	// HTTPClient downloads bundles. Nil means a client with a 30 second
	// timeout.
	HTTPClient *http.Client
}

// Agent is one node's connection to the Gateway. It runs handlers, keeps
// the run journal, and reconnects until its context ends.
type Agent struct {
	cfg     Config
	cache   *bundle.Cache
	journal *journal
	tls     *tls.Config
	workDir string
	// elevation decides every privileged run (docs/architecture.md
	// section 8.5).
	elevation *elevationGate

	mu       sync.Mutex
	runs     map[string]*run
	current  *session
	buffered int64
}

// New builds an agent. It fails when the Gateway URL, the credential, or
// the state directory is missing, or when the journal cannot be loaded.
func New(cfg Config) (*Agent, error) {
	if cfg.GatewayURL == "" {
		return nil, herrors.New(herrors.Internal, "the node agent needs a Gateway URL", nil)
	}
	u, err := url.Parse(cfg.GatewayURL)
	if err != nil || u.Hostname() == "" {
		return nil, herrors.New(herrors.Internal, "the Gateway URL is not a valid URL", nil)
	}
	if u.Scheme != "https" && (u.Scheme != "http" || !config.IsLoopbackHost(u.Hostname())) {
		return nil, herrors.New(herrors.Internal,
			"the Gateway URL must use https, unless its host is a loopback address", nil)
	}
	if cfg.Credential == "" {
		return nil, herrors.New(herrors.Internal, "the node agent needs a node credential", nil)
	}
	if cfg.StateDir == "" {
		return nil, herrors.New(herrors.Internal, "the node agent needs a state directory", nil)
	}
	if cfg.MaxConcurrentRuns <= 0 {
		cfg.MaxConcurrentRuns = DefaultMaxConcurrentRuns
	}
	if cfg.BufferLimit <= 0 {
		cfg.BufferLimit = DefaultBufferLimit
	}
	if cfg.AgentVersion == "" {
		cfg.AgentVersion = version.Version
	}
	if cfg.OS == "" {
		cfg.OS = osName()
	}
	if cfg.Arch == "" {
		cfg.Arch = archName()
	}
	if cfg.HelloTimeout <= 0 {
		cfg.HelloTimeout = agentproto.HelloTimeout
	}
	if cfg.ReconnectBase <= 0 {
		cfg.ReconnectBase = DefaultReconnectBase
	}
	if cfg.ReconnectMax <= 0 {
		cfg.ReconnectMax = DefaultReconnectMax
	}
	if cfg.HourlyRetry <= 0 {
		cfg.HourlyRetry = DefaultHourlyRetry
	}
	if cfg.WriteTimeout <= 0 {
		cfg.WriteTimeout = DefaultWriteTimeout
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.After == nil {
		cfg.After = time.After
	}
	if cfg.Rand == nil {
		cfg.Rand = rand.Float64
	}
	cacheDir := cfg.CacheDir
	if cacheDir == "" {
		cacheDir = filepath.Join(cfg.StateDir, "cache")
	}
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		return nil, herrors.New(herrors.Internal,
			fmt.Sprintf("cannot create the handler cache %s: %s", cacheDir, err), nil)
	}
	tlsConfig, err := tlsConfigFor(cfg.CAFile)
	if err != nil {
		return nil, err
	}
	j, err := openJournal(filepath.Join(cfg.StateDir, journalFile))
	if err != nil {
		return nil, herrors.New(herrors.Internal, err.Error(), nil)
	}
	workDir := filepath.Join(cfg.StateDir, "work")
	if err := os.MkdirAll(workDir, 0o700); err != nil {
		return nil, herrors.New(herrors.Internal,
			fmt.Sprintf("cannot create the work directory %s: %s", workDir, err), nil)
	}
	return &Agent{
		cfg: cfg,
		cache: &bundle.Cache{
			Dir:              cacheDir,
			GatewayURL:       cfg.GatewayURL,
			Credential:       cfg.Credential,
			Client:           cfg.HTTPClient,
			MaxUnpackedBytes: cfg.MaxUnpackedBytes,
		},
		journal:   j,
		tls:       tlsConfig,
		workDir:   workDir,
		elevation: newElevationGate(cfg.Elevation, cfg.Now, passwordVerifier(cfg)),
		runs:      map[string]*run{},
	}, nil
}

// passwordVerifier returns the node's password check. A configured check
// wins; otherwise the platform check runs the same supervisor command the
// launch uses, so sudo needs permission for exactly one command.
func passwordVerifier(cfg Config) func(account, password string) error {
	if cfg.VerifyCredential != nil {
		return cfg.VerifyCredential
	}
	supervisor := append([]string(nil), cfg.SupervisorCommand...)
	return func(account, password string) error {
		return runner.VerifyCredential(account, password, supervisor...)
	}
}

// tlsConfigFor builds the Gateway TLS configuration, with the extra CA
// file when one is configured.
func tlsConfigFor(caFile string) (*tls.Config, error) {
	if caFile == "" {
		return nil, nil
	}
	data, err := os.ReadFile(caFile)
	if err != nil {
		return nil, herrors.New(herrors.Internal,
			fmt.Sprintf("cannot read ca_file %s: %s", caFile, err), nil)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(data) {
		return nil, herrors.New(herrors.Internal,
			fmt.Sprintf("ca_file %s holds no PEM certificate", caFile), nil)
	}
	return &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}, nil
}

// Run connects, serves runs, and reconnects with backoff until ctx ends.
// A close with 4002 ends the agent: another connection replaced this one,
// and this process must not reconnect (docs/agent-protocol.md section 10).
func (a *Agent) Run(ctx context.Context) error {
	attempt := 0
	for {
		if ctx.Err() != nil {
			a.stopAll()
			return nil
		}
		outcome := a.session(ctx)
		if outcome.terminal {
			a.stopAll()
			return nil
		}
		if ctx.Err() != nil {
			a.stopAll()
			return nil
		}
		if outcome.welcomed {
			attempt = 0
		}
		wait := a.cfg.HourlyRetry
		if !outcome.hourly {
			wait = reconnectDelay(attempt, a.cfg.ReconnectBase, a.cfg.ReconnectMax, a.cfg.Rand)
			attempt++
		}
		a.logf("%s; reconnecting in %s", outcome.reason(), wait)
		if err := a.wait(ctx, wait); err != nil {
			a.stopAll()
			return nil
		}
	}
}

// sessionResult is how one connection ended.
type sessionResult struct {
	closeCode int
	welcomed  bool
	err       error
	// terminal ends the agent: close code 4002.
	terminal bool
	// hourly replaces the backoff with the once-an-hour retry.
	hourly bool
}

// reason describes the end of one connection for the agent's own log.
func (r sessionResult) reason() string {
	switch {
	case r.closeCode == closeReplaced:
		return "another connection for this node replaced this one"
	case r.closeCode == closeRevoked:
		return "the node credential was revoked or is invalid"
	case r.closeCode == closeUnsupportedVersion:
		return "the Gateway does not support this agent protocol version"
	case r.closeCode == closeNodeMismatch:
		return "the node's OS or architecture differs from nodes.yaml"
	case r.closeCode == closeNodeDisabled:
		return "the node is disabled in nodes.yaml"
	case r.closeCode == closeProtocolError:
		return "the connection ended with a protocol error"
	case r.err != nil:
		return "the connection was lost: " + r.err.Error()
	default:
		return "the connection closed"
	}
}

// session opens one connection and serves it until it ends.
func (a *Agent) session(ctx context.Context) sessionResult {
	ws, resp, err := a.dial(ctx)
	if err != nil {
		code := 0
		if resp != nil {
			switch resp.StatusCode {
			case http.StatusUnauthorized:
				code = closeRevoked
			case http.StatusUpgradeRequired:
				code = closeUnsupportedVersion
			}
		}
		return classify(code, false, err)
	}
	defer func() { _ = ws.Close() }()

	if err := a.write(ws, a.hello()); err != nil {
		return classify(0, false, err)
	}
	_ = ws.SetReadDeadline(time.Now().Add(a.cfg.HelloTimeout))
	msg, err := readMessage(ws)
	if err != nil {
		if isTimeout(err) {
			_ = writeClose(ws, closeProtocolError, "the welcome message did not arrive in time")
			return classify(closeProtocolError, false, err)
		}
		return classify(closeCodeOf(err), false, err)
	}
	welcome, ok := msg.(*agentproto.Welcome)
	if !ok {
		_ = writeClose(ws, closeProtocolError, "the first message is not welcome")
		return classify(closeProtocolError, false,
			fmt.Errorf("the first message is %q, not welcome", msg.MessageType()))
	}
	a.applyWelcome(welcome)

	s := newSession(ws)
	a.mu.Lock()
	a.current = s
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		if a.current == s {
			a.current = nil
		}
		a.mu.Unlock()
		s.close()
	}()
	go func() {
		select {
		case <-ctx.Done():
			_ = ws.Close()
		case <-s.done:
		}
	}()
	interval := time.Duration(welcome.HeartbeatInterval) * time.Second
	timeout := time.Duration(welcome.HeartbeatTimeout) * time.Second
	go a.writeLoop(ctx, s, interval, timeout)
	s.wakeUp()

	code := 0
	for {
		_ = ws.SetReadDeadline(time.Now().Add(timeout))
		msg, err := readMessage(ws)
		if err != nil {
			var unknown *agentproto.UnknownMessageError
			if errors.As(err, &unknown) {
				// A newer Gateway may probe for a message type this agent
				// does not know. Answer and keep the connection open
				// (docs/agent-protocol.md section 10).
				a.sendControl(&agentproto.Error{
					Type:    agentproto.TypeError,
					Code:    "unknown_message",
					Message: "unknown message type: " + unknown.Type,
				})
				continue
			}
			var decode *agentproto.DecodeError
			if errors.As(err, &decode) {
				_ = writeClose(ws, closeProtocolError, "a message is not valid for this protocol")
				return classify(closeProtocolError, true, err)
			}
			code = closeCodeOf(err)
			if code == 0 && !errors.Is(err, context.Canceled) {
				a.logf("the connection ended: %v", err)
			}
			return classify(code, true, err)
		}
		if code := a.handle(msg); code != 0 {
			_ = writeClose(ws, code, "the message is not valid for this protocol")
			return classify(code, true, nil)
		}
	}
}

// classify turns one connection end into the next wait's terms.
func classify(code int, welcomed bool, err error) sessionResult {
	outcome := sessionResult{closeCode: code, welcomed: welcomed, err: err}
	switch code {
	case closeReplaced:
		outcome.terminal = true
	case closeRevoked, closeUnsupportedVersion, closeNodeMismatch:
		outcome.hourly = true
	}
	return outcome
}

// reconnectDelay returns the wait before the next connection: the base
// doubled per failed attempt, capped at max, plus up to 20% of the wait as
// jitter (docs/agent-protocol.md section 3.4).
func reconnectDelay(attempt int, base, maximum time.Duration, randFloat func() float64) time.Duration {
	if base <= 0 {
		base = DefaultReconnectBase
	}
	if maximum <= 0 {
		maximum = DefaultReconnectMax
	}
	if maximum < base {
		maximum = base
	}
	wait := base
	for i := 0; i < attempt && wait < maximum; i++ {
		wait *= 2
	}
	if wait > maximum {
		wait = maximum
	}
	jitter := float64(wait) * 0.2 * randFloat()
	return wait + time.Duration(jitter)
}

// wait sleeps on the agent's clock until d passes or ctx ends.
func (a *Agent) wait(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-a.cfg.After(d):
		return nil
	}
}

// dial opens the agent WebSocket with the node credential
// (docs/agent-protocol.md section 3.1).
func (a *Agent) dial(ctx context.Context) (*websocket.Conn, *http.Response, error) {
	endpoint, err := a.connectURL()
	if err != nil {
		return nil, nil, err
	}
	dialer := a.cfg.Dialer
	if dialer == nil {
		dialer = &websocket.Dialer{
			HandshakeTimeout: a.cfg.HelloTimeout,
			TLSClientConfig:  a.tls,
			Subprotocols:     []string{agentproto.SubprotocolV1},
		}
	}
	header := http.Header{
		"Authorization": []string{"Bearer " + a.cfg.Credential},
	}
	return dialer.DialContext(ctx, endpoint, header)
}

// connectURL builds the WebSocket URL of GET /v1/agent/connect.
func (a *Agent) connectURL() (string, error) {
	u, err := url.Parse(a.cfg.GatewayURL)
	if err != nil || u.Host == "" {
		return "", herrors.New(herrors.Internal, "the Gateway URL is not a valid URL", nil)
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	}
	u.Path = strings.TrimSuffix(u.Path, "/") + "/v1/agent/connect"
	return u.String(), nil
}

// hello builds the agent's first message and lists every run whose outcome
// the agent still holds, plus every run a restart lost
// (docs/agent-protocol.md section 4.1).
func (a *Agent) hello() *agentproto.Hello {
	a.mu.Lock()
	states := make([]agentproto.RunState, 0, len(a.runs)+len(a.journal.lost()))
	for _, r := range a.runs {
		state := agentproto.RunStateRunning
		if r.resultSeq > 0 {
			state = agentproto.RunStateFinished
		}
		states = append(states, agentproto.RunState{
			RunID:   r.id,
			State:   state,
			LastSeq: r.seq,
		})
	}
	for _, id := range a.journal.lost() {
		if _, live := a.runs[id]; live {
			continue
		}
		states = append(states, agentproto.RunState{
			RunID: id, State: agentproto.RunStateLost, LastSeq: 0,
		})
	}
	a.mu.Unlock()
	return &agentproto.Hello{
		Type:              agentproto.TypeHello,
		Protocol:          agentproto.ProtocolV1,
		AgentVersion:      a.cfg.AgentVersion,
		OS:                a.cfg.OS,
		Arch:              a.cfg.Arch,
		Contracts:         []string{ContractV1},
		MaxConcurrentRuns: a.cfg.MaxConcurrentRuns,
		Runs:              states,
	}
}

// applyWelcome carries out the Gateway's decision for every run of hello
// (docs/agent-protocol.md section 8.2). A run that resumes resends what is
// above acked_seq; a run that is abandoned is stopped and forgotten.
func (a *Agent) applyWelcome(welcome *agentproto.Welcome) {
	a.mu.Lock()
	listed := make(map[string]bool, len(welcome.Runs))
	for _, decision := range welcome.Runs {
		listed[decision.RunID] = true
		r := a.runs[decision.RunID]
		if r == nil {
			// A lost run, or one the agent already forgot.
			_ = a.journal.remove(decision.RunID)
			continue
		}
		switch decision.Action {
		case agentproto.ActionAbandon:
			a.abandonLocked(r)
		case agentproto.ActionResume:
			acked := int64(0)
			if decision.AckedSeq != nil {
				acked = *decision.AckedSeq
			}
			a.applyAckLocked(r, acked)
			for _, pm := range r.pending {
				pm.sent = false
			}
		}
	}
	// A run the welcome does not name keeps everything above acked_seq
	// and resends it, so a missing decision never loses a message.
	for id, r := range a.runs {
		if listed[id] {
			continue
		}
		for _, pm := range r.pending {
			pm.sent = false
		}
	}
	s := a.current
	a.mu.Unlock()
	if s != nil {
		s.wakeUp()
	}
}

// writeLoop sends unacknowledged messages, control messages, and
// heartbeats for one connection. It is the only writer of the socket.
func (a *Agent) writeLoop(ctx context.Context, s *session, interval, timeout time.Duration) {
	if interval <= 0 {
		interval = 15 * time.Second
	}
	if timeout <= 0 {
		timeout = 45 * time.Second
	}
	if interval > timeout {
		interval = timeout
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	lastSent := a.cfg.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.done:
			return
		case msg := <-s.control:
			if err := s.write(msg, a.cfg.WriteTimeout); err != nil {
				s.fail()
				return
			}
			lastSent = a.cfg.Now()
		case <-s.wake:
		case <-ticker.C:
		}
		sent, err := a.flush(s)
		if err != nil {
			s.fail()
			return
		}
		now := a.cfg.Now()
		if sent {
			lastSent = now
		}
		if now.Sub(lastSent) >= interval {
			heartbeat := &agentproto.Heartbeat{
				Type: agentproto.TypeHeartbeat,
				TS:   agentproto.Timestamp(now),
			}
			if err := s.write(heartbeat, a.cfg.WriteTimeout); err != nil {
				s.fail()
				return
			}
			lastSent = now
		}
	}
}

// flush writes every pending message that this connection has not sent.
// It returns whether it wrote anything.
func (a *Agent) flush(s *session) (bool, error) {
	type item struct {
		message agentproto.Message
	}
	a.mu.Lock()
	items := make([]item, 0, len(a.runs))
	for _, r := range a.runs {
		for _, pm := range r.pending {
			if pm.sent {
				continue
			}
			pm.sent = true
			items = append(items, item{message: pm.message})
		}
	}
	a.mu.Unlock()
	for _, it := range items {
		if err := s.write(it.message, a.cfg.WriteTimeout); err != nil {
			return false, err
		}
	}
	return len(items) > 0, nil
}

// handle processes one Gateway message. A non-zero return asks the caller
// to close the connection with that code.
func (a *Agent) handle(msg agentproto.Message) int {
	switch m := msg.(type) {
	case *agentproto.Heartbeat:
		return 0
	case *agentproto.Run:
		a.acceptRun(m)
		return 0
	case *agentproto.Cancel:
		a.cancelRun(m.RunID)
		return 0
	case *agentproto.Ack:
		a.handleAck(m.RunID, m.Seq)
		return 0
	case *agentproto.Error:
		a.logf("the Gateway reported %s: %s", m.Code, m.Message)
		return 0
	default:
		a.sendControl(&agentproto.Error{
			Type:    agentproto.TypeError,
			Code:    "unexpected_message",
			Message: fmt.Sprintf("message type %q is not sent by the Gateway", msg.MessageType()),
		})
		return closeProtocolError
	}
}

// stopAll stops every handler when the agent shuts down.
func (a *Agent) stopAll() {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, r := range a.runs {
		if r.cancel != nil {
			r.cancel()
		}
	}
}

// sendControl sends one message that is not part of a run's buffer.
func (a *Agent) sendControl(msg agentproto.Message) {
	a.mu.Lock()
	s := a.current
	a.mu.Unlock()
	if s != nil {
		s.enqueueControl(msg)
	}
}

// logf writes one agent log line.
func (a *Agent) logf(format string, args ...any) {
	if a.cfg.Logf != nil {
		a.cfg.Logf(format, args...)
	}
}

// session is one live connection. Its writer goroutine is the only writer
// of the socket.
type session struct {
	ws      *websocket.Conn
	control chan agentproto.Message
	wake    chan struct{}
	done    chan struct{}
	once    sync.Once
}

// newSession prepares a connection's channels.
func newSession(ws *websocket.Conn) *session {
	return &session{
		ws:      ws,
		control: make(chan agentproto.Message, 64),
		wake:    make(chan struct{}, 1),
		done:    make(chan struct{}),
	}
}

// write sends one message frame.
func (s *session) write(msg agentproto.Message, timeout time.Duration) error {
	data, err := agentproto.Encode(msg)
	if err != nil {
		return err
	}
	_ = s.ws.SetWriteDeadline(time.Now().Add(timeout))
	return s.ws.WriteMessage(websocket.TextMessage, data)
}

// wakeUp asks the writer to flush pending messages.
func (s *session) wakeUp() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// enqueueControl queues one message outside a run's buffer. A full queue
// drops the message; the Gateway sends the run again after a reconnect.
func (s *session) enqueueControl(msg agentproto.Message) {
	select {
	case s.control <- msg:
	default:
	}
}

// fail marks the connection as ended.
func (s *session) fail() { s.close() }

// close ends the connection once.
func (s *session) close() {
	s.once.Do(func() { close(s.done) })
}

// readMessage reads and decodes one message from the connection.
func readMessage(ws *websocket.Conn) (agentproto.Message, error) {
	_, data, err := ws.ReadMessage()
	if err != nil {
		return nil, err
	}
	return agentproto.Decode(data)
}

// write closes the connection with a code and a reason.
func writeClose(ws *websocket.Conn, code int, reason string) error {
	return ws.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(code, reason), time.Now().Add(time.Second))
}

// write encodes and writes one message without a session, for the hello
// exchange.
func (a *Agent) write(ws *websocket.Conn, msg agentproto.Message) error {
	data, err := agentproto.Encode(msg)
	if err != nil {
		return err
	}
	_ = ws.SetWriteDeadline(time.Now().Add(a.cfg.WriteTimeout))
	return ws.WriteMessage(websocket.TextMessage, data)
}

// isTimeout reports whether a read failed because its deadline passed.
func isTimeout(err error) bool {
	var timeout net.Error
	return errors.As(err, &timeout) && timeout.Timeout()
}

// closeCodeOf returns the protocol close code of a read failure, or 0 when
// the failure was not a close frame with a code.
func closeCodeOf(err error) int {
	var closeErr *websocket.CloseError
	if errors.As(err, &closeErr) {
		switch closeErr.Code {
		case websocket.CloseNoStatusReceived, websocket.CloseAbnormalClosure:
			return 0
		}
		return closeErr.Code
	}
	return 0
}

// osName and archName report the values hello sends.
func osName() string   { return goruntime.GOOS }
func archName() string { return goruntime.GOARCH }
