package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"runtime"
	"strings"

	"github.com/achmadss/hanse/internal/capinstall"
	"github.com/achmadss/hanse/internal/client"
	hanseconfig "github.com/achmadss/hanse/internal/config"
	herrors "github.com/achmadss/hanse/internal/errors"
)

// defaultGatewayConfigPath is the default gateway.yaml location per
// operating system, the same one `hanse-gateway` uses
// (docs/configuration.md section 1.1).
func defaultGatewayConfigPath() string {
	switch runtime.GOOS {
	case "darwin":
		return "/usr/local/etc/hanse/gateway.yaml"
	case "windows":
		return `C:\ProgramData\Hanse\gateway\gateway.yaml`
	default:
		return "/etc/hanse/gateway.yaml"
	}
}

// capabilitiesInstallCommand implements
// `hanse capabilities install <git-url|path> [--ref <ref>]`.
func (a *app) capabilitiesInstallCommand(ctx context.Context, args []string) error {
	fs := newFlagSet("capabilities install")
	formatValue := formatFlag(fs)
	configValue := fs.String("config", "", "path to gateway.yaml")
	yesValue := fs.Bool("yes", false, "install without asking for confirmation")
	refValue := fs.String("ref", "", "git branch, tag, or commit to install")
	positional, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(positional) != 1 {
		return usagef("usage: hanse capabilities install <git-url|path> [--ref <ref>] [--config <gateway.yaml>] [--yes]")
	}
	fm, err := outputFormat(*formatValue)
	if err != nil {
		return err
	}
	dirs, err := a.capabilityDirs(*configValue)
	if err != nil {
		return err
	}
	target := dirs[0]

	pkg, err := capinstall.Prepare(positional[0], *refValue)
	if err != nil {
		return err
	}
	defer pkg.Cleanup()

	if path, ok, err := capinstall.Find(pkg.ID, dirs); err != nil {
		return err
	} else if ok {
		return herrors.New(herrors.OperationConflict,
			fmt.Sprintf("capability %q is already installed at %s; remove it first with \"hanse capabilities remove %s\"",
				pkg.ID, path, pkg.ID),
			map[string]any{"capability": pkg.ID})
	}

	preview := a.opts.Stdout
	if fm == formatJSON {
		// Keep standard output a single JSON document; the preview still
		// reaches the person who is asked to confirm.
		preview = a.opts.Stderr
	}
	if err := a.writeInstallPreview(preview, pkg, target); err != nil {
		return err
	}
	if err := a.confirmChange(fmt.Sprintf("Install capability %q into %s?", pkg.ID, target), *yesValue); err != nil {
		return err
	}
	path, err := pkg.Install(target)
	if err != nil {
		return err
	}
	return a.reportChange(ctx, fm, changeResult{
		Capability: pkg.ID,
		Path:       path,
		Source:     pkg.Source,
		Ref:        pkg.Ref,
		Commit:     pkg.SHA,
		verb:       "Installed",
	})
}

// capabilitiesRemoveCommand implements `hanse capabilities remove <id>`.
func (a *app) capabilitiesRemoveCommand(ctx context.Context, args []string) error {
	fs := newFlagSet("capabilities remove")
	formatValue := formatFlag(fs)
	configValue := fs.String("config", "", "path to gateway.yaml")
	yesValue := fs.Bool("yes", false, "remove without asking for confirmation")
	positional, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(positional) != 1 {
		return usagef("usage: hanse capabilities remove <id> [--config <gateway.yaml>] [--yes]")
	}
	fm, err := outputFormat(*formatValue)
	if err != nil {
		return err
	}
	dirs, err := a.capabilityDirs(*configValue)
	if err != nil {
		return err
	}
	id := positional[0]
	path, err := capinstall.Locate(id, dirs)
	if err != nil {
		return err
	}
	if fm == formatJSON {
		if _, err := fmt.Fprintf(a.opts.Stderr, "Remove capability %q from %s?\n", id, path); err != nil {
			return err
		}
	} else if _, err := fmt.Fprintf(a.opts.Stdout, "Capability: %s\nPath: %s\n", id, path); err != nil {
		return err
	}
	if err := a.confirmChange(fmt.Sprintf("Remove capability %q?", id), *yesValue); err != nil {
		return err
	}
	removed, err := capinstall.Remove(id, dirs)
	if err != nil {
		return err
	}
	return a.reportChange(ctx, fm, changeResult{Capability: id, Path: removed, verb: "Removed"})
}

// capabilityDirs loads gateway.yaml and returns its paths.capabilities
// entries. The first entry is the install target.
func (a *app) capabilityDirs(configPath string) ([]string, error) {
	path := configPath
	if path == "" {
		path = defaultGatewayConfigPath()
	}
	cfg, problems, err := hanseconfig.Load(path)
	if err != nil {
		return nil, herrors.New(herrors.Internal, err.Error(), nil)
	}
	if len(problems) > 0 {
		return nil, herrors.New(herrors.InvalidArguments,
			fmt.Sprintf("%s: %s", path, problems.Error()), nil)
	}
	dirs := cfg.Gateway.Paths.Capabilities
	if len(dirs) == 0 {
		return nil, herrors.New(herrors.Internal,
			fmt.Sprintf("%s: paths.capabilities is empty", path), nil)
	}
	return dirs, nil
}

