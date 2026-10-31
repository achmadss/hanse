package config

import (
	"errors"
	"net"
	"net/url"
	"strings"
	"time"
)

func strPtr(s string) *string { return &s }

func boolPtr(b bool) *bool { return &b }

var errSyntax = errors.New("not an address")

// GatewayConfig is the parsed gateway.yaml (docs/configuration.md §3).
// Fields with a documented default stay nil or empty until the loader's
// finalize pass fills them, so an explicitly configured value and the
// default are always distinguishable.
type GatewayConfig struct {
	// Version is the file format version; it must be 1.
	Version int
	// Node is the node ID in nodes.yaml that is the Gateway host. Its http
	// capabilities always route to this node, and the Gateway makes those
	// calls itself.
	Node string
	// EmbeddedAgent runs the host node's exec handlers inside the Gateway
	// process. It defaults to true. False runs them on the host node's
	// hanse-node agent instead, so the Gateway runs no exec handler of its
	// own (docs/configuration.md §3.2).
	EmbeddedAgent *bool `yaml:"embedded_agent"`
	// PublicURL is the URL that clients and node agents use to reach the
	// Gateway.
	PublicURL string `yaml:"public_url"`
	// Listen is the address the API listens on. Default 127.0.0.1:8443.
	Listen *string
	// TLS holds the TLS certificate and key paths. Pointers keep the
	// "set both or neither" rule exact.
	TLS *GatewayTLS
	// AllowInsecureHTTP allows plain HTTP on a non-loopback address.
	AllowInsecureHTTP bool `yaml:"allow_insecure_http"`
	// Paths holds the nodes, routes, capabilities, credentials, and state
	// locations, resolved to absolute paths during finalize.
	Paths GatewayPaths
	// Auth holds the authentication settings.
	Auth GatewayAuth
	// History holds history settings.
	History GatewayHistory
	// Timeouts holds the default timeouts.
	Timeouts GatewayTimeouts
	// Limits holds response and log size limits.
	Limits GatewayLimits
	// Handlers holds handler environment settings.
	Handlers GatewayHandlers
	// Agents holds node agent settings.
	Agents GatewayAgents
	// Log holds logging settings.
	Log GatewayLog
}

// GatewayTLS holds the TLS certificate and key paths.
type GatewayTLS struct {
	// CertFile is the TLS certificate, in PEM format.
	CertFile string `yaml:"cert_file"`
	// KeyFile is the TLS private key, in PEM format.
	KeyFile string `yaml:"key_file"`
}

// GatewayPaths holds path settings. After loading, every value is
// absolute: relative paths resolve from the directory of gateway.yaml.
type GatewayPaths struct {
	// Nodes is the nodes file. Default nodes.yaml next to gateway.yaml.
	Nodes string
	// Routes is the routes file. Default routes.yaml next to gateway.yaml.
	Routes string
	// Capabilities lists directories to scan for capabilities. Required.
	Capabilities []string
	// CredentialsDir is the directory of secret files. Required.
	CredentialsDir string `yaml:"credentials_dir"`
	// StateDir is the directory for the database. Required.
	StateDir string `yaml:"state_dir"`
}

// GatewayAuth holds authentication settings.
type GatewayAuth struct {
	// TokensFile holds the API token hashes, managed by hanse-gateway
	// token. Default <state_dir>/tokens.json.
	TokensFile string `yaml:"tokens_file"`
}

// GatewayHistory holds history settings.
type GatewayHistory struct {
	// Retention is how long operation records and logs are kept.
	// Default 30d.
	Retention *Duration
}

// GatewayTimeouts holds default timeout settings.
type GatewayTimeouts struct {
	// Operation is the timeout of an operation whose manifest sets none.
	// Default 5m.
	Operation *Duration
	// Options is the timeout of an option provider call. Default 10s.
	Options *Duration
	// GracePeriod is the time between a stop request and the kill.
	// Default 10s.
	GracePeriod *Duration `yaml:"grace_period"`
}

// GatewayLimits holds size limit settings.
type GatewayLimits struct {
	// ResponseSize is the largest response a handler may return.
	// Default 1MiB.
	ResponseSize *Size `yaml:"response_size"`
	// LogLine is the longest log line before the line is split.
	// Default 16KiB.
	LogLine *Size `yaml:"log_line"`
	// LogTotal is the most log data stored for one operation.
	// Default 10MiB.
	LogTotal *Size `yaml:"log_total"`
	// Summary is the longest summary kept from a response. Default 1KiB.
	Summary *Size
	// ErrorMessage is the longest error.message kept from a response.
	// Default 4KiB.
	ErrorMessage *Size `yaml:"error_message"`
}

// GatewayHandlers holds handler environment settings.
type GatewayHandlers struct {
	// EnvAllow lists extra environment variables passed to handlers on
	// the Gateway host.
	EnvAllow []string `yaml:"env_allow"`
}

