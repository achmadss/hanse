package main

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/achmadss/hanse/internal/agentconn"
	"github.com/achmadss/hanse/internal/config"
	herrors "github.com/achmadss/hanse/internal/errors"
	"github.com/achmadss/hanse/internal/operations"
	"github.com/achmadss/hanse/internal/registry"
	"github.com/achmadss/hanse/internal/runtime"
)

// registryInput converts the parsed configuration files into the registry's
// own input shape: the nodes, the Gateway host node, and the routing.
// Nothing secret is copied: nodes.yaml holds only non-secret details
// (docs/configuration.md section 4.1).
func registryInput(cfg *config.Config) registry.Input {
	in := registry.Input{
		GatewayHost: cfg.Gateway.Node,
		Routes:      registry.Routes{Capabilities: map[string]registry.CapabilityRoute{}},
	}
	ids := make([]string, 0, len(cfg.Nodes.Nodes))
	for id := range cfg.Nodes.Nodes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		node := cfg.Nodes.Nodes[id]
		in.Nodes = append(in.Nodes, registry.Node{
			ID:         id,
			Name:       node.Name,
			OS:         node.OS,
			Arch:       node.Arch,
			Address:    node.Address,
			Roles:      append([]string(nil), node.Roles...),
			Attributes: nodeAttributes(node),
			Enabled:    node.IsEnabled(),
		})
	}
	for capID, route := range cfg.Routes.Capabilities {
		converted := registry.CapabilityRoute{
			Wildcard:        route.Executors.Star,
			Executors:       append([]string(nil), route.Executors.Nodes...),
			DefaultExecutor: route.DefaultExecutor,
		}
		if len(route.Operations) > 0 {
			converted.Operations = make(map[string]registry.OperationRoute, len(route.Operations))
		}
		for opID, op := range route.Operations {
			convertedOp := registry.OperationRoute{
				DefaultExecutor: op.DefaultExecutor,
				DefaultSet:      op.DefaultExecutor != "",
			}
			if op.Executors != nil {
				convertedOp.ExecutorsSet = true
				convertedOp.Wildcard = op.Executors.Star
				convertedOp.Executors = append([]string(nil), op.Executors.Nodes...)
			}
			converted.Operations[opID] = convertedOp
		}
		in.Routes.Capabilities[capID] = converted
	}
	return in
}

// nodeAttributes converts nodes.yaml attributes to the registry's string
// values. A YAML scalar that decodes as a number or a boolean keeps its
// text form, because node filters compare attribute names, not types.
func nodeAttributes(node config.NodeConfig) map[string]string {
	if len(node.Attributes) == 0 {
		return nil
	}
	out := make(map[string]string, len(node.Attributes))
	for name, value := range node.Attributes {
		if text, ok := value.(string); ok {
			out[name] = text
			continue
		}
		out[name] = fmt.Sprint(value)
	}
	return out
}

// embeddedNode is the Gateway host node as the embedded agent sees it: the
// node in nodes.yaml that gateway.yaml's `node` names.
func embeddedNode(cfg *config.Config) runtime.Node {
	node := cfg.Nodes.Nodes[cfg.Gateway.Node]
	attributes := make(map[string]any, len(node.Attributes))
	for name, value := range node.Attributes {
		attributes[name] = value
	}
	return runtime.Node{
		ID:         cfg.Gateway.Node,
		Name:       node.Name,
		OS:         node.OS,
		Arch:       node.Arch,
		Address:    node.Address,
		Attributes: attributes,
	}
}

// dispatcher routes one validated call to the runtime its capability
// declares. An exec capability runs on the embedded agent when the resolved
// executor is the Gateway host node and on the node agent otherwise
// (docs/architecture.md sections 7.2 and 7.3).
type dispatcher struct {
	registry *registry.Registry
	host     string
	exec     *operations.ExecDispatcher
	http     *operations.HTTPDispatcher
	agent    *operations.AgentDispatcher
}

// Execute runs one call on its runtime.
func (d *dispatcher) Execute(ctx context.Context, call *operations.Call) (runtime.Response, error) {
	if call == nil || call.Request == nil {
		return runtime.Response{}, herrors.New(herrors.Internal,
			"the dispatcher received no request envelope", nil)
	}
	capability, ok := d.registry.Capability(call.Request.Capability)
	if !ok {
		return runtime.Response{}, herrors.New(herrors.UnknownCapability,
			fmt.Sprintf("unknown capability %q", call.Request.Capability),
			map[string]any{"capability": call.Request.Capability})
	}
	if capability.Manifest == nil {
		return runtime.Response{}, herrors.New(herrors.Internal,
			fmt.Sprintf("capability %q has no manifest", capability.ID), nil)
	}
	switch capability.Manifest.Handler.Kind {
	case registry.KindHTTP:
		return d.http.Execute(ctx, call)
	case registry.KindExec:
		if call.Request.Executor.ID == d.host {
			return d.exec.Execute(ctx, call)
		}
		return d.agent.Execute(ctx, call)
	default:
		return runtime.Response{}, herrors.New(herrors.Internal,
			fmt.Sprintf("capability %q has an unsupported handler kind %q",
				capability.ID, capability.Manifest.Handler.Kind), nil)
	}
}

// lateLogSink is the hub's log sink before the operations manager exists.
// The hub and the manager depend on each other through the dispatcher, so
// the hub is built with this forwarding sink and the manager is filled in
// as soon as it is built. A line that arrives before that is dropped, which
// can only happen before the Gateway serves a request.
type lateLogSink struct {
	mu   sync.RWMutex
	sink agentconn.LogSink
}

// Log forwards one line to the manager once it is set.
func (l *lateLogSink) Log(runID string, at time.Time, stream, line string) {
	l.mu.RLock()
	sink := l.sink
	l.mu.RUnlock()
	if sink != nil {
		sink.Log(runID, at, stream, line)
	}
}

// set installs the real sink.
func (l *lateLogSink) set(sink agentconn.LogSink) {
	l.mu.Lock()
	l.sink = sink
	l.mu.Unlock()
}
