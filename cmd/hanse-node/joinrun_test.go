package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/achmadss/hanse/internal/config"
)

// TestJoinThenRunFindsTheCredential is the end-to-end acceptance test: a
// join that names --state-dir records it in agent.yaml, and a run with that
// same file connects, without root (issue #71).
func TestJoinThenRunFindsTheCredential(t *testing.T) {
	gateway := newTestGateway(t)
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	configPath := filepath.Join(dir, "agent.yaml")
	// The file is what an operator starts with: a version and nothing
	// else. join fills in the Gateway URL and the state directory.
	if err := os.WriteFile(configPath, []byte("version: 1\n"), 0o600); err != nil {
		t.Fatalf("write agent.yaml: %v", err)
	}
	token, err := gateway.enrollment.IssueJoinToken("node-a")
	if err != nil {
		t.Fatalf("issue a join token: %v", err)
	}
	var joinOut, joinErr bytes.Buffer
	if code := run([]string{"join", gateway.server.URL, token.Plain,
		"--state-dir", stateDir, "--config", configPath}, &joinOut, &joinErr); code != exitOK {
		t.Fatalf("join: exit %d: %s", code, joinErr.String())
	}
	cfg, problems, err := config.LoadAgent(configPath)
	if err != nil {
		t.Fatalf("load agent.yaml: %v", err)
	}
	if len(problems) > 0 {
		t.Fatalf("problems: %v", problems)
	}
	if cfg.StateDir != stateDir {
		t.Fatalf("state_dir = %q, want %q", cfg.StateDir, stateDir)
	}

	// Now run the agent from the same configuration file. It must find
	// the credential that join stored, or it never connects.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logs := &agentLog{}
	done := make(chan int, 1)
	go func() { done <- agentRun(ctx, configPath, logs, false) }()
	waitForOnline(t, gateway.hub, "node-a", logs)
	cancel()
	select {
	case code := <-done:
		if code != exitOK {
			t.Fatalf("run: exit %d:\n%s", code, logs.String())
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("the agent did not stop:\n%s", logs.String())
	}
}
