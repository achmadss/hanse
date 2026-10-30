package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"

	"github.com/achmadss/hanse/internal/api"
	"github.com/achmadss/hanse/internal/client"
	herrors "github.com/achmadss/hanse/internal/errors"
)

// exitCode maps an error to the CLI exit code of
// docs/architecture.md section 14.
func exitCode(err error) int {
	return herrors.ExitCode(err)
}

// versionCommand prints the CLI version.
func (a *app) versionCommand(args []string) error {
	fs := newFlagSet("version")
	formatValue := formatFlag(fs)
	positional, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(positional) != 0 {
		return usagef("version takes no arguments")
	}
	fm, err := outputFormat(*formatValue)
	if err != nil {
		return err
	}
	if fm == formatJSON {
		return writeJSON(a.opts.Stdout, struct {
			Version string `json:"version"`
		}{Version: versionValue()})
	}
	_, err = fmt.Fprintf(a.opts.Stdout, "hanse %s\n", versionValue())
	return err
}

// loginCommand saves the Gateway URL and token, then reports the Gateway's
// API version. Nothing is saved unless the Gateway answers GET /v1/version
// with the given credentials.
func (a *app) loginCommand(ctx context.Context, args []string) error {
	fs := newFlagSet("login")
	formatValue := formatFlag(fs)
	gatewayURL := fs.String("url", "", "Gateway root URL")
	tokenFlag := fs.String("token", "", "bearer token (prefer standard input)")
	positional, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(positional) != 0 {
		return usagef("login takes no arguments")
	}
	fm, err := outputFormat(*formatValue)
	if err != nil {
		return err
	}
	rawURL := strings.TrimSpace(*gatewayURL)
	if rawURL == "" {
		return usagef(`login needs --url, for example --url https://hanse.example`)
	}
	token, err := a.readToken(*tokenFlag)
	if err != nil {
		return err
	}
	c, err := client.New(client.Config{BaseURL: rawURL, Token: token})
	if err != nil {
		return usagef("%s", err)
	}

	// Verify the URL and token before saving anything, so a typo never
	// leaves a configuration that fails every later command.
	gatewayVersion, err := c.Version(ctx)
	if err != nil {
		return err
	}
	a.warnVersionMismatch(gatewayVersion.APIVersion)

	path, err := a.configPath()
	if err != nil {
		return herrors.New(herrors.Internal, err.Error(), nil)
	}
	if err := saveConfig(path, config{GatewayURL: c.BaseURL(), Token: token}); err != nil {
		return herrors.New(herrors.Internal, err.Error(), nil)
	}

	if fm == formatJSON {
		return writeJSON(a.opts.Stdout, struct {
			GatewayURL string `json:"gateway_url"`
			APIVersion string `json:"api_version"`
		}{GatewayURL: c.BaseURL(), APIVersion: gatewayVersion.APIVersion})
	}
	if _, err := fmt.Fprintf(a.opts.Stdout, "Logged in to %s (API %s).\n", c.BaseURL(), gatewayVersion.APIVersion); err != nil {
		return err
	}
	_, err = fmt.Fprintf(a.opts.Stdout, "Credentials saved to %s.\n", path)
	return err
}

// readToken returns the token from the flag, or from standard input when
// the flag is empty. An interactive terminal is refused: reading a token
// without hiding the echo would show it on screen.
func (a *app) readToken(flagValue string) (string, error) {
	if token := strings.TrimSpace(flagValue); token != "" {
		return token, nil
	}
	if a.stdinIsTerminal() {
		return "", usagef("the token must come from --token or standard input")
	}
	data, err := io.ReadAll(io.LimitReader(a.opts.Stdin, 1<<20))
	if err != nil {
		return "", herrors.New(herrors.Internal, "cannot read the token from standard input: "+err.Error(), nil)
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return "", usagef("the token is empty: pass --token or write the token to standard input")
	}
	return token, nil
}

// stdinIsTerminal reports whether standard input is interactive.
func (a *app) stdinIsTerminal() bool {
	if a.opts.StdinIsTerminal {
		return true
	}
	f, ok := a.opts.Stdin.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// open loads the configuration, builds a client, and checks the Gateway's
// API version. The version check is best effort: the command itself
// reports an unreachable Gateway or a rejected token.
func (a *app) open(ctx context.Context) (*client.Client, config, error) {
	path, err := a.configPath()
	if err != nil {
		return nil, config{}, herrors.New(herrors.Internal, err.Error(), nil)
	}
	cfg, err := loadConfig(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, config{}, herrors.New(herrors.AuthenticationFailed,
				fmt.Sprintf("not logged in: no CLI configuration at %s; run \"hanse login --url <gateway-url>\"", path), nil)
		}
		return nil, config{}, herrors.New(herrors.Internal, err.Error(), nil)
	}
	if cfg.GatewayURL == "" || cfg.Token == "" {
		return nil, config{}, herrors.New(herrors.AuthenticationFailed,
			fmt.Sprintf("%s has no Gateway URL or token; run \"hanse login\" again", path), nil)
	}
	c, err := client.New(client.Config{BaseURL: cfg.GatewayURL, Token: cfg.Token})
	if err != nil {
		return nil, config{}, herrors.New(herrors.Internal, err.Error(), nil)
	}
	if v, err := c.Version(ctx); err == nil {
		a.warnVersionMismatch(v.APIVersion)
	}
	return c, cfg, nil
}

