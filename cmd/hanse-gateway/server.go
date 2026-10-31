package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/achmadss/hanse/internal/agent"
	"github.com/achmadss/hanse/internal/agentconn"
	"github.com/achmadss/hanse/internal/api"
	"github.com/achmadss/hanse/internal/auth"
	"github.com/achmadss/hanse/internal/config"
	"github.com/achmadss/hanse/internal/enroll"
	herrors "github.com/achmadss/hanse/internal/errors"
	"github.com/achmadss/hanse/internal/history"
	"github.com/achmadss/hanse/internal/operations"
	"github.com/achmadss/hanse/internal/registry"
	"github.com/achmadss/hanse/internal/version"
)

// shutdownTimeout bounds the wait for in-flight requests when the Gateway
// stops. The default grace period is 10 seconds; a zero or negative value
// falls back to it.
func shutdownTimeout(cfg *config.GatewayConfig) time.Duration {
	if cfg.Timeouts.GracePeriod != nil && cfg.Timeouts.GracePeriod.Value > 0 {
		return cfg.Timeouts.GracePeriod.Value
	}
	return config.DefaultTimeouts().GracePeriod
}

// startOptions carries the test seams of startGateway. Production leaves
// both zero: the configuration supplies the listen address and the log
// goes to standard error.
type startOptions struct {
	// listen overrides gateway.yaml's listen address. Tests use it to bind
	// 127.0.0.1:0 and learn the port from the returned Gateway.
	listen string
	// stderr receives the structured log. Nil means os.Stderr.
	stderr io.Writer
}

// gateway is a started Gateway: its bound address, its shutdown hook, and
// the channel that closes when the process stops serving.
type gateway struct {
	// Addr is the bound listen address, with the real port when the
	// configuration asked for 0.
	Addr string

	ready atomic.Bool
	log   *slog.Logger
	// reg is the live capability registry a configuration reload updates.
	reg    *registry.Registry
	server *http.Server
	hub    *agentconn.Hub
	store  *history.Store
	enroll *enroll.Service
	grace  time.Duration
	done   chan error
	// closeOnce keeps a second shutdown from closing the stores twice.
	closeOnce atomic.Bool
}

// Done reports the result of serving: nil after a clean shutdown, an error
// when the listener failed.
func (g *gateway) Done() <-chan error { return g.done }

// readyz reports the readiness value the /readyz route serves.
func (g *gateway) readyz() bool { return g.ready.Load() }

