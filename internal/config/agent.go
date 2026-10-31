package config

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// AgentConfig is the parsed agent.yaml (docs/configuration.md section 7).
// Fields with a documented default stay nil or empty until the loader's
// finalize pass fills them, so an explicitly configured value and the
// default are always distinguishable.
type AgentConfig struct {
	// Version is the file format version; it must be 1.
	Version int
	// GatewayURL is the Gateway's public_url. It must use https, unless
	// its host is a loopback address.
	GatewayURL string `yaml:"gateway_url"`
	// CAFile is an extra CA certificate to trust for the Gateway, in PEM
	// format. Empty means the system roots.
	CAFile string `yaml:"ca_file"`
	// StateDir holds the node credential, the run journal, the handler
	// cache, and scratch directories. Default /var/lib/hanse-node, or
	// C:\ProgramData\Hanse\node\state on Windows.
	StateDir string `yaml:"state_dir"`
	// MaxConcurrentRuns is the most handlers that run at the same time.
	// Default 16.
	MaxConcurrentRuns *int `yaml:"max_concurrent_runs"`
	// BufferLimit is the most unacknowledged log data held while
	// disconnected. Default 10MiB.
	BufferLimit *Size `yaml:"buffer_limit"`
	// Handlers holds handler environment settings.
	Handlers AgentHandlers
	// Elevation holds the privileged-operation settings.
	Elevation AgentElevation
	// Log holds logging settings.
	Log AgentLog
}

// AgentHandlers holds handler environment settings.
type AgentHandlers struct {
	// EnvAllow lists extra environment variables passed to handlers on
	// this node.
	EnvAllow []string `yaml:"env_allow"`
}

// AgentElevation holds the privileged-operation settings of one node.
type AgentElevation struct {
	// Enabled allows privileged operations on this node.
	Enabled bool
	// Operations allows only these `<cap>.<op>` operations. An empty
	// list allows all of them.
	Operations []string
	// Account is the Windows administrator account whose password
	// authorizes elevation. It is required on Windows when Enabled is
	// set and unused on Linux and macOS.
	Account string
	// Lockout holds the wrong-password lockout settings.
	Lockout AgentLockout
}

// AgentLockout holds the elevation lockout settings.
type AgentLockout struct {
	// Attempts is the number of wrong passwords allowed within Window.
	// Default 5.
	Attempts *int
	// Window is the period in which wrong passwords are counted.
	// Default 10m.
	Window *Duration
	// Duration is how long elevation stays refused after too many wrong
	// passwords. Default 10m.
	Duration *Duration
}

// AgentLog holds the node agent's logging settings.
type AgentLog struct {
	// Level is debug, info, warn, or error. Default info.
	Level string
	// Format is text or json. Default text.
	Format string
}

// LoadAgent reads and validates one agent.yaml. It returns the
// configuration and every problem found; problems is empty only when the
// configuration is valid. Relative paths resolve from the file's directory
// (docs/configuration.md sections 2 and 7).
func LoadAgent(path string) (*AgentConfig, Problems, error) {
	problems := Problems{}
	agent := &AgentConfig{}
	if !decodeFile(path, agent, &problems) {
		return nil, problems, nil
	}
	finalizeAgent(agent, filepath.Dir(path))
	checkAgent(agent, path, &problems)
	return agent, problems, nil
}

// AgentStateDir returns the state_dir that agent.yaml names, resolved
// against the file's directory like every other path in the file
// (docs/configuration.md section 7). The second result reports whether the
// file names one at all.
//
// It reads only state_dir, so `hanse-node join` can use a file that is not
// complete yet: a file without gateway_url is one that join is about to
// finish. A missing file names no state directory.
func AgentStateDir(path string) (string, bool, error) {
	if path == "" {
		return "", false, nil
	}
	data, err := os.ReadFile(path)
	switch {
	case os.IsNotExist(err):
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("cannot read %s: %v", path, err)
	}
	var field struct {
		StateDir string `yaml:"state_dir"`
	}
	if err := yaml.Unmarshal(data, &field); err != nil {
		return "", false, fmt.Errorf("cannot read %s: %v", path, err)
	}
	if field.StateDir == "" {
		return "", false, nil
	}
	return resolvePath(filepath.Dir(path), field.StateDir), true, nil
}

// defaultAgentStateDir is the documented state directory per OS
// (docs/configuration.md section 7.1).
func defaultAgentStateDir() string {
	if runtime.GOOS == "windows" {
		return `C:\ProgramData\Hanse\node\state`
	}
	return "/var/lib/hanse-node"
}

