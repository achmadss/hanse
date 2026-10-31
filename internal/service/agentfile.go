package service

import (
	"strconv"
	"strings"
)

// agentYAML renders the default agent.yaml that install writes when the
// node has none (docs/configuration.md section 7). The values are the
// documented defaults, so the file is complete and easy to review.
//
// The Gateway URL comes from an existing enrollment: `hanse-node join`
// stores it beside the credential, and install copies it here, so one
// join and one install leave the node ready to start. A node that has not
// enrolled yet gets the field as a comment, and join fills it in when it
// is given the same path.
func agentYAML(o Options) string {
	var b strings.Builder
	b.WriteString("# Hanse node agent configuration. See docs/configuration.md.\n")
	b.WriteString("# Only this node's administrator may change this file.\n")
	b.WriteString("version: 1\n\n")
	if o.GatewayURL != "" {
		b.WriteString("gateway_url: " + strconv.Quote(o.GatewayURL) + "\n")
	} else {
		b.WriteString("# Run `hanse-node join <gateway-url> <join-token>` first, then install\n")
		b.WriteString("# again, or `hanse-node join --config <this file>`.\n")
		b.WriteString("# gateway_url: https://gateway.example.com\n")
	}
	b.WriteString("\n")
	b.WriteString("state_dir: " + o.StateDir + "\n\n")
	b.WriteString(`max_concurrent_runs: 16
buffer_limit: 10MiB

handlers:
  env_allow: []

elevation:
  enabled: false
  # Allow only these privileged operations. An empty list allows all of them.
  operations: []
  # Windows only: the administrator account whose password authorizes elevation.
  # account: admin-user
  lockout:
    attempts: 5
    window: 10m
    duration: 10m

log:
  level: info
  format: text
`)
	return b.String()
}