// startGateway loads and validates the configuration, opens the stores,
// loads the registries, builds every component, and starts serving. It
// refuses to start on any configuration problem, on a plain-HTTP listener
// that is not loopback, and on any failure while opening a store or a
// registry. The returned Gateway stops when ctx is cancelled.
func startGateway(ctx context.Context, configPath string, opts startOptions) (*gateway, error) {
	cfg, problems, err := config.Load(configPath)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", configPath, err)
	}
	if len(problems) > 0 {
		return nil, configProblems(configPath, problems)
	}

	listen := *cfg.Gateway.Listen
	if opts.listen != "" {
		listen = opts.listen
	}
	if err := checkTransport(listen, cfg.Gateway); err != nil {
		return nil, err
	}

	stderr := opts.stderr
	if stderr == nil {
		stderr = os.Stderr
	}
	log := newLogger(stderr, cfg.Gateway.Log)
	grace := shutdownTimeout(&cfg.Gateway)

	store, err := history.Open(history.Options{
		StateDir:  cfg.Gateway.Paths.StateDir,
		Retention: durationValue(cfg.Gateway.History.Retention),
	})
	if err != nil {
		return nil, err
	}
	fail := func(cause error) (*gateway, error) {
		_ = store.Close()
		return nil, cause
	}

	reg, warnings, err := registry.Load(registry.Options{
		Dirs:  cfg.Gateway.Paths.Capabilities,
		Input: registryInput(cfg),
		// A reload re-reads nodes.yaml and routes.yaml from the paths this
		// gateway.yaml resolved at startup; gateway.yaml itself is not
		// re-read (docs/architecture.md section 11.2).
		ReloadInput: func() (registry.Input, error) {
			reloaded, problems, err := config.LoadNodesRoutes(&cfg.Gateway)
			if err != nil {
				return registry.Input{}, err
			}
			if len(problems) > 0 {
				return registry.Input{}, problems
			}
			return registryInput(reloaded), nil
		},
	})
	if err != nil {
		return fail(err)
	}
	for _, warning := range warnings {
		log.Warn("capability registry warning",
			slog.String("capability", warning.Capability),
			slog.String("message", warning.Message))
	}

	timeouts := timeoutsOf(&cfg.Gateway)
	// The embedded agent runs the host node's exec handlers in this process.
	// With embedded_agent: false the Gateway runs none of them: they go to
	// the host node's hanse-node agent like any other node's, and http
	// capabilities still run here because the Gateway makes those calls
	// itself (docs/architecture.md §8).
	embeddedAgent := cfg.Gateway.EmbeddedAgentEnabled()
	var embedded *agent.Agent
	if embeddedAgent {
		embedded, err = agent.New(agent.Config{
			Node:           embeddedNode(cfg),
			Registry:       reg,
			ResponseLimit:  sizeValue(cfg.Gateway.Limits.ResponseSize),
			GracePeriod:    durationValue(cfg.Gateway.Timeouts.GracePeriod),
			OptionsTimeout: durationValue(cfg.Gateway.Timeouts.Options),
			EnvAllow:       cfg.Gateway.Handlers.EnvAllow,
			WorkRoot:       cfg.Gateway.Paths.StateDir,
		})
		if err != nil {
			return fail(err)
		}
	}
	var execDispatch *operations.ExecDispatcher
	if embedded != nil {
		execDispatch, err = operations.NewExecDispatcher(operations.ExecConfig{Registry: reg, Agent: embedded})
		if err != nil {
			return fail(err)
		}
	}
	httpDispatch, err := operations.NewHTTPDispatcher(operations.HTTPConfig{
		Registry:       reg,
		CredentialsDir: cfg.Gateway.Paths.CredentialsDir,
		ResponseSize:   sizeValue(cfg.Gateway.Limits.ResponseSize),
		OptionsTimeout: durationValue(cfg.Gateway.Timeouts.Options),
	})
	if err != nil {
		return fail(err)
	}

	// The enroll service and the hub refer to each other: the hub verifies
	// node credentials through the service, and the service closes a live
	// connection through the hub after a revoke. The hub is built first
	// with a closure over a variable the service fills in below.
	var enrollService *enroll.Service
	hubLogs := &lateLogSink{}
	hub, err := agentconn.New(agentconn.Config{
		Registry:       reg,
		GatewayVersion: version.Version,
		Verify: func(credential string) (string, error) {
			if enrollService == nil {
				return "", herrors.New(herrors.AuthenticationFailed, "", nil)
			}
			return enrollService.Verify(credential)
		},
		HeartbeatInterval: durationValue(cfg.Gateway.Agents.HeartbeatInterval),
		HeartbeatTimeout:  durationValue(cfg.Gateway.Agents.HeartbeatTimeout),
		ReconnectWindow:   durationValue(cfg.Gateway.Agents.ReconnectWindow),
		GracePeriod:       durationValue(cfg.Gateway.Timeouts.GracePeriod),
		LogLineBytes:      int(sizeValue(cfg.Gateway.Limits.LogLine)),
		Logs:              hubLogs,
	})
	if err != nil {
		return fail(err)
	}
	// A reload that removes a node disconnects its agent, because the node
	// no longer exists in nodes.yaml (docs/architecture.md section 8.2).
	reg.SetNodeRemovedHook(hub.NodeRemoved)
	agentDispatcher := operations.NewAgentDispatcher(hub, reg, timeouts)
	dispatch := &dispatcher{
		registry: reg,
		host:     cfg.Gateway.Node,
		embedded: embeddedAgent,
		exec:     execDispatch,
		http:     httpDispatch,
		agent:    agentDispatcher,
	}
	manager, err := operations.New(operations.Config{
		Registry:     reg,
		Dispatcher:   dispatch,
		Timeouts:     timeouts,
		Store:        store,
		LogStore:     store,
		LogLimit:     int(sizeValue(cfg.Gateway.Limits.LogTotal)),
		LogLineLimit: int(sizeValue(cfg.Gateway.Limits.LogLine)),
	})
	if err != nil {
		return fail(err)
	}
	// The hub records agent log lines in the same sink the manager uses, so
	// a line of a remote run is redacted, stored, and followed exactly like
	// a line of a run on the Gateway host.
	hubLogs.set(manager)

	enrollService, err = enroll.Open(enroll.Options{
		StateDir:     cfg.Gateway.Paths.StateDir,
		JoinTokenTTL: durationValue(cfg.Gateway.Agents.JoinTokenTTL),
		Disconnect:   hub,
	})
	if err != nil {
		return fail(err)
	}
	failAll := func(cause error) (*gateway, error) {
		_ = enrollService.Close()
		return fail(cause)
	}

	tokens, err := auth.LoadStore(cfg.Gateway.Auth.TokensFile)
	if err != nil {
		return failAll(err)
	}

	g := &gateway{
		ready:  atomic.Bool{},
		log:    log,
		reg:    reg,
		hub:    hub,
		store:  store,
		enroll: enrollService,
		grace:  grace,
		done:   make(chan error, 1),
	}
	// The API refuses a node agent for the host node only when the embedded
	// agent owns that node's exec handlers. With embedded_agent: false the
	// host node enrolls and connects like any other node.
	embeddedNodeID := ""
	if embeddedAgent {
		embeddedNodeID = cfg.Gateway.Node
	}
	handler := api.New(reg, manager, enrollService,
		api.WithAgentHub(hub),
		api.WithEmbeddedNode(embeddedNodeID),
		// Exec option providers follow the same executor routing as an
		// operation, so they run in process only for the embedded node and
		// on the executor's agent otherwise (docs/handler-contract.md §8).
		api.WithOptionProviders(api.OptionProviders{Exec: dispatch, HTTP: httpDispatch}),
		api.WithReadiness(g.readyz))
	g.server = &http.Server{
		Handler:           guard(tokens, handler),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}

	listener, err := net.Listen("tcp", listen)
	if err != nil {
		return failAll(err)
	}
	g.Addr = listener.Addr().String()
	useTLS := tlsConfigured(&cfg.Gateway)
	if useTLS {
		// Load the certificate before the Gateway reports ready, so a
		// wrong path or a corrupt key refuses startup instead of failing
		// on the first client.
		certificate, err := tls.LoadX509KeyPair(cfg.Gateway.TLS.CertFile, cfg.Gateway.TLS.KeyFile)
		if err != nil {
			_ = listener.Close()
			return failAll(fmt.Errorf("load the TLS certificate: %w", err))
		}
		g.server.TLSConfig = &tls.Config{
			Certificates: []tls.Certificate{certificate},
			MinVersion:   tls.VersionTLS12,
		}
	}

	serveErr := make(chan error, 1)
	go func() {
		var err error
		if useTLS {
			err = g.server.ServeTLS(listener, "", "")
		} else {
			err = g.server.Serve(listener)
		}
		serveErr <- err
	}()
	g.ready.Store(true)
	log.Info("gateway listening",
		slog.String("address", g.Addr),
		slog.String("public_url", cfg.Gateway.PublicURL),
		slog.String("host_node", cfg.Gateway.Node),
		slog.Bool("embedded_agent", embeddedAgent),
		slog.Bool("tls", useTLS))

	go func() {
		select {
		case err := <-serveErr:
			g.ready.Store(false)
			g.closeStores()
			if errors.Is(err, http.ErrServerClosed) {
				err = nil
			}
			g.done <- err
		case <-ctx.Done():
			g.shutdown()
			g.done <- nil
		}
	}()
	return g, nil
}

