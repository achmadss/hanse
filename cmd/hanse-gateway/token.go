package main

import (
	stderrors "errors"
	"io"
	"strings"

	"github.com/achmadss/hanse/internal/auth"
	"github.com/achmadss/hanse/internal/config"
)

// runToken runs the `token` subcommands: add, list, and revoke. They
// manage the tokens file named in gateway.yaml
// (docs/configuration.md §1): the file stores token hashes only, and a
// new plain token goes to stdout exactly once, never through a log.
func runToken(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		tokenUsage(stderr)
		return exitUse
	}
	switch args[0] {
	case "add":
		return runTokenAdd(args[1:], stdout, stderr)
	case "list":
		return runTokenList(args[1:], stdout, stderr)
	case "revoke":
		return runTokenRevoke(args[1:], stdout, stderr)
	}
	tokenUsage(stderr)
	return exitUse
}

func tokenUsage(w io.Writer) {
	printf(w, "usage: hanse-gateway token add <name> [--allow <cap.op>,...] [--config <path>]\n")
	printf(w, "       hanse-gateway token list [--config <path>]\n")
	printf(w, "       hanse-gateway token revoke <name> [--config <path>]\n")
}

// parsedFlags are the flags and positionals of one token subcommand.
type parsedFlags struct {
	config      string
	allow       string
	positionals []string
}

// parseTokenArgs scans subcommand arguments without the flag package:
// the name of a token is a plain word, so flag parsing would stop at it
// and miss trailing flags. Unknown flags are errors, as everywhere else
// (docs/configuration.md §2: a misspelled key never gets ignored
// silently). withAllow records whether this subcommand takes --allow.
func parseTokenArgs(args []string, withAllow bool, stderr io.Writer) (parsedFlags, bool) {
	var out parsedFlags
	for i := 0; i < len(args); i++ {
		s := args[i]
		// Strip the leading dashes and look for the "=" form.
		flagName := strings.TrimLeft(s, "-")
		value, hasEq := "", false
		if eq := strings.IndexByte(flagName, '='); eq >= 0 {
			flagName, value, hasEq = flagName[:eq], flagName[eq+1:], true
		}
		takesValue := func() (string, bool) {
			if hasEq {
				return value, true
			}
			if i+1 >= len(args) {
				return "", false
			}
			i++
			return args[i], true
		}
		switch flagName {
		case "config", "allow":
			if flagName == "allow" && !withAllow {
				printf(stderr, "hanse-gateway: unknown flag: %s\n", s)
				return out, false
			}
			if hasEq {
				break
			}
			v, ok := takesValue()
			if !ok {
				printf(stderr, "hanse-gateway: %s needs a value\n", s)
				return out, false
			}
			value = v
		default:
			if strings.HasPrefix(s, "-") {
				printf(stderr, "hanse-gateway: unknown flag: %s\n", s)
				return out, false
			}
			out.positionals = append(out.positionals, s)
			continue
		}
		switch flagName {
		case "config":
			out.config = value
		default:
			out.allow = value
		}
	}
	return out, true
}

// loadTokensFile loads gateway.yaml to resolve paths and then the
// tokens file it names. Answering `token` requires the whole
// configuration, because the tokens file's default location depends on
// paths.state_dir.
func loadTokensFile(stderr io.Writer, cfgPath string) (*auth.Store, bool) {
	cfg, problems, err := config.Load(cfgPath)
	if err != nil {
		printf(stderr, "hanse-gateway: %v\n", err)
		return nil, false
	}
	if len(problems) > 0 {
		for _, p := range problems {
			printf(stderr, "%s\n", p.Error())
		}
		printf(stderr, "hanse-gateway: %s: %d configuration problem(s)\n", cfgPath, len(problems))
		return nil, false
	}
	store, err := auth.LoadStore(cfg.Gateway.Auth.TokensFile)
	if err != nil {
		printf(stderr, "hanse-gateway: %v\n", err)
		return nil, false
	}
	return store, true
}

// oneName requires exactly one positional argument: the token name.
func oneName(f parsedFlags, stderr io.Writer) (string, bool) {
	if len(f.positionals) != 1 {
		tokenUsage(stderr)
		return "", false
	}
	return f.positionals[0], true
}

// runTokenAdd creates a token for name and prints it once to stdout.
// The printed line is the plain token; nothing else about the token is
// ever output, so the printed value is the one copy of it.
func runTokenAdd(args []string, stdout, stderr io.Writer) int {
	f, ok := parseTokenArgs(args, true, stderr)
	if !ok {
		return exitUse
	}
	name, ok := oneName(f, stderr)
	if !ok {
		return exitUse
	}
	store, ok := loadTokensFile(stderr, f.config)
	if !ok {
		return exitFail
	}
	var allow []string
	if f.allow != "" {
		allow = strings.Split(f.allow, ",")
	}
	plain, err := store.Add(name, allow)
	if err != nil {
		printfTokenOpError(stderr, "add", name, err)
		return exitFail
	}
	// The token appears exactly once, here.
	printf(stdout, "%s\n", plain)
	return exitOK
}

// runTokenList prints the stored tokens: their names, when they were
// created, and their allow lists. It never prints a hash or, of
// course, a plain token.
func runTokenList(args []string, stdout, stderr io.Writer) int {
	f, ok := parseTokenArgs(args, false, stderr)
	if !ok {
		return exitUse
	}
	if len(f.positionals) != 0 {
		tokenUsage(stderr)
		return exitUse
	}
	store, ok := loadTokensFile(stderr, f.config)
	if !ok {
		return exitFail
	}
	records := store.List()
	printf(stdout, "name\tcreated\tallow\n")
	for _, r := range records {
		printf(stdout, "%s\t%s\t%s\n", r.Name, r.Created, allowCSV(r.Allow))
	}
	return exitOK
}

// allowCSV renders an allow list: "*" for the default allow-everything
// form, otherwise the entries comma-joined.
func allowCSV(allow []string) string {
	if len(allow) == 0 {
		return "*"
	}
	return strings.Join(allow, ",")
}

// runTokenRevoke removes the named token. Requests that still present
// the revoked token fail authentication.
func runTokenRevoke(args []string, stdout, stderr io.Writer) int {
	f, ok := parseTokenArgs(args, false, stderr)
	if !ok {
		return exitUse
	}
	name, ok := oneName(f, stderr)
	if !ok {
		return exitUse
	}
	store, ok := loadTokensFile(stderr, f.config)
	if !ok {
		return exitFail
	}
	if err := store.Revoke(name); err != nil {
		printfTokenOpError(stderr, "revoke", name, err)
		return exitFail
	}
	printf(stdout, "revoked %s\n", name)
	return exitOK
}

// printfTokenOpError prints one token-operation failure. The messages
// name the token's name, never a token value.
func printfTokenOpError(stderr io.Writer, verb, name string, err error) {
	if !stderrors.Is(err, auth.ErrNotFound) {
		printf(stderr, "hanse-gateway: %v\n", err)
		return
	}
	if verb == "add" {
		printf(stderr, "hanse-gateway: the token name %q already exists\n", name)
		return
	}
	printf(stderr, "hanse-gateway: the token name %q does not exist\n", name)
}