// finalizeAgent fills every documented default and resolves relative paths
// against baseDir, the directory containing agent.yaml.
func finalizeAgent(a *AgentConfig, baseDir string) {
	if a.StateDir == "" {
		a.StateDir = defaultAgentStateDir()
	}
	a.StateDir = resolvePath(baseDir, a.StateDir)
	if a.CAFile != "" {
		a.CAFile = resolvePath(baseDir, a.CAFile)
	}
	if a.MaxConcurrentRuns == nil {
		maximum := 16
		a.MaxConcurrentRuns = &maximum
	}
	if a.BufferLimit == nil {
		a.BufferLimit = &Size{Value: 10 << 20}
	}
	if a.Elevation.Lockout.Attempts == nil {
		attempts := 5
		a.Elevation.Lockout.Attempts = &attempts
	}
	if a.Elevation.Lockout.Window == nil {
		a.Elevation.Lockout.Window = &Duration{Value: 10 * time.Minute}
	}
	if a.Elevation.Lockout.Duration == nil {
		a.Elevation.Lockout.Duration = &Duration{Value: 10 * time.Minute}
	}
	if a.Log.Level == "" {
		a.Log.Level = "info"
	}
	if a.Log.Format == "" {
		a.Log.Format = "text"
	}
}

// checkAgent validates agent.yaml alone. Name is the file's path as given
// on the command line; every problem carries it.
func checkAgent(a *AgentConfig, file string, problems *Problems) {
	if a.Version != 1 {
		*problems = append(*problems, Problem{file, "version", "must be 1"})
	}
	checkGatewayURL(a, file, problems)
	if a.MaxConcurrentRuns != nil && *a.MaxConcurrentRuns < 1 {
		*problems = append(*problems, Problem{file, "max_concurrent_runs", "must be at least 1"})
	}
	if a.BufferLimit != nil && a.BufferLimit.Value < 1 {
		*problems = append(*problems, Problem{file, "buffer_limit", "must be at least 1B"})
	}
	for _, name := range a.Handlers.EnvAllow {
		if name == "" || strings.ContainsAny(name, "=\x00") {
			*problems = append(*problems, Problem{file,
				"handlers.env_allow", "must list environment variable names"})
			break
		}
	}
	for _, operation := range a.Elevation.Operations {
		if !operationPattern.MatchString(operation) {
			*problems = append(*problems, Problem{file, "elevation.operations",
				"must list <capability>.<operation> names, got " + operation})
		}
	}
	if a.Elevation.Enabled && runtime.GOOS == "windows" && a.Elevation.Account == "" {
		*problems = append(*problems, Problem{file, "elevation.account",
			"is required on Windows when elevation is enabled"})
	}
	if a.Elevation.Lockout.Attempts != nil && *a.Elevation.Lockout.Attempts < 1 {
		*problems = append(*problems, Problem{file, "elevation.lockout.attempts", "must be at least 1"})
	}
	checkAgentDurations(a, file, problems)
	switch a.Log.Level {
	case "", "debug", "info", "warn", "error":
	default:
		*problems = append(*problems, Problem{file, "log.level", `must be one of debug, info, warn, or error`})
	}
	switch a.Log.Format {
	case "", "text", "json":
	default:
		*problems = append(*problems, Problem{file, "log.format", `must be text or json`})
	}
	checkAgentFiles(a, file, problems)
}

// checkAgentDurations rejects a zero or negative lockout duration, which
// would lock the node out forever or never count a wrong password.
func checkAgentDurations(a *AgentConfig, file string, problems *Problems) {
	if a.Elevation.Lockout.Window != nil && a.Elevation.Lockout.Window.Value < 1 {
		*problems = append(*problems, Problem{file, "elevation.lockout.window", "must be positive"})
	}
	if a.Elevation.Lockout.Duration != nil && a.Elevation.Lockout.Duration.Value < 1 {
		*problems = append(*problems, Problem{file, "elevation.lockout.duration", "must be positive"})
	}
}

// checkAgentFiles checks the paths an agent.yaml names: the file must stay
// under the administrator's control, and the CA file must be readable.
func checkAgentFiles(a *AgentConfig, file string, problems *Problems) {
	if runtime.GOOS != "windows" {
		if info, err := os.Stat(file); err == nil && info.Mode().Perm()&0o022 != 0 {
			*problems = append(*problems, Problem{file, "file",
				"must not be writable by group or other users"})
		}
	}
	if a.CAFile != "" {
		if _, err := os.Stat(a.CAFile); err != nil {
			*problems = append(*problems, Problem{file, "ca_file",
				"cannot read the file: " + err.Error()})
		}
	}
}

// checkGatewayURL validates gateway_url: a parseable URL that uses https,
// unless its host is a loopback address (docs/configuration.md section
// 7.1, docs/agent-protocol.md section 1).
func checkGatewayURL(a *AgentConfig, file string, problems *Problems) {
	const field = "gateway_url"
	if a.GatewayURL == "" {
		*problems = append(*problems, Problem{file, field, "is required"})
		return
	}
	u, err := url.Parse(a.GatewayURL)
	if err != nil || u.Hostname() == "" {
		*problems = append(*problems, Problem{file, field, "is not a valid URL"})
		return
	}
	if u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		*problems = append(*problems, Problem{file, field,
			"must not carry a query, a fragment, or a user name"})
		return
	}
	if u.Scheme == "https" || u.Scheme == "http" && IsLoopbackHost(u.Hostname()) {
		return
	}
	*problems = append(*problems, Problem{file, field,
		"must use https, unless its host is a loopback address"})
}

// operationPattern matches a `<capability>.<operation>` name.
var operationPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]*\.[a-z][a-z0-9_-]*$`)