// reloadConfig re-reads the capability directories, nodes.yaml, and
// routes.yaml and swaps the validated result into the running Gateway. A
// problem keeps the running configuration and every reported problem goes
// to the log (docs/architecture.md section 11.2). SIGHUP calls it.
func (g *gateway) reloadConfig() {
	if g == nil || g.reg == nil {
		return
	}
	warnings, err := g.reg.Reload()
	if err != nil {
		g.log.Error("configuration reload failed; keeping the running configuration",
			slog.String("error", err.Error()))
		return
	}
	for _, warning := range warnings {
		g.log.Warn("capability registry warning",
			slog.String("capability", warning.Capability),
			slog.String("message", warning.Message))
	}
	g.log.Info("configuration reloaded",
		slog.Int("capabilities", len(g.reg.Capabilities())))
}

// shutdown stops accepting requests, waits for in-flight ones up to the
// configured grace period, marks the operations that were still queued or
// running as failed with the gateway_restart reason (docs/architecture.md
// section 9.6), and closes the stores.
func (g *gateway) shutdown() {
	g.ready.Store(false)
	ctx, cancel := context.WithTimeout(context.Background(), g.grace)
	defer cancel()
	if err := g.server.Shutdown(ctx); err != nil {
		g.log.Warn("graceful shutdown timed out; closing connections",
			slog.String("error", err.Error()))
		_ = g.server.Close()
	}
	if n, err := g.store.RecoverInterrupted(time.Now().UTC()); err != nil {
		g.log.Error("cannot mark the interrupted operations", slog.String("error", err.Error()))
	} else if n > 0 {
		g.log.Info("marked interrupted operations as failed",
			slog.Int("operations", n),
			slog.String("code", herrors.GatewayRestart.Name()))
	}
	g.closeStores()
	g.log.Info("gateway stopped")
}

