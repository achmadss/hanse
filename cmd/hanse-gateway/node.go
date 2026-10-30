package main

import (
	"io"
	"time"

	"github.com/achmadss/hanse/internal/config"
	"github.com/achmadss/hanse/internal/enroll"
)

// runNode runs the `node` subcommands: enroll and revoke. They manage the
// enrollment store in the Gateway state directory, which keeps only the
// hashes of join tokens and node credentials (docs/agent-protocol.md §2).
func runNode(args []string, stdout, stderr io.Writer, outerConfig string) int {
	if len(args) == 0 {
		nodeUsage(stderr)
		return exitUse
	}
	switch args[0] {
	case "enroll":
		return runNodeEnroll(args[1:], stdout, stderr, outerConfig)
	case "revoke":
		return runNodeRevoke(args[1:], stdout, stderr, outerConfig)
	}
	nodeUsage(stderr)
	return exitUse
}

func nodeUsage(w io.Writer) {
	printf(w, "usage: hanse-gateway node enroll <id> [--config <path>]\n")
	printf(w, "       hanse-gateway node revoke <id> [--config <path>]\n")
}

// loadNodeConfig loads gateway.yaml and the nodes and routes files it
// names. Both subcommands need the node list: a node that nodes.yaml does
// not hold cannot enroll, and a mistyped ID must fail instead of silently
// revoking nothing.
func loadNodeConfig(stderr io.Writer, cfgPath string) (*config.Config, bool) {
	cfg, problems, err := config.Load(cfgPath)
	if err != nil {
		printf(stderr, "hanse-gateway: %v\n", err)
		return nil, false
	}
	if len(problems) > 0 {
		for _, p := range problems {
			printf(stderr, "%s\n", p.Error())
		}
		printf(stderr, "hanse-gateway: %s: %d configuration problem(s)\n", cfgPath, len(problems))
		return nil, false
	}
	return cfg, true
}

// openEnroll opens the enrollment store in the Gateway state directory.
func openEnroll(cfg *config.Config, stderr io.Writer) (*enroll.Service, bool) {
	var ttl time.Duration
	if cfg.Gateway.Agents.JoinTokenTTL != nil {
		ttl = cfg.Gateway.Agents.JoinTokenTTL.Value
	}
	service, err := enroll.Open(enroll.Options{
		StateDir:     cfg.Gateway.Paths.StateDir,
		JoinTokenTTL: ttl,
	})
	if err != nil {
		printf(stderr, "hanse-gateway: %v\n", err)
		return nil, false
	}
	return service, true
}

// nodeID requires exactly one positional argument: the node ID, which must
// exist in nodes.yaml.
func nodeID(f parsedFlags, cfg *config.Config, stderr io.Writer) (string, bool) {
	if len(f.positionals) != 1 {
		nodeUsage(stderr)
		return "", false
	}
	id := f.positionals[0]
	if !cfg.Nodes.Has(id) {
		printf(stderr, "hanse-gateway: node %q is not in %s\n", id, cfg.Gateway.Paths.Nodes)
		return "", false
	}
	return id, true
}

// runNodeEnroll issues a one-time join token for a node and prints it,
// with the join command, once. The token is never printed again, and the
// store keeps only its hash.
func runNodeEnroll(args []string, stdout, stderr io.Writer, outerConfig string) int {
	f, ok := parseTokenArgs(args, false, stderr)
	if !ok {
		return exitUse
	}
	configPath := f.config
	if configPath == "" {
		configPath = outerConfig
	}
	cfg, ok := loadNodeConfig(stderr, configPath)
	if !ok {
		return exitFail
	}
	id, ok := nodeID(f, cfg, stderr)
	if !ok {
		return exitFail
	}
	service, ok := openEnroll(cfg, stderr)
	if !ok {
		return exitFail
	}
	defer func() { _ = service.Close() }()
	token, err := service.IssueJoinToken(id)
	if err != nil {
		printf(stderr, "hanse-gateway: %v\n", err)
		return exitFail
	}
	// The token appears exactly once, here.
	printf(stdout, "join token for %s (one use, expires %s):\n\n", token.Node, token.Expires.UTC().Format(time.RFC3339))
	printf(stdout, "  hanse-node join %s %s\n\n", cfg.Gateway.PublicURL, token.Plain)
	printf(stdout, "Run that command on the node. This token is shown only once.\n")
	return exitOK
}

// runNodeRevoke revokes a node's credential and closes its live
// connection. The node must enroll again before it can connect.
func runNodeRevoke(args []string, stdout, stderr io.Writer, outerConfig string) int {
	f, ok := parseTokenArgs(args, false, stderr)
	if !ok {
		return exitUse
	}
	configPath := f.config
	if configPath == "" {
		configPath = outerConfig
	}
	cfg, ok := loadNodeConfig(stderr, configPath)
	if !ok {
		return exitFail
	}
	id, ok := nodeID(f, cfg, stderr)
	if !ok {
		return exitFail
	}
	service, ok := openEnroll(cfg, stderr)
	if !ok {
		return exitFail
	}
	defer func() { _ = service.Close() }()
	if err := service.Revoke(id); err != nil {
		printf(stderr, "hanse-gateway: %v\n", err)
		return exitFail
	}
	printf(stdout, "revoked the node credential of %s\n", id)
	return exitOK
}
