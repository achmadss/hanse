package config

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config is the whole parsed Gateway configuration: gateway.yaml,
// nodes.yaml, and routes.yaml.
type Config struct {
	// Gateway is the parsed gateway.yaml, with defaults filled and every
	// relative path resolved.
	Gateway GatewayConfig
	// Nodes is the parsed nodes.yaml, with defaults filled.
	Nodes NodesConfig
	// Routes is the parsed routes.yaml.
	Routes RoutesConfig
}

// Load reads gateway.yaml, the nodes file and routes file it names, and
// validates all of them. It returns the configuration and every problem
// found; problems is empty only when the configuration is valid.
//
// path is the gateway.yaml path. Relative paths inside it resolve from
// its directory (docs/configuration.md §2).
func Load(path string) (*Config, Problems, error) {
	problems := Problems{}
	g, ok := loadGateway(path, &problems)
	if !ok {
		// gateway.yaml must load at all for anything else to be checkable.
		return nil, problems, nil
	}

	nodes := &NodesConfig{}
	nodesLoaded := false
	if g.Paths.Nodes != "" {
		if loaded, ok := loadNodes(g.Paths.Nodes, &problems); ok {
			nodes = loaded
			finalizeNodes(nodes)
			checkNodes(nodes, g.Paths.Nodes, &problems)
			nodesLoaded = true
		}
	}
	routes := &RoutesConfig{}
	routesLoaded := false
	if g.Paths.Routes != "" {
		if loaded, ok := loadRoutes(g.Paths.Routes, &problems); ok {
			finalizeRoutes(loaded)
			routes = loaded
			checkRoutes(routes, g.Paths.Routes, &problems)
			routesLoaded = true
		}
	}

	// A file that failed to load leaves empty data behind; the cross-file
	// checks would then only report noise, so they run against files that
	// did load.
	if nodesLoaded {
		checkGatewayNodes(g, nodes, path, &problems)
	}
	if routesLoaded && nodesLoaded {
		checkRouting(routes, nodes, g.Paths.Routes, &problems)
	}
	checkPermissions(g, path, &problems)

	return &Config{Gateway: *g, Nodes: *nodes, Routes: *routes}, problems, nil
}

// loadGateway reads and decodes gateway.yaml, fills its defaults and
// resolves its paths, and runs the per-file checks. It returns nil when
// the file cannot be used at all.
func loadGateway(path string, problems *Problems) (*GatewayConfig, bool) {
	g := &GatewayConfig{}
	if !decodeFile(path, g, problems) {
		return nil, false
	}
	finalizeGateway(g, filepath.Dir(path))
	checkGateway(g, path, problems)
	return g, true
}

// loadNodes reads and decodes nodes.yaml.
func loadNodes(path string, problems *Problems) (*NodesConfig, bool) {
	n := &NodesConfig{}
	if decodeFile(path, n, problems) {
		return n, true
	}
	return nil, false
}

// loadRoutes reads and decodes routes.yaml.
func loadRoutes(path string, problems *Problems) (*RoutesConfig, bool) {
	r := &RoutesConfig{}
	if decodeFile(path, r, problems) {
		return r, true
	}
	return nil, false
}

// decodeFile reads path and decodes it strictly into out. Every decode
// error, including unknown keys, becomes one problem named by the file
// and the decoder's line; the return is false when the file was not
// usable after all its errors.
func decodeFile(path string, out any, problems *Problems) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		*problems = append(*problems, Problem{path, "file", "cannot read the file: " + err.Error()})
		return false
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(out); err != nil && err != io.EOF {
		// yaml.TypeError reports each mistyped or unknown field as one
		// entry in Errors; anything else is a single syntax problem.
		if typeErr, ok := err.(*yaml.TypeError); ok {
			for _, e := range typeErr.Errors {
				*problems = append(*problems, Problem{path, "file", e})
			}
			return onlyUnknownFields(typeErr)
		}
		*problems = append(*problems, Problem{path, "file", err.Error()})
		return false
	}
	return true
}

// onlyUnknownFields reports whether a struct is usable after its type
// errors: every error line must be only an unknown field. A mistyped
// value or a null makes the struct unusable, because a required field
// could be silently missing or wrong.
func onlyUnknownFields(typeErr *yaml.TypeError) bool {
	for _, e := range typeErr.Errors {
		if !unknownFieldLine(e) {
			return false
		}
	}
	return true
}

// unknownFieldLine matches the yaml.v3 message for an unknown key:
// `line 3: field timeuouts not found in type config.GatewayTimeouts`.
func unknownFieldLine(e string) bool {
	return strings.Contains(e, "field ") && strings.Contains(e, " not found in type ")
}