// GatewayAgents holds node agent settings.
type GatewayAgents struct {
	// JoinTokenTTL is how long a join token stays valid. Default 15m.
	JoinTokenTTL *Duration `yaml:"join_token_ttl"`
	// ReconnectWindow is how long the Gateway waits for a disconnected
	// agent. Default 5m.
	ReconnectWindow *Duration `yaml:"reconnect_window"`
	// HeartbeatInterval is how often heartbeats are sent. Default 15s.
	HeartbeatInterval *Duration `yaml:"heartbeat_interval"`
	// HeartbeatTimeout is how long a side tolerates silence.
	// Default 45s. Must be at least twice HeartbeatInterval.
	HeartbeatTimeout *Duration `yaml:"heartbeat_timeout"`
}

// GatewayLog holds logging settings.
type GatewayLog struct {
	// Level is debug, info, warn, or error. Default info.
	Level string
	// Format is text or json. Default text.
	Format string
}

// finalizeGateway fills every documented default and resolves relative
// paths against baseDir, the directory containing gateway.yaml.
func finalizeGateway(g *GatewayConfig, baseDir string) {
	defaultString := func(dst **string, v string) {
		if *dst == nil {
			*dst = strPtr(v)
		}
	}
	defaultString(&g.Listen, "127.0.0.1:8443")

	if g.Paths.Nodes == "" {
		g.Paths.Nodes = "nodes.yaml"
	}
	if g.Paths.Routes == "" {
		g.Paths.Routes = "routes.yaml"
	}
	g.Paths.Nodes = resolvePath(baseDir, g.Paths.Nodes)
	g.Paths.Routes = resolvePath(baseDir, g.Paths.Routes)
	addRel := func(p string) string { return resolvePath(baseDir, p) }
	for i, capDir := range g.Paths.Capabilities {
		g.Paths.Capabilities[i] = addRel(capDir)
	}
	g.Paths.CredentialsDir = addRel(g.Paths.CredentialsDir)
	g.Paths.StateDir = addRel(g.Paths.StateDir)

	if g.Auth.TokensFile == "" {
		g.Auth.TokensFile = resolvePath(g.Paths.StateDir, "tokens.json")
	} else {
		g.Auth.TokensFile = addRel(g.Auth.TokensFile)
	}

	if g.History.Retention == nil {
		g.History.Retention = &Duration{Value: 30 * 24 * time.Hour}
	}
	if g.Timeouts.Operation == nil {
		g.Timeouts.Operation = &Duration{Value: DefaultTimeouts().Operation}
	}
	if g.Timeouts.Options == nil {
		g.Timeouts.Options = &Duration{Value: DefaultTimeouts().Options}
	}
	if g.Timeouts.GracePeriod == nil {
		g.Timeouts.GracePeriod = &Duration{Value: DefaultTimeouts().GracePeriod}
	}
	if g.Limits.ResponseSize == nil {
		g.Limits.ResponseSize = &Size{Value: 1 << 20}
	}
	if g.Limits.LogLine == nil {
		g.Limits.LogLine = &Size{Value: 16 << 10}
	}
	if g.Limits.LogTotal == nil {
		g.Limits.LogTotal = &Size{Value: 10 << 20}
	}
	if g.Limits.Summary == nil {
		g.Limits.Summary = &Size{Value: 1 << 10}
	}
	if g.Limits.ErrorMessage == nil {
		g.Limits.ErrorMessage = &Size{Value: 4 << 10}
	}
	if g.Agents.JoinTokenTTL == nil {
		g.Agents.JoinTokenTTL = &Duration{Value: 15 * time.Minute}
	}
	if g.Agents.ReconnectWindow == nil {
		g.Agents.ReconnectWindow = &Duration{Value: 5 * time.Minute}
	}
	if g.Agents.HeartbeatInterval == nil {
		g.Agents.HeartbeatInterval = &Duration{Value: 15 * time.Second}
	}
	if g.Agents.HeartbeatTimeout == nil {
		g.Agents.HeartbeatTimeout = &Duration{Value: 45 * time.Second}
	}
	if g.EmbeddedAgent == nil {
		g.EmbeddedAgent = boolPtr(true)
	}
	if g.Log.Level == "" {
		g.Log.Level = "info"
	}
	if g.Log.Format == "" {
		g.Log.Format = "text"
	}
}

// EmbeddedAgentEnabled reports whether the Gateway runs the host node's
// exec handlers inside its own process. The loader always fills the field;
// a nil value means the documented default, true.
func (g *GatewayConfig) EmbeddedAgentEnabled() bool {
	return g.EmbeddedAgent == nil || *g.EmbeddedAgent
}