// warnVersionMismatch warns on stderr when the Gateway speaks another API
// version than the CLI. The warning never changes the exit code.
func (a *app) warnVersionMismatch(gatewayAPIVersion string) {
	if gatewayAPIVersion == "" || gatewayAPIVersion == api.APIVersion {
		return
	}
	a.warnf("the CLI speaks API version %s but the Gateway speaks %s", api.APIVersion, gatewayAPIVersion)
}

// capabilitiesCommand implements capabilities and capabilities show.
func (a *app) capabilitiesCommand(ctx context.Context, args []string) error {
	fs := newFlagSet("capabilities")
	formatValue := formatFlag(fs)
	positional, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	fm, err := outputFormat(*formatValue)
	if err != nil {
		return err
	}
	switch {
	case len(positional) == 0:
		c, _, err := a.open(ctx)
		if err != nil {
			return err
		}
		caps, err := c.Capabilities(ctx)
		if err != nil {
			return err
		}
		return a.writeCapabilities(fm, caps)
	case len(positional) == 2 && positional[0] == "show":
		c, _, err := a.open(ctx)
		if err != nil {
			return err
		}
		capability, err := c.Capability(ctx, positional[1])
		if err != nil {
			return err
		}
		return a.writeCapability(fm, capability)
	default:
		return usagef("usage: hanse capabilities [show <id>]")
	}
}

// writeCapabilities prints the capability list.
func (a *app) writeCapabilities(fm format, caps client.Capabilities) error {
	if fm == formatJSON {
		return writeJSON(a.opts.Stdout, caps)
	}
	tw := newTable(a.opts.Stdout)
	if _, err := fmt.Fprintln(tw, "ID\tNAME\tHANDLER\tOPERATIONS"); err != nil {
		return err
	}
	for _, capability := range caps.Capabilities {
		if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n",
			oneLine(capability.ID), oneLine(capability.Name),
			oneLine(capability.Handler.Kind), oneLine(joinIDs(capability.Operations))); err != nil {
			return err
		}
	}
	return tw.Flush()
}

// writeCapability prints one capability.
func (a *app) writeCapability(fm format, capability client.Capability) error {
	if fm == formatJSON {
		return writeJSON(a.opts.Stdout, client.CapabilityEnvelope{Capability: capability})
	}
	rows := [][2]string{
		{"ID", capability.ID},
		{"Name", capability.Name},
		{"Description", capability.Description},
	}
	if capability.Contract != "" {
		rows = append(rows, [2]string{"Contract", capability.Contract})
	}
	rows = append(rows, [2]string{"Handler", capability.Handler.Kind})
	if err := writeKeyValue(a.opts.Stdout, rows); err != nil {
		return err
	}
	if len(capability.Operations) == 0 {
		return nil
	}
	if _, err := fmt.Fprintln(a.opts.Stdout, "\nOperations:"); err != nil {
		return err
	}
	tw := newTable(a.opts.Stdout)
	if _, err := fmt.Fprintln(tw, "  ID\tMODE\tPRIVILEGED\tDESCRIPTION"); err != nil {
		return err
	}
	for _, op := range capability.Operations {
		if _, err := fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n",
			oneLine(op.ID), oneLine(op.Mode), describeBool(op.Privileged), oneLine(op.Description)); err != nil {
			return err
		}
	}
	return tw.Flush()
}

// nodesCommand implements nodes, nodes show, and nodes capabilities.
func (a *app) nodesCommand(ctx context.Context, args []string) error {
	fs := newFlagSet("nodes")
	formatValue := formatFlag(fs)
	positional, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	fm, err := outputFormat(*formatValue)
	if err != nil {
		return err
	}
	switch {
	case len(positional) == 0:
		c, _, err := a.open(ctx)
		if err != nil {
			return err
		}
		nodes, err := c.Nodes(ctx)
		if err != nil {
			return err
		}
		return a.writeNodes(fm, nodes)
	case len(positional) == 2 && positional[0] == "show":
		c, _, err := a.open(ctx)
		if err != nil {
			return err
		}
		node, err := c.Node(ctx, positional[1])
		if err != nil {
			return err
		}
		return a.writeNode(fm, node)
	case len(positional) == 2 && positional[0] == "capabilities":
		c, _, err := a.open(ctx)
		if err != nil {
			return err
		}
		caps, err := c.NodeCapabilities(ctx, positional[1])
		if err != nil {
			return err
		}
		return a.writeNodeCapabilities(fm, caps)
	default:
		return usagef("usage: hanse nodes [show <id> | capabilities <id>]")
	}
}

