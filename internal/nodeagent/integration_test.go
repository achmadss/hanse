package nodeagent_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/achmadss/hanse/internal/agentconn"
	"github.com/achmadss/hanse/internal/api"
	"github.com/achmadss/hanse/internal/enroll"
	"github.com/achmadss/hanse/internal/registry"
	"github.com/achmadss/hanse/internal/runtime"
)

// nodeBinary is the hanse-node executable the integration tests run as a
// separate process. TestMain builds it once.
var nodeBinary string

// TestMain builds the agent binary into a temporary directory.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "hanse-node-binary-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "create binary directory: %v\n", err)
		os.Exit(1)
	}
	name := "hanse-node"
	if goruntime.GOOS == "windows" {
		name += ".exe"
	}
	nodeBinary = filepath.Join(dir, name)
	build := exec.Command("go", "build", "-o", nodeBinary, "./cmd/hanse-node")
	build.Dir = filepath.Join("..", "..")
	if output, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build hanse-node: %v\n%s\n", err, output)
		_ = os.RemoveAll(dir)
		os.Exit(1)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// recordingSink stores the log lines the Gateway receives.
type recordingSink struct {
	mu    sync.Mutex
	lines []sinkLine
}

// sinkLine is one stored log line.
type sinkLine struct {
	runID  string
	stream string
	line   string
}

// Log implements agentconn.LogSink.
func (s *recordingSink) Log(runID string, _ time.Time, stream, line string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lines = append(s.lines, sinkLine{runID: runID, stream: stream, line: line})
}

// count returns how many stored lines equal line.
func (s *recordingSink) count(line string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	total := 0
	for _, stored := range s.lines {
		if stored.line == line {
			total++
		}
	}
	return total
}