// writeInstallPreview shows the capability, the pinned commit, and every
// operation the package adds, before the caller confirms.
func (a *app) writeInstallPreview(w io.Writer, pkg *capinstall.Package, target string) error {
	if _, err := fmt.Fprintf(w, "Capability: %s\n", oneLine(pkg.ID)); err != nil {
		return err
	}
	if pkg.Name != "" && pkg.Name != pkg.ID {
		if _, err := fmt.Fprintf(w, "Name: %s\n", oneLine(pkg.Name)); err != nil {
			return err
		}
	}
	if pkg.Description != "" {
		if _, err := fmt.Fprintf(w, "Description: %s\n", oneLine(pkg.Description)); err != nil {
			return err
		}
	}
	if pkg.Source != "" {
		if _, err := fmt.Fprintf(w, "Source: %s\n", oneLine(pkg.Source)); err != nil {
			return err
		}
	}
	if pkg.Ref != "" {
		if _, err := fmt.Fprintf(w, "Ref: %s\n", oneLine(pkg.Ref)); err != nil {
			return err
		}
	}
	if pkg.SHA != "" {
		if _, err := fmt.Fprintf(w, "Commit: %s\n", pkg.SHA); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(w, "Target: %s\n", oneLine(target)); err != nil {
		return err
	}
	tw := newTable(w)
	if _, err := fmt.Fprintln(tw, "\nOPERATION\tPRIVILEGED\tDESCRIPTION"); err != nil {
		return err
	}
	for _, op := range pkg.Operations {
		if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\n",
			oneLine(op.ID), describeBool(op.Privileged), oneLine(op.Description)); err != nil {
			return err
		}
	}
	return tw.Flush()
}

// confirmChange asks a yes/no question on an interactive terminal. --yes skips the
// prompt; without a terminal the command refuses rather than assume an
// answer.
func (a *app) confirmChange(question string, yes bool) error {
	if yes {
		return nil
	}
	if !a.stdinIsTerminal() {
		return usagef("refusing to change the installation without --yes: standard input is not a terminal")
	}
	if _, err := fmt.Fprintf(a.opts.Stderr, "%s [y/N] ", question); err != nil {
		return err
	}
	reader := bufio.NewReader(a.opts.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return herrors.New(herrors.Internal, "cannot read the confirmation: "+err.Error(), nil)
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	if answer != "y" && answer != "yes" {
		return herrors.New(herrors.Internal, "the change was not confirmed; nothing was changed", nil)
	}
	return nil
}

// changeResult is the text and JSON output of install and remove.
type changeResult struct {
	Capability string                 `json:"capability"`
	Path       string                 `json:"path"`
	Source     string                 `json:"source,omitempty"`
	Ref        string                 `json:"ref,omitempty"`
	Commit     string                 `json:"commit,omitempty"`
	Reloaded   bool                   `json:"reloaded"`
	Warnings   []client.ReloadWarning `json:"warnings"`

	// verb is the past tense the text output starts with. JSON ignores it.
	verb string
}

// reportChange prints the change, asks the Gateway to reload, and reports
// the result. A Gateway that is unreachable or that refuses the reload is a
// warning only: the file change already happened and exits successfully.
func (a *app) reportChange(ctx context.Context, fm format, result changeResult) error {
	reload, reloaded := a.reloadAfterChange(ctx)
	result.Reloaded = reloaded
	result.Warnings = reload.Warnings
	if result.Warnings == nil {
		result.Warnings = []client.ReloadWarning{}
	}
	if fm == formatJSON {
		return writeJSON(a.opts.Stdout, result)
	}
	if _, err := fmt.Fprintf(a.opts.Stdout, "%s capability %q in %s.\n", result.verb, result.Capability, result.Path); err != nil {
		return err
	}
	if reloaded {
		if _, err := fmt.Fprintf(a.opts.Stdout, "Reloaded the Gateway: %d capabilities.\n", len(reload.Capabilities)); err != nil {
			return err
		}
	}
	return nil
}

// reloadAfterChange calls POST /v1/capabilities/reload with the CLI login.
// When the CLI is not logged in or the Gateway is unreachable, it warns that
// the change needs a reload or a restart and returns false.
func (a *app) reloadAfterChange(ctx context.Context) (client.Reload, bool) {
	c, _, err := a.open(ctx)
	if err != nil {
		a.warnf("the change takes effect after a reload or a restart: %s", err)
		return client.Reload{}, false
	}
	reload, err := c.ReloadCapabilities(ctx)
	if err != nil {
		a.warnf("the change takes effect after a reload or a restart: %s", err)
		return client.Reload{}, false
	}
	for _, warning := range reload.Warnings {
		a.warnf("capability %s: %s", warning.Capability, warning.Message)
	}
	return reload, true
}