// writeNodes prints the node list.
func (a *app) writeNodes(fm format, nodes client.Nodes) error {
	if fm == formatJSON {
		return writeJSON(a.opts.Stdout, nodes)
	}
	tw := newTable(a.opts.Stdout)
	if _, err := fmt.Fprintln(tw, "ID\tOS\tARCH\tROLES\tSTATUS\tENABLED"); err != nil {
		return err
	}
	for _, node := range nodes.Nodes {
		if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			oneLine(node.ID), oneLine(node.OS), oneLine(node.Arch),
			oneLine(strings.Join(node.Roles, ", ")), oneLine(node.Status),
			describeBool(node.Enabled)); err != nil {
			return err
		}
	}
	return tw.Flush()
}

// writeNode prints one node.
func (a *app) writeNode(fm format, node client.Node) error {
	if fm == formatJSON {
		return writeJSON(a.opts.Stdout, client.NodeEnvelope{Node: node})
	}
	rows := [][2]string{
		{"ID", node.ID},
		{"OS", node.OS},
		{"Arch", node.Arch},
		{"Roles", strings.Join(node.Roles, ", ")},
		{"Status", node.Status},
		{"Enabled", describeBool(node.Enabled)},
	}
	if err := writeKeyValue(a.opts.Stdout, rows); err != nil {
		return err
	}
	if len(node.Attributes) == 0 {
		return nil
	}
	if _, err := fmt.Fprintln(a.opts.Stdout, "\nAttributes:"); err != nil {
		return err
	}
	tw := newTable(a.opts.Stdout)
	if _, err := fmt.Fprintln(tw, "  KEY\tVALUE"); err != nil {
		return err
	}
	for _, key := range sortedKeys(node.Attributes) {
		if _, err := fmt.Fprintf(tw, "  %s\t%s\n", oneLine(key), oneLine(node.Attributes[key])); err != nil {
			return err
		}
	}
	return tw.Flush()
}

// writeNodeCapabilities prints the capabilities of one node.
func (a *app) writeNodeCapabilities(fm format, caps client.NodeCapabilities) error {
	if fm == formatJSON {
		return writeJSON(a.opts.Stdout, caps)
	}
	if _, err := fmt.Fprintf(a.opts.Stdout, "Node: %s\n\n", oneLine(caps.Node)); err != nil {
		return err
	}
	tw := newTable(a.opts.Stdout)
	if _, err := fmt.Fprintln(tw, "CAPABILITY\tOPERATIONS"); err != nil {
		return err
	}
	for _, capability := range caps.Capabilities {
		if _, err := fmt.Fprintf(tw, "%s\t%s\n", oneLine(capability.ID), oneLine(strings.Join(capability.Operations, ", "))); err != nil {
			return err
		}
	}
	return tw.Flush()
}

// schemaCommand implements schema <capability> <operation>.
func (a *app) schemaCommand(ctx context.Context, args []string) error {
	fs := newFlagSet("schema")
	formatValue := formatFlag(fs)
	positional, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if _, err := outputFormat(*formatValue); err != nil {
		return err
	}
	if len(positional) != 2 {
		return usagef("usage: hanse schema <capability> <operation>")
	}
	c, _, err := a.open(ctx)
	if err != nil {
		return err
	}
	schema, err := c.Schema(ctx, positional[0], positional[1])
	if err != nil {
		return err
	}
	// Text and JSON both print the schema itself, which is already JSON.
	return writeSchemaJSON(a.opts.Stdout, schema)
}

// optionsCommand implements options <capability> <operation> <field>.
func (a *app) optionsCommand(ctx context.Context, args []string) error {
	fs := newFlagSet("options")
	formatValue := formatFlag(fs)
	positional, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	fm, err := outputFormat(*formatValue)
	if err != nil {
		return err
	}
	if len(positional) != 3 {
		return usagef("usage: hanse options <capability> <operation> <field>")
	}
	c, _, err := a.open(ctx)
	if err != nil {
		return err
	}
	options, err := c.Options(ctx, positional[0], positional[1], positional[2])
	if err != nil {
		return err
	}
	if fm == formatJSON {
		return writeJSON(a.opts.Stdout, options)
	}
	tw := newTable(a.opts.Stdout)
	if _, err := fmt.Fprintln(tw, "VALUE\tLABEL\tDESCRIPTION"); err != nil {
		return err
	}
	for _, option := range options.Options {
		if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\n",
			oneLine(option.Value), oneLine(option.Label), oneLine(option.Description)); err != nil {
			return err
		}
	}
	return tw.Flush()
}