// waitFor waits until one stored line equals line.
func (s *recordingSink) waitFor(t *testing.T, line string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		if s.count(line) > 0 {
			return
		}
		if time.Now().After(deadline) {
			s.mu.Lock()
			got := append([]sinkLine(nil), s.lines...)
			s.mu.Unlock()
			t.Fatalf("no log line %q; got %+v", line, got)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// waitForContains waits until one stored line contains text. The shell and
// Python echo handlers quote the message differently, so the tests match on
// the stable part of the line.
func (s *recordingSink) waitForContains(t *testing.T, text string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		s.mu.Lock()
		found := false
		for _, stored := range s.lines {
			if strings.Contains(stored.line, text) {
				found = true
				break
			}
		}
		s.mu.Unlock()
		if found {
			return
		}
		if time.Now().After(deadline) {
			s.mu.Lock()
			got := append([]sinkLine(nil), s.lines...)
			s.mu.Unlock()
			t.Fatalf("no log line containing %q; got %+v", text, got)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// lines returns a copy of the stored lines.
func (s *recordingSink) linesCopy() []sinkLine {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]sinkLine(nil), s.lines...)
}

// stack is one in-process Gateway with its agent process.
type stack struct {
	reg            *registry.Registry
	hub            *agentconn.Hub
	service        *enroll.Service
	server         *httptest.Server
	logs           *recordingSink
	bundleRequests *atomic.Int64
	capsDir        string
	stateDir       string
	configPath     string
	process        *exec.Cmd
	output         *bytes.Buffer
}

// newStack builds the Gateway side, enrolls node-a with the real join
// command, and starts the agent process.
func newStack(t *testing.T) *stack {
	t.Helper()
	requireInterpreter(t)
	root := filepath.Join("..", "..")
	capsDir := filepath.Join(t.TempDir(), "capabilities")
	copyTree(t, filepath.Join(root, "capabilities", "echo"), filepath.Join(capsDir, "echo"))

	reg, warnings, err := registry.Load(registry.Options{
		Dirs: []string{capsDir},
		Input: registry.Input{
			Nodes: []registry.Node{{
				ID: "node-a", OS: goruntime.GOOS, Arch: goruntime.GOARCH,
				Roles: []string{"server"}, Enabled: true,
			}},
			Routes: registry.Routes{Capabilities: map[string]registry.CapabilityRoute{
				"echo": {Executors: []string{"node-a"}, DefaultExecutor: "node-a"},
			}},
		},
	})
	if err != nil {
		t.Fatalf("load registry: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings: %+v", warnings)
	}
	sink := &recordingSink{}
	var service *enroll.Service
	hub, err := agentconn.New(agentconn.Config{
		Registry:       reg,
		GatewayVersion: "1.0.0",
		Logs:           sink,
		Verify: func(credential string) (string, error) {
			if service == nil {
				return "", fmt.Errorf("the enrollment service is not ready")
			}
			return service.Verify(credential)
		},
		HeartbeatInterval: 2 * time.Second,
		HeartbeatTimeout:  10 * time.Second,
		ReconnectWindow:   time.Minute,
	})
	if err != nil {
		t.Fatalf("build hub: %v", err)
	}
	service, err = enroll.Open(enroll.Options{StateDir: t.TempDir(), Disconnect: hub})
	if err != nil {
		t.Fatalf("open enrollment store: %v", err)
	}
	t.Cleanup(func() { _ = service.Close() })

	apiHandler := api.New(reg, nil, service, api.WithAgentHub(hub))
	bundleRequests := &atomic.Int64{}
	mux := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v1/agent/bundles/") {
			bundleRequests.Add(1)
		}
		apiHandler.ServeHTTP(w, r)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	st := &stack{
		reg:            reg,
		hub:            hub,
		service:        service,
		server:         server,
		logs:           sink,
		bundleRequests: bundleRequests,
		capsDir:        capsDir,
		stateDir:       filepath.Join(t.TempDir(), "state"),
		configPath:     filepath.Join(t.TempDir(), "agent.yaml"),
	}
	if err := os.WriteFile(st.configPath, []byte(fmt.Sprintf(
		"version: 1\nstate_dir: %s\nmax_concurrent_runs: 4\nlog:\n  level: info\n  format: text\n",
		st.stateDir)), 0o600); err != nil {
		t.Fatalf("write agent.yaml: %v", err)
	}
	token, err := service.IssueJoinToken("node-a")
	if err != nil {
		t.Fatalf("issue join token: %v", err)
	}
	st.runCommand(t, "join", server.URL, token.Plain, "--state-dir", st.stateDir, "--config", st.configPath)

	st.output = &bytes.Buffer{}
	st.process = exec.Command(nodeBinary, "run", "--config", st.configPath)
	st.process.Stdout = st.output
	st.process.Stderr = st.output
	if err := st.process.Start(); err != nil {
		t.Fatalf("start hanse-node: %v", err)
	}
	t.Cleanup(func() {
		stopProcess(st.process)
		if t.Failed() {
			t.Logf("hanse-node output:\n%s", st.output.String())
		}
	})
	deadline := time.Now().Add(30 * time.Second)
	for !hub.Online("node-a") {
		if time.Now().After(deadline) {
			t.Fatalf("the agent did not connect; output:\n%s", st.output.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	return st
}

// bundleCount returns the number of bundle downloads so far.
func (s *stack) bundleCount() int64 { return s.bundleRequests.Load() }

// runCommand runs the agent binary with arguments and fails on a non-zero
// exit.
func (s *stack) runCommand(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command(nodeBinary, args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("hanse-node %v: %v\n%s", args, err, out.String())
	}
	return out.String()
}

// call runs one operation through the Gateway's hub.
func (s *stack) call(ctx context.Context, t *testing.T, runID, operation string, arguments map[string]any) (runtime.Response, error) {
	t.Helper()
	capability, ok := s.reg.Capability("echo")
	if !ok {
		t.Fatal("the echo capability is not loaded")
	}
	op, ok := capability.Operation(operation)
	if !ok {
		t.Fatalf("echo has no operation %q", operation)
	}
	entry := op.Entry[goruntime.GOOS]
	if len(entry) == 0 {
		t.Skipf("the echo capability has no entry command for %s", goruntime.GOOS)
	}
	deadline := time.Now().Add(5 * time.Minute).UTC().Format("2006-01-02T15:04:05.000Z")
	argumentsJSON, err := json.Marshal(arguments)
	if err != nil {
		t.Fatalf("encode arguments: %v", err)
	}
	envelope, err := json.Marshal(map[string]any{
		"contract":   runtime.ContractV1,
		"kind":       runtime.KindOperation,
		"id":         runID,
		"capability": "echo",
		"operation":  operation,
		"arguments":  json.RawMessage(argumentsJSON),
		"nodes":      map[string]any{},
		"executor": map[string]any{
			"id": "node-a", "name": "Node A", "os": goruntime.GOOS, "arch": goruntime.GOARCH,
			"address": "192.0.2.11", "attributes": map[string]any{},
		},
		"secrets":  map[string]string{},
		"deadline": deadline,
	})
	if err != nil {
		t.Fatalf("encode request: %v", err)
	}
	return s.hub.Execute(ctx, &agentconn.Call{
		RunID:       runID,
		Node:        "node-a",
		Capability:  "echo",
		Operation:   operation,
		Bundle:      capability.BundleHash,
		Entry:       entry,
		Request:     envelope,
		GracePeriod: 10 * time.Second,
	})
}

// TestEchoSayRunsOnTheAgent is the end-to-end proof: a separate hanse-node
// process downloads the bundle and runs echo.say.
func TestEchoSayRunsOnTheAgent(t *testing.T) {
	st := newStack(t)
	response, err := st.call(context.Background(), t, "op_say", "say", map[string]any{"message": "hello"})
	if err != nil {
		t.Fatalf("execute echo.say: %v", err)
	}
	if response.Status != runtime.StatusOK {
		t.Fatalf("response status %q, want ok: %+v", response.Status, response)
	}
	if !strings.Contains(string(response.Result), `"said":"hello"`) {
		t.Errorf("result %s, want the echoed message", response.Result)
	}
	st.logs.waitForContains(t, "say:")
	if got := st.bundleCount(); got != 1 {
		t.Errorf("bundle downloads %d, want 1", got)
	}
}

// TestStderrArrivesWhileTheHandlerRuns proves a stderr line reaches the
// Gateway before the result.
func TestStderrArrivesWhileTheHandlerRuns(t *testing.T) {
	st := newStack(t)
	done := make(chan error, 1)
	go func() {
		response, err := st.call(context.Background(), t, "op_sleep", "sleep", map[string]any{"seconds": 3})
		if err == nil && response.Status != runtime.StatusOK {
			err = fmt.Errorf("status %q", response.Status)
		}
		done <- err
	}()
	st.logs.waitFor(t, "sleep 1 of 3")
	select {
	case err := <-done:
		t.Fatalf("the run ended before the first log line arrived: %v", err)
	default:
	}
	if err := <-done; err != nil {
		t.Fatalf("execute echo.sleep: %v", err)
	}
	st.logs.waitFor(t, "sleep 3 of 3")
}

// TestChangedHandlerIsDownloadedAgain proves the cache is content
// addressed: an unchanged bundle is not fetched again, a changed one is.
func TestChangedHandlerIsDownloadedAgain(t *testing.T) {
	st := newStack(t)
	if _, err := st.call(context.Background(), t, "op_unchanged_1", "say", map[string]any{"message": "one"}); err != nil {
		t.Fatalf("first run: %v", err)
	}
	afterFirst := st.bundleCount()
	if afterFirst != 1 {
		t.Fatalf("bundle downloads %d, want 1", afterFirst)
	}
	if _, err := st.call(context.Background(), t, "op_unchanged_2", "say", map[string]any{"message": "two"}); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if got := st.bundleCount(); got != afterFirst {
		t.Fatalf("an unchanged bundle was downloaded again: %d -> %d", afterFirst, got)
	}

	// Change the handler and reload the registry, which recomputes the
	// content hash.
	handler := filepath.Join(st.capsDir, "echo", "handler.sh")
	data, err := os.ReadFile(handler)
	if err != nil {
		t.Fatalf("read handler: %v", err)
	}
	if err := os.WriteFile(handler, append(data, []byte("\n# changed\n")...), 0o644); err != nil {
		t.Fatalf("change handler: %v", err)
	}
	if _, err := st.reg.Reload(); err != nil {
		t.Fatalf("reload registry: %v", err)
	}
	if _, err := st.call(context.Background(), t, "op_changed", "say", map[string]any{"message": "three"}); err != nil {
		t.Fatalf("run after change: %v", err)
	}
	if got := st.bundleCount(); got != afterFirst+1 {
		t.Fatalf("bundle downloads %d, want %d", got, afterFirst+1)
	}
}

// TestReconnectDeliversEveryLogAndTheResult drops the Gateway-side socket
// during a run and proves every log line and the result arrive after the
// reconnect, with no gaps or duplicates.
func TestReconnectDeliversEveryLogAndTheResult(t *testing.T) {
	st := newStack(t)
	done := make(chan struct {
		response runtime.Response
		err      error
	}, 1)
	go func() {
		response, err := st.call(context.Background(), t, "op_drop", "sleep", map[string]any{"seconds": 5})
		done <- struct {
			response runtime.Response
			err      error
		}{response: response, err: err}
	}()
	st.logs.waitFor(t, "sleep 1 of 5")
	st.server.CloseClientConnections()

	select {
	case outcome := <-done:
		if outcome.err != nil {
			t.Fatalf("execute echo.sleep: %v", outcome.err)
		}
		if outcome.response.Status != runtime.StatusOK {
			t.Fatalf("response status %q, want ok", outcome.response.Status)
		}
		if !strings.Contains(string(outcome.response.Result), `"slept":5`) {
			t.Errorf("result %s, want slept 5", outcome.response.Result)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("the run did not finish after the reconnect")
	}
	for i := 1; i <= 5; i++ {
		line := fmt.Sprintf("sleep %d of 5", i)
		if got := st.logs.count(line); got != 1 {
			t.Errorf("log line %q arrived %d times, want exactly 1; lines: %+v", line, got, st.logs.linesCopy())
		}
	}
}

// requireInterpreter skips a test when the echo capability's interpreter
// is missing. Windows needs Python 3; Linux and macOS need sh.
func requireInterpreter(t *testing.T) {
	t.Helper()
	name := "sh"
	if goruntime.GOOS == "windows" {
		name = "python"
	}
	if _, err := exec.LookPath(name); err != nil {
		t.Skipf("%s is required to run the echo capability on %s", name, goruntime.GOOS)
	}
}

// copyTree copies every regular file under src into dst.
func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		mode := os.FileMode(0o644)
		if info.Mode().Perm()&0o111 != 0 {
			mode = 0o755
		}
		return os.WriteFile(target, data, mode)
	})
	if err != nil {
		t.Fatalf("copy capability: %v", err)
	}
}

// stopProcess stops an agent process, first with the termination signal so
// the agent can stop its handlers, then with a kill.
func stopProcess(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	if goruntime.GOOS == "windows" {
		// Windows has no signal that stops a console process cleanly, and
		// the agent's job objects kill its handlers when it ends.
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return
	}
	_ = cmd.Process.Signal(os.Interrupt)
	waited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(waited)
	}()
	select {
	case <-waited:
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		<-waited
	}
}