// closeStores closes the history and enrollment stores once.
func (g *gateway) closeStores() {
	if !g.closeOnce.CompareAndSwap(false, true) {
		return
	}
	if err := g.store.Close(); err != nil {
		g.log.Error("cannot close the history store", slog.String("error", err.Error()))
	}
	if err := g.enroll.Close(); err != nil {
		g.log.Error("cannot close the enrollment store", slog.String("error", err.Error()))
	}
}

// guard applies the bearer-token middleware to every route except the
// routes that authenticate themselves or need no authentication: the two
// probes, the agent join and connect routes, and the bundle download, which
// checks the node credential itself (docs/architecture.md sections 3 and
// 15).
func guard(tokens *auth.Store, next http.Handler) http.Handler {
	authenticated := tokens.Middleware(next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if publicPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		authenticated.ServeHTTP(w, r)
	})
}

// publicPath reports whether a route authenticates itself or needs no
// authentication.
func publicPath(path string) bool {
	switch path {
	case "/healthz", "/readyz", "/v1/agent/join", "/v1/agent/connect":
		return true
	}
	return strings.HasPrefix(path, "/v1/agent/bundles/")
}

// checkTransport refuses a plain-HTTP listener on a non-loopback address.
// TLS, a loopback address, or an explicit allow_insecure_http setting makes
// the address acceptable (docs/architecture.md section 15 and
// docs/configuration.md section 3.2).
func checkTransport(listen string, cfg config.GatewayConfig) error {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return fmt.Errorf("listen address %q is not a host:port address", listen)
	}
	if tlsConfigured(&cfg) || cfg.AllowInsecureHTTP || config.IsLoopbackHost(host) {
		return nil
	}
	return fmt.Errorf("refusing to listen on %q without TLS: set tls.cert_file and tls.key_file, use a loopback address, or set allow_insecure_http behind a TLS reverse proxy", listen)
}

// tlsConfigured reports whether both TLS files are set.
func tlsConfigured(cfg *config.GatewayConfig) bool {
	return cfg.TLS != nil && cfg.TLS.CertFile != "" && cfg.TLS.KeyFile != ""
}

// timeoutsOf converts the configured timeouts to the operations shape.
func timeoutsOf(cfg *config.GatewayConfig) config.Timeouts {
	return config.Timeouts{
		Operation:   durationValue(cfg.Timeouts.Operation),
		Options:     durationValue(cfg.Timeouts.Options),
		GracePeriod: durationValue(cfg.Timeouts.GracePeriod),
	}
}

// durationValue unwraps a configured duration; nil means zero, which every
// consumer replaces with its documented default.
func durationValue(d *config.Duration) time.Duration {
	if d == nil {
		return 0
	}
	return d.Value
}

// sizeValue unwraps a configured size; nil means zero, which every consumer
// replaces with its documented default.
func sizeValue(s *config.Size) int64 {
	if s == nil {
		return 0
	}
	return s.Value
}

// newLogger writes the Gateway log in the configured level and format.
func newLogger(w io.Writer, cfg config.GatewayLog) *slog.Logger {
	level := slog.LevelInfo
	switch cfg.Level {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	options := &slog.HandlerOptions{Level: level}
	if cfg.Format == "json" {
		return slog.New(slog.NewJSONHandler(w, options))
	}
	return slog.New(slog.NewTextHandler(w, options))
}

// configProblems joins every problem into one error, so the operator sees
// all of them at once, exactly as --check-config prints them.
func configProblems(path string, problems config.Problems) error {
	lines := make([]string, 0, len(problems))
	for _, problem := range problems {
		lines = append(lines, problem.Error())
	}
	return fmt.Errorf("%s: %d configuration problem(s)\n%s",
		path, len(problems), strings.Join(lines, "\n"))
}
