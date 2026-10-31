package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/achmadss/hanse/internal/config"
	"github.com/achmadss/hanse/internal/service"
)

// defaultNodeUser is the account that install runs the agent as when
// --user names none. The account must exist on the node; docs/node-setup.md
// shows how to create it.
const defaultNodeUser = "hanse-node"

// serviceArgs are the arguments that install and uninstall share.
type serviceArgs struct {
	user       string
	configPath string
	stateDir   string
}

// runInstall registers the agent as a system service, so it starts at boot
// before anyone logs in (docs/architecture.md section 8.4).
func runInstall(args []string, stdout, stderr io.Writer, configPath, stateDir string) int {
	parsed, ok := parseServiceArgs(args, configPath, stateDir, stderr)
	if !ok {
		return exitUse
	}
	return installService(service.NewHost(stdout, stderr), parsed, stdout, stderr)
}

// installService is runInstall with the operating system's calls behind an
// interface, so a test installs into a recorder without root.
func installService(host service.Host, args serviceArgs, stdout, stderr io.Writer) int {
	if code, ok := requireAdministrator(host, "install", stderr); !ok {
		return code
	}
	stateDir, err := resolveServiceStateDir(args.stateDir, args.configPath)
	if err != nil {
		printf(stderr, "hanse-node: %v\n", err)
		return exitFail
	}
	binary, err := os.Executable()
	if err != nil {
		printf(stderr, "hanse-node: cannot find the hanse-node executable: %v\n", err)
		return exitFail
	}
	account, err := serviceAccount(host, args.user)
	if err != nil {
		printf(stderr, "hanse-node: %v\n", err)
		return exitFail
	}
	start := credentialExists(stateDir)
	plan, err := service.Build(service.Options{
		OS:         runtime.GOOS,
		Action:     service.Install,
		User:       args.user,
		Binary:     binary,
		ConfigPath: args.configPath,
		StateDir:   stateDir,
		GatewayURL: enrolledGatewayURL(stateDir),
		Start:      start,
	}, account)
	if err != nil {
		printf(stderr, "hanse-node: %v\n", err)
		return exitFail
	}
	if code := applyPlan(plan, host, stdout, stderr); code != exitOK {
		return code
	}
	if start {
		printf(stdout, "hanse-node: installed %s for %s, and it is running now\n",
			plan.Service, plan.Account)
		printf(stdout, "hanse-node: it starts at boot without a login\n")
		return exitOK
	}
	printf(stdout, "hanse-node: installed %s for %s\n", plan.Service, plan.Account)
	printf(stdout, "hanse-node: it starts at boot without a login once the node has enrolled\n")
	printf(stdout, "hanse-node: this node has not enrolled yet: run `hanse-node join "+
		"<gateway-url> <join-token> --config %s`, then `hanse-node install` again "+
		"(docs/node-setup.md)\n", args.configPath)
	return exitOK
}

// runUninstall removes the system service. It keeps agent.yaml, the node
// credential, and the state directory, so a later install needs no new
// enrollment.
func runUninstall(args []string, stdout, stderr io.Writer, configPath, stateDir string) int {
	parsed, ok := parseServiceArgs(args, configPath, stateDir, stderr)
	if !ok {
		return exitUse
	}
	return uninstallService(service.NewHost(stdout, stderr), parsed, stdout, stderr)
}

// uninstallService is runUninstall with the operating system's calls
// behind an interface, so a test uninstalls from a recorder without root.
func uninstallService(host service.Host, args serviceArgs, stdout, stderr io.Writer) int {
	if code, ok := requireAdministrator(host, "uninstall", stderr); !ok {
		return code
	}
	stateDir, err := resolveServiceStateDir(args.stateDir, args.configPath)
	if err != nil {
		// Uninstall removes the service, so a configuration file it cannot
		// read must not stop it. The path appears only in the report.
		stateDir = defaultStateDir()
	}
	plan, err := service.Build(service.Options{
		OS:     runtime.GOOS,
		Action: service.Uninstall,
		User:   args.user,
	}, service.Account{})
	if err != nil {
		printf(stderr, "hanse-node: %v\n", err)
		return exitFail
	}
	if code := applyPlan(plan, host, stdout, stderr); code != exitOK {
		return code
	}
	printf(stdout, "hanse-node: removed %s; %s and the node credential in %s are kept\n",
		plan.Service, args.configPath, stateDir)
	return exitOK
}

// applyPlan carries out the plan and prints each step as it succeeds.
func applyPlan(plan service.Plan, host service.Host, stdout, stderr io.Writer) int {
	err := plan.Apply(host, func(step service.Step) {
		printf(stdout, "hanse-node: %s\n", step.Description)
	})
	if err != nil {
		printf(stderr, "hanse-node: %v\n", err)
		return exitFail
	}
	return exitOK
}