// checkGateway validates gateway.yaml alone. Name is the file's path as
// given on the command line; every problem carries it. The node checks
// that need nodes.yaml run later, in Load.
func checkGateway(g *GatewayConfig, file string, problems *Problems) {
	if g.Version != 1 {
		*problems = append(*problems, Problem{file, "version", "must be 1"})
	}
	// `node` is required: it names the Gateway host node, which is the only
	// eligible executor of an `http` capability. The embedded agent is the
	// separate, optional executor for that node.
	if g.Node == "" {
		*problems = append(*problems, Problem{file, "node", "is required"})
	} else if !ValidID(g.Node) {
		*problems = append(*problems, Problem{file, "node", "must be a valid node ID: lowercase letters, digits, \"_\" and \"-\", starting with a letter"})
	}
	checkPublicURL(g, file, problems)
	checkListen(g, file, problems)
	if g.TLS != nil && (g.TLS.CertFile == "") != (g.TLS.KeyFile == "") {
		*problems = append(*problems, Problem{file, "tls", "set both tls.cert_file and tls.key_file, or neither"})
	}
	if len(g.Paths.Capabilities) == 0 {
		*problems = append(*problems, Problem{file, "paths.capabilities", "is required: at least one directory"})
	}
	if g.Paths.CredentialsDir == "" {
		*problems = append(*problems, Problem{file, "paths.credentials_dir", "is required"})
	}
	if g.Paths.StateDir == "" {
		*problems = append(*problems, Problem{file, "paths.state_dir", "is required"})
	}
	switch g.Log.Level {
	case "", "debug", "info", "warn", "error":
	default:
		*problems = append(*problems, Problem{file, "log.level", `must be one of debug, info, warn, or error`})
	}
	switch g.Log.Format {
	case "", "text", "json":
	default:
		*problems = append(*problems, Problem{file, "log.format", `must be text or json`})
	}
	if interval, timeout := g.Agents.HeartbeatInterval, g.Agents.HeartbeatTimeout; interval != nil && timeout != nil && timeout.Value < 2*interval.Value {
		*problems = append(*problems, Problem{file, "agents.heartbeat_timeout",
			"must be at least twice agents.heartbeat_interval"})
	}
}

// checkPublicURL validates public_url: a parseable URL that uses https,
// unless its host is a loopback address (docs/configuration.md §3.2).
func checkPublicURL(g *GatewayConfig, file string, problems *Problems) {
	const field = "public_url"
	if g.PublicURL == "" {
		*problems = append(*problems, Problem{file, field, "is required"})
		return
	}
	u, err := url.Parse(g.PublicURL)
	if err != nil || u.Hostname() == "" {
		*problems = append(*problems, Problem{file, field, "is not a valid URL"})
		return
	}
	// The scheme must be https, or http on a loopback host
	// (docs/configuration.md §3.2).
	if u.Scheme == "https" || u.Scheme == "http" && IsLoopbackHost(u.Hostname()) {
		return
	}
	*problems = append(*problems, Problem{file, field,
		"must use https, unless its host is a loopback address"})
}

// checkListen validates listen: a host:port address that is loopback, or
// allow_insecure_http is set, or TLS is configured (docs/configuration.md
// §3.2).
func checkListen(g *GatewayConfig, file string, problems *Problems) {
	const field = "listen"
	host, port, err := splitHostPort(*g.Listen)
	if err != nil {
		*problems = append(*problems, Problem{file, field, "must be a host:port address"})
		return
	}
	if port == "" || port == "0" {
		*problems = append(*problems, Problem{file, field, "needs a port"})
	}
	if g.AllowInsecureHTTP || g.TLS != nil && g.TLS.CertFile != "" && g.TLS.KeyFile != "" {
		return
	}
	if !IsLoopbackHost(host) {
		p := Problem{file, field,
			"must be a loopback address, because there is no TLS: set tls.cert_file and tls.key_file, or set allow_insecure_http"}
		*problems = append(*problems, p)
	}
}

// splitHostPort splits host:port and reports whether the split worked.
// Unlike net.SplitHostPort it also accepts port-less hosts, so the error
// message can name the missing part specifically.
func splitHostPort(s string) (host, port string, err error) {
	if s == "" {
		return "", "", errSyntax
	}
	if i := strings.LastIndex(s, "]"); i >= 0 {
		if !strings.HasPrefix(s, "[") {
			return "", "", errSyntax
		}
		host, s = s[1:i], s[i+1:]
		if !strings.HasPrefix(s, ":") {
			if s != "" {
				return "", "", errSyntax
			}
			return host, "", nil
		}
		return host, s[1:], nil
	}
	i := strings.LastIndex(s, ":")
	if i < 0 {
		return s, "", nil
	}
	return s[:i], s[i+1:], nil
}

// IsLoopbackHost reports whether host is a loopback address: 127.0.0.0/8,
// ::1, or the name localhost. It backs every loopback rule the Gateway
// applies, including the API's transport gate (architecture.md §15).
func IsLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}
