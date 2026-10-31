package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/achmadss/hanse/internal/config"
	"github.com/achmadss/hanse/internal/nodeagent"
)

// defaultConfigPath is the documented agent.yaml location per operating
// system (docs/configuration.md section 1.1).
func defaultConfigPath() string {
	switch runtime.GOOS {
	case "darwin":
		return "/usr/local/etc/hanse-node/agent.yaml"
	case "windows":
		return `C:\ProgramData\Hanse\node\agent.yaml`
	default:
		return "/etc/hanse-node/agent.yaml"
	}
}

// runAgent loads agent.yaml, reads the node credential that `hanse-node
// join` wrote, and runs the agent until SIGINT or SIGTERM.
func runAgent(args []string, stderr io.Writer, configPath string) int {
	path, ok := parseRunArgs(args, configPath, stderr)
	if !ok {
		return exitUse
	}
	cfg, problems, err := config.LoadAgent(path)
	if err != nil {
		printf(stderr, "hanse-node: %v\n", err)
		return exitFail
	}
	if len(problems) > 0 {
		for _, problem := range problems {
			printf(stderr, "%s\n", problem.Error())
		}
		printf(stderr, "hanse-node: %s: %d configuration problem(s)\n", path, len(problems))
		return exitFail
	}
	credential, err := readCredential(cfg.StateDir)
	if err != nil {
		printf(stderr, "hanse-node: %v\n", err)
		return exitFail
	}
	logf := newAgentLogger(stderr, cfg.Log.Level, cfg.Log.Format)
	agent, err := nodeagent.New(nodeagent.Config{
		GatewayURL:        cfg.GatewayURL,
		Credential:        credential,
		CAFile:            cfg.CAFile,
		StateDir:          cfg.StateDir,
		MaxConcurrentRuns: *cfg.MaxConcurrentRuns,
		BufferLimit:       cfg.BufferLimit.Value,
		EnvAllow:          cfg.Handlers.EnvAllow,
		Elevation: nodeagent.Elevation{
			Enabled:         cfg.Elevation.Enabled,
			Operations:      cfg.Elevation.Operations,
			Account:         cfg.Elevation.Account,
			LockoutAttempts: *cfg.Elevation.Lockout.Attempts,
			LockoutWindow:   cfg.Elevation.Lockout.Window.Value,
			LockoutDuration: cfg.Elevation.Lockout.Duration.Value,
		},
		Logf: logf,
	})
	if err != nil {
		printf(stderr, "hanse-node: %v\n", err)
		return exitFail
	}
	ctx, stop := signal.NotifyContext(context.Background(), terminationSignals()...)
	defer stop()
	logf("connecting to %s", cfg.GatewayURL)
	if err := agent.Run(ctx); err != nil {
		printf(stderr, "hanse-node: %v\n", err)
		return exitFail
	}
	return exitOK
}

// parseRunArgs reads the run command's arguments. The configuration path
// may appear as --config before or after the run subcommand.
func parseRunArgs(args []string, configPath string, stderr io.Writer) (string, bool) {
	for i := 0; i < len(args); i++ {
		name := strings.TrimLeft(args[i], "-")
		value, hasEq := "", false
		if eq := strings.IndexByte(name, '='); eq >= 0 {
			name, value, hasEq = name[:eq], name[eq+1:], true
		}
		switch name {
		case "config":
			if !hasEq {
				if i+1 >= len(args) {
					printf(stderr, "hanse-node: --config needs a value\n")
					return "", false
				}
				i++
				value = args[i]
			}
			if value == "" {
				printf(stderr, "hanse-node: --config needs a value\n")
				return "", false
			}
			configPath = value
		default:
			printf(stderr, "hanse-node: unknown flag: %s\n", args[i])
			return "", false
		}
	}
	return configPath, true
}

// readCredential reads <state_dir>/credential, written by `hanse-node
// join` (docs/configuration.md section 8.2). The value never reaches a
// log line.
func readCredential(stateDir string) (string, error) {
	path := filepath.Join(stateDir, credentialFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("cannot read the node credential %s; run `hanse-node join` first", path)
	}
	var stored credentialFile
	if err := json.Unmarshal(data, &stored); err != nil {
		return "", fmt.Errorf("the node credential file %s is not valid", path)
	}
	if stored.Credential == "" {
		return "", fmt.Errorf("the node credential file %s holds no credential", path)
	}
	return stored.Credential, nil
}

// newAgentLogger writes the agent's own lines in the configured format.
// The agent logs connection and handler events at the info level, so the
// level setting only documents the intended verbosity until the agent has
// debug lines to add. It never prints a credential or a secret.
func newAgentLogger(w io.Writer, level, format string) func(format string, args ...any) {
	_ = level
	return func(message string, args ...any) {
		line := fmt.Sprintf(message, args...)
		if format == "json" {
			payload, err := json.Marshal(struct {
				Level   string `json:"level"`
				Message string `json:"message"`
			}{Level: "info", Message: line})
			if err == nil {
				printf(w, "%s\n", payload)
				return
			}
		}
		printf(w, "hanse-node: %s\n", line)
	}
}