// requireAdministrator refuses install and uninstall where the process
// cannot register a service.
func requireAdministrator(host service.Host, action string, stderr io.Writer) (int, bool) {
	administrator, err := host.Administrator()
	if err != nil {
		printf(stderr, "hanse-node: cannot check this process's rights: %v\n", err)
		return exitFail, false
	}
	if administrator {
		return exitOK, true
	}
	if runtime.GOOS == "windows" {
		printf(stderr, "hanse-node: %s needs an elevated prompt: "+
			"open a terminal as an administrator and run the command again\n", action)
		return exitFail, false
	}
	printf(stderr, "hanse-node: %s must run as root, for example: sudo hanse-node %s\n", action, action)
	return exitFail, false
}

// serviceAccount returns the account that the service runs as. Windows
// always uses the virtual service account, so --user cannot name another
// one there.
func serviceAccount(host service.Host, name string) (service.Account, error) {
	if runtime.GOOS == "windows" {
		if name != "" && !strings.EqualFold(name, service.WindowsAccount) {
			return service.Account{}, fmt.Errorf(
				"a Windows service always runs as %s, so --user cannot name %q",
				service.WindowsAccount, name)
		}
		return service.Account{Name: service.WindowsAccount}, nil
	}
	if name == "" {
		name = defaultNodeUser
	}
	account, err := host.Account(name)
	if err != nil {
		return service.Account{}, fmt.Errorf("%v; create it first (docs/node-setup.md) or pass --user", err)
	}
	return account, nil
}

// parseServiceArgs reads the flags of install and uninstall.
func parseServiceArgs(args []string, configPath, stateDir string, stderr io.Writer) (serviceArgs, bool) {
	parsed := serviceArgs{configPath: configPath, stateDir: stateDir}
	for i := 0; i < len(args); i++ {
		flag := args[i]
		name := strings.TrimLeft(flag, "-")
		value, hasEq := "", false
		if eq := strings.IndexByte(name, '='); eq >= 0 {
			name, value, hasEq = name[:eq], name[eq+1:], true
		}
		switch name {
		case "user", "config", "state-dir":
			if !hasEq {
				if i+1 >= len(args) {
					printf(stderr, "hanse-node: %s needs a value\n", flag)
					return serviceArgs{}, false
				}
				i++
				value = args[i]
			}
			if value == "" {
				printf(stderr, "hanse-node: %s needs a value\n", flag)
				return serviceArgs{}, false
			}
			switch name {
			case "user":
				parsed.user = value
			case "config":
				parsed.configPath = value
			default:
				parsed.stateDir = value
			}
		default:
			if strings.HasPrefix(flag, "-") {
				printf(stderr, "hanse-node: unknown flag: %s\n", flag)
			} else {
				printf(stderr, "hanse-node: this command takes no arguments: %s\n", flag)
			}
			return serviceArgs{}, false
		}
	}
	return parsed, true
}

// resolveServiceStateDir picks the state directory that install reports
// and creates: --state-dir, then state_dir in agent.yaml, then the OS
// default. The result is absolute, because a service plan requires
// absolute paths, and a relative state_dir resolves against the file's
// directory (docs/configuration.md section 7).
func resolveServiceStateDir(flagDir, configPath string) (string, error) {
	if flagDir != "" {
		absolute, err := filepath.Abs(flagDir)
		if err != nil {
			return "", fmt.Errorf("cannot resolve the state directory %s: %v", flagDir, err)
		}
		return absolute, nil
	}
	if configPath != "" {
		fromConfig, ok, err := config.AgentStateDir(configPath)
		if err != nil {
			return "", err
		}
		if ok {
			absolute, err := filepath.Abs(fromConfig)
			if err != nil {
				return "", fmt.Errorf("cannot resolve the state directory %s: %v", fromConfig, err)
			}
			return absolute, nil
		}
	}
	return defaultStateDir(), nil
}

// credentialExists reports whether this node has enrolled, which is what
// decides whether install starts the service at once.
func credentialExists(stateDir string) bool {
	_, err := os.Stat(filepath.Join(stateDir, credentialFileName))
	return err == nil
}

// enrolledGatewayURL returns the Gateway URL that `hanse-node join`
// stored beside the credential, so the default agent.yaml carries it and
// the agent can start as soon as it is installed. A node that has not
// enrolled yet returns an empty URL.
func enrolledGatewayURL(stateDir string) string {
	data, err := os.ReadFile(filepath.Join(stateDir, credentialFileName))
	if err != nil {
		return ""
	}
	var stored credentialFile
	if err := json.Unmarshal(data, &stored); err != nil {
		return ""
	}
	return stored.GatewayURL
}
