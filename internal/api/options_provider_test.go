package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	goruntime "runtime"
	"testing"
	"time"

	"github.com/achmadss/hanse/internal/agent"
	"github.com/achmadss/hanse/internal/operations"
	"github.com/achmadss/hanse/internal/registry"
	"github.com/achmadss/hanse/internal/runtime"
)

// apiEchoInterpreterAvailable reports whether the current OS has the
// interpreter that capabilities/echo uses.
func apiEchoInterpreterAvailable() bool {
	if goruntime.GOOS == "windows" {
		if _, err := exec.LookPath("python"); err == nil {
			return true
		}
		_, err := exec.LookPath("python3")
		return err == nil
	}
	_, err := exec.LookPath("sh")
	return err == nil
}

// apiLoadEcho loads capabilities/echo with a Gateway host node.
func apiLoadEcho(t *testing.T) *registry.Registry {
	t.Helper()
	reg, _, err := registry.Load(registry.Options{
		Dirs: []string{filepath.Join("..", "..", "capabilities")},
		Input: registry.Input{
			Nodes: []registry.Node{{
				ID:      "gateway",
				Name:    "Gateway host",
				OS:      goruntime.GOOS,
				Arch:    goruntime.GOARCH,
				Enabled: true,
			}},
			GatewayHost: "gateway",
			Routes: registry.Routes{Capabilities: map[string]registry.CapabilityRoute{
				"echo": {Wildcard: true, DefaultExecutor: "gateway"},
			}},
		},
	})
	if err != nil {
		t.Fatalf("load capabilities/echo: %v", err)
	}
	return reg
}

// TestExecOptionProvider covers an exec option provider through the
// embedded agent and the options endpoint.
func TestExecOptionProvider(t *testing.T) {
	if !apiEchoInterpreterAvailable() {
		t.Skip("the interpreter of capabilities/echo is not installed")
	}
	reg := apiLoadEcho(t)
	host, err := agent.New(agent.Config{
		Node: runtime.Node{
			ID:   "gateway",
			Name: "Gateway host",
			OS:   goruntime.GOOS,
			Arch: goruntime.GOARCH,
		},
		Registry:      reg,
		ResponseLimit: 1 << 20,
		GracePeriod:   time.Second,
		// A Windows runner can take several seconds to start `sh` for the
		// echo handler, so the option provider deadline is generous: the
		// test proves the exec provider runs, not how fast a process
		// starts.
		OptionsTimeout: 30 * time.Second,
		WorkRoot:       t.TempDir(),
	})
	if err != nil {
		t.Fatalf("build the embedded agent: %v", err)
	}
	dispatcher, err := operations.NewExecDispatcher(operations.ExecConfig{Registry: reg, Agent: host})
	if err != nil {
		t.Fatalf("build the exec dispatcher: %v", err)
	}
	handler := NewWithOptionProviders(reg, nil, nil, OptionProviders{Exec: dispatcher})
	resp, body := get(t, handler, "/v1/operations/echo/sleep/options/seconds")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, body %s", resp.StatusCode, body)
	}
	values := decodeOptionValues(t, body)
	if len(values) != 2 || values[0] != "5" || values[1] != "60" {
		t.Errorf("options = %v, want [5 60]", values)
	}
}

