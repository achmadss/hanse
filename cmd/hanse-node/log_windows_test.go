//go:build windows

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNodeLogWritesIntoTheStateDirectory covers the service log: the lines
// land in <state dir>\hanse-node.log, which is what an operator opens.
func TestNodeLogWritesIntoTheStateDirectory(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	log := &nodeLog{}
	log.SetStateDir(stateDir)
	if _, err := log.Write([]byte("hanse-node: connecting to https://gateway.example.com\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := log.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	path := filepath.Join(stateDir, logFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the log: %v", err)
	}
	if !strings.Contains(string(data), "connecting to https://gateway.example.com") {
		t.Errorf("log = %q, want the agent line", data)
	}
}

// TestNodeLogKeepsLinesBeforeTheStateDirectoryIsKnown covers a
// configuration problem: the agent logs before it has read state_dir, and
// those lines must still reach the file, because they are the reason the
// service did not start.
func TestNodeLogKeepsLinesBeforeTheStateDirectoryIsKnown(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	log := &nodeLog{}
	if _, err := log.Write([]byte("hanse-node: gateway_url is required\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	log.SetStateDir(stateDir)
	if err := log.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(stateDir, logFileName))
	if err != nil {
		t.Fatalf("read the log: %v", err)
	}
	if !strings.Contains(string(data), "gateway_url is required") {
		t.Errorf("log = %q, want the early line", data)
	}
}

// TestNodeLogRotatesAtTenMiB covers the size cap: the file moves to .1 and
// a fresh file starts, and only one old file is kept.
func TestNodeLogRotatesAtTenMiB(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	log := &nodeLog{}
	log.SetStateDir(stateDir)
	chunk := bytes.Repeat([]byte("x"), 1<<20)
	for i := 0; i < 10; i++ {
		if _, err := log.Write(chunk); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	path := filepath.Join(stateDir, logFileName)
	if info, err := os.Stat(path); err != nil || info.Size() != logLimit {
		t.Fatalf("log size = %v, want the full %d bytes", info, logLimit)
	}
	// One more MiB rotates the full file to .1 and starts a new one.
	if _, err := log.Write(chunk); err != nil {
		t.Fatalf("write after the limit: %v", err)
	}
	current, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat the log: %v", err)
	}
	if current.Size() != 1<<20 {
		t.Errorf("current log size = %d, want 1 MiB", current.Size())
	}
	old, err := os.Stat(path + logRotationSuffix)
	if err != nil {
		t.Fatalf("stat the old log: %v", err)
	}
	if old.Size() != logLimit {
		t.Errorf("old log size = %d, want %d", old.Size(), logLimit)
	}
	// A second rotation replaces the old file instead of adding another.
	for i := 0; i < 10; i++ {
		if _, err := log.Write(chunk); err != nil {
			t.Fatalf("second rotation write %d: %v", i, err)
		}
	}
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		t.Fatalf("read the state directory: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("state directory holds %d files, want the log and its one old file", len(entries))
	}
	if err := log.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// TestNodeLogMovesToTheConfiguredStateDirectory covers a state_dir that
// differs from the documented default: the early lines stay in the default
// file and the later lines go to the configured directory.
func TestNodeLogMovesToTheConfiguredStateDirectory(t *testing.T) {
	defaultDir := filepath.Join(t.TempDir(), "default")
	configuredDir := filepath.Join(t.TempDir(), "configured")
	log := &nodeLog{}
	log.SetStateDir(defaultDir)
	if _, err := log.Write([]byte("first\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	log.SetStateDir(configuredDir)
	if _, err := log.Write([]byte("second\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := log.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	first, err := os.ReadFile(filepath.Join(defaultDir, logFileName))
	if err != nil {
		t.Fatalf("read the default log: %v", err)
	}
	second, err := os.ReadFile(filepath.Join(configuredDir, logFileName))
	if err != nil {
		t.Fatalf("read the configured log: %v", err)
	}
	if string(first) != "first\n" {
		t.Errorf("default log = %q, want the first line", first)
	}
	if string(second) != "second\n" {
		t.Errorf("configured log = %q, want the second line", second)
	}
}

// TestOpenAgentLogUsesStderrInteractively covers a service that an operator
// started by hand: standard error is still a terminal, so it is kept.
func TestOpenAgentLogUsesStderrInteractively(t *testing.T) {
	var stderr bytes.Buffer
	logs, closeLogs := openAgentLog(false, &stderr)
	defer closeLogs()
	if _, err := logs.Write([]byte("hanse-node: hello\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if stderr.String() != "hanse-node: hello\n" {
		t.Errorf("stderr = %q, want the line", stderr.String())
	}
}
