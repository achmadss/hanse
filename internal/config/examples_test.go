package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// decodeStrict mirrors the decoding in decodeFile, without file IO
// semantics, for fixtures that live outside the package.
func decodeStrict(t *testing.T, path string, out any) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		t.Skipf("example file %s is not present", path)
	}
	if err != nil {
		t.Fatal(err)
	}
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(out); err != nil {
		return []string{err.Error()}
	}
	return nil
}

// TestDecodeRepoExamples keeps the example configuration files in the
// repository aligned with the structs: every key in the examples must be
// known, and the nodes and routes examples must validate cleanly.
func TestDecodeRepoExamples(t *testing.T) {
	dir := filepath.Join("..", "..", "config")

	var gw GatewayConfig
	if errs := decodeStrict(t, filepath.Join(dir, "gateway.example.yaml"), &gw); errs != nil {
		t.Errorf("gateway.example.yaml does not decode strictly: %v", errs)
	}
	if got := gw.Node; got != "gateway" {
		t.Errorf("gateway.example.yaml node = %q, want gateway", got)
	}

	var nodes NodesConfig
	if errs := decodeStrict(t, filepath.Join(dir, "nodes.example.yaml"), &nodes); errs != nil {
		t.Errorf("nodes.example.yaml does not decode strictly: %v", errs)
	}
	finalizeNodes(&nodes)
	problems := Problems{}
	checkNodes(&nodes, "nodes.example.yaml", &problems)
	assertProblems(t, problems, nil)

	var routes RoutesConfig
	if errs := decodeStrict(t, filepath.Join(dir, "routes.example.yaml"), &routes); errs != nil {
		t.Errorf("routes.example.yaml does not decode strictly: %v", errs)
	}
	finalizeRoutes(&routes)
	checkRoutes(&routes, "routes.example.yaml", &problems)
	checkRouting(&routes, &nodes, "routes.example.yaml", &problems)
	assertProblems(t, problems, nil)
}