// TestHTTPOptionProvider covers an http option provider through the HTTP
// dispatcher and the options endpoint. The partial arguments name a node,
// so the envelope's `nodes` object carries it (docs/handler-contract.md
// sections 2 and 8).
func TestHTTPOptionProvider(t *testing.T) {
	nodesCh := make(chan map[string]runtime.Node, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		var envelope struct {
			Kind       string                  `json:"kind"`
			ID         string                  `json:"id"`
			Capability string                  `json:"capability"`
			Operation  string                  `json:"operation"`
			Field      string                  `json:"field"`
			Nodes      map[string]runtime.Node `json:"nodes"`
		}
		if err := json.NewDecoder(r.Body).Decode(&envelope); err != nil {
			t.Errorf("decode request envelope: %v", err)
		}
		if envelope.Kind != runtime.KindOptions || envelope.Field != "mode" {
			t.Errorf("envelope = %+v, want an options call for mode", envelope)
		}
		nodesCh <- envelope.Nodes
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"contract":"hanse.handler/v1","status":"ok","result":{"options":[` +
			`{"value":"fast","label":"Fast profile"},{"value":"safe","description":"Lower risk"}]}}`))
	}))
	defer srv.Close()

	root := t.TempDir()
	capDir := filepath.Join(root, "remote")
	if err := os.MkdirAll(capDir, 0o755); err != nil {
		t.Fatalf("create capability directory: %v", err)
	}
	manifest := fmt.Sprintf(`version: 1
id: remote
name: Remote
description: Serve options from an HTTP service
contract: hanse.handler/v1

handler:
  kind: http
  url: %s/options

operations:
  probe:
    description: Probe a remote machine
    arguments:
      type: object
      properties:
        mode:
          type: string
        node:
          type: string
          format: hanse:node
    options:
      mode: provider
`, srv.URL)
	if err := os.WriteFile(filepath.Join(capDir, "manifest.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	reg, _, err := registry.Load(registry.Options{
		Dirs: []string{root},
		Input: registry.Input{
			Nodes: []registry.Node{
				{
					ID:      "gateway",
					Name:    "Gateway host",
					OS:      goruntime.GOOS,
					Arch:    goruntime.GOARCH,
					Enabled: true,
				},
				{
					ID:         "node-b",
					Name:       "Target machine",
					OS:         "windows",
					Arch:       "amd64",
					Address:    "192.0.2.20",
					Enabled:    true,
					Attributes: map[string]string{"mac": "00:00:5e:00:53:01"},
				},
			},
			GatewayHost: "gateway",
			Routes: registry.Routes{Capabilities: map[string]registry.CapabilityRoute{
				"remote": {Wildcard: true, DefaultExecutor: "gateway"},
			}},
		},
	})
	if err != nil {
		t.Fatalf("load the http capability: %v", err)
	}
	dispatcher, err := operations.NewHTTPDispatcher(operations.HTTPConfig{
		Registry:       reg,
		ResponseSize:   1 << 20,
		OptionsTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("build the HTTP dispatcher: %v", err)
	}
	handler := NewWithOptionProviders(reg, nil, nil, OptionProviders{HTTP: dispatcher})
	resp, body := get(t, handler, "/v1/operations/remote/probe/options/mode?node=node-b")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, body %s", resp.StatusCode, body)
	}
	values := decodeOptionValues(t, body)
	if len(values) != 2 || values[0] != "fast" || values[1] != "safe" {
		t.Errorf("options = %v, want [fast safe]", values)
	}
	nodes := <-nodesCh
	want := runtime.Node{
		ID:         "node-b",
		Name:       "Target machine",
		OS:         "windows",
		Arch:       "amd64",
		Address:    "192.0.2.20",
		Attributes: map[string]any{"mac": "00:00:5e:00:53:01"},
	}
	if got, ok := nodes["node-b"]; !ok {
		t.Fatalf("envelope nodes %v have no node-b", nodes)
	} else if !reflect.DeepEqual(got, want) {
		t.Errorf("envelope node %+v, want %+v", got, want)
	}
}

// decodeOptionValues reads the option values as strings, which both
// provider tests use, and preserves number formatting.
func decodeOptionValues(t *testing.T, body []byte) []string {
	t.Helper()
	var shape struct {
		Options []struct {
			Value json.RawMessage `json:"value"`
		} `json:"options"`
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&shape); err != nil {
		t.Fatalf("decode options: %v (%s)", err, body)
	}
	values := make([]string, 0, len(shape.Options))
	for _, option := range shape.Options {
		var value any
		valueDec := json.NewDecoder(bytes.NewReader(option.Value))
		valueDec.UseNumber()
		if err := valueDec.Decode(&value); err != nil {
			t.Fatalf("decode option value: %v", err)
		}
		values = append(values, fmt.Sprint(value))
	}
	return values
}
