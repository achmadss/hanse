package service

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Action selects what a plan does.
type Action int

const (
	// Install registers the service, writes the default agent.yaml, and
	// hands the files the agent reads to the service account.
	Install Action = iota
	// Uninstall removes the service. It keeps agent.yaml, the node
	// credential, and the state directory, so a later install needs no
	// new enrollment.
	Uninstall
)

// String names the action in an operator's words.
func (a Action) String() string {
	if a == Uninstall {
		return "uninstall"
	}
	return "install"
}

// Account is the local account that runs the agent
// (docs/architecture.md section 8.4).
type Account struct {
	// Name is the account's login name, as systemd's User= and launchd's
	// UserName use it.
	Name string
	// UID and GID are the POSIX ids. They are zero on Windows, where the
	// service account is a virtual account and owns no file.
	UID, GID int
	// Group is the POSIX name of the account's primary group. It is empty
	// on Windows.
	Group string
}

// Owner is the POSIX owner of a file or directory that a plan writes.
type Owner struct {
	// UID and GID are the numeric user and group ids.
	UID, GID int
}

// Options is one install or uninstall. The caller resolves the documented
// default paths of its own operating system
// (docs/configuration.md section 1.1) and passes them here.
type Options struct {
	// OS is the target operating system: linux, darwin, or windows.
	OS string
	// Action is what the plan does.
	Action Action
	// User is the account that runs the agent. It is the account named by
	// --user on Linux and macOS, and empty on Windows, where the account
	// is always [WindowsAccount].
	User string
	// Binary is the absolute path of the hanse-node executable.
	Binary string
	// ConfigPath is the agent.yaml that the service reads.
	ConfigPath string
	// StateDir is the state directory that the default agent.yaml names.
	StateDir string
	// GatewayURL is the Gateway's public URL from an existing enrollment.
	// Empty means the node has not enrolled yet, so the default
	// agent.yaml carries the field as a comment.
	GatewayURL string
	// Start asks the plan to start the service once it is registered.
	Start bool
}

// WindowsName is the name of the Windows service.
const WindowsName = "hanse-node"

// WindowsAccount is the virtual account that the Windows service runs as.
// The service manager creates it from the service name, so it exists only
// on Windows and needs no password.
const WindowsAccount = `NT SERVICE\hanse-node`

// AdministratorsSID is the well-known SID of the local Administrators
// group. The group's name is localized, so a plan never names it.
const AdministratorsSID = "S-1-5-32-544"

// credentialName is the node credential's file name inside the state
// directory (docs/configuration.md section 8.2).
const credentialName = "credential"

// Build renders the plan of one install or uninstall. The caller looks up
// acc and checks the process's rights, so Build is a pure function of its
// arguments and a test on any platform can assert every step.
func Build(o Options, acc Account) (Plan, error) {
	if err := o.validate(acc); err != nil {
		return Plan{}, err
	}
	switch o.OS {
	case "linux":
		return linuxPlan(o, acc)
	case "darwin":
		return darwinPlan(o, acc)
	default:
		return windowsPlan(o), nil
	}
}

// validate refuses every combination that no installer can carry out.
func (o Options) validate(acc Account) error {
	switch o.OS {
	case "linux", "darwin", "windows":
	default:
		return fmt.Errorf("the operating system %q has no service support", o.OS)
	}
	if o.Action != Install && o.Action != Uninstall {
		return fmt.Errorf("%q is neither an install nor an uninstall", o.Action)
	}
	if o.OS == "windows" && o.User != "" && !strings.EqualFold(o.User, WindowsAccount) {
		return fmt.Errorf("a Windows service always runs as %s, so --user cannot name %q",
			WindowsAccount, o.User)
	}
	if o.Action == Uninstall {
		return nil
	}
	for _, field := range []struct{ name, value string }{
		{"the hanse-node executable", o.Binary},
		{"the configuration file", o.ConfigPath},
		{"the state directory", o.StateDir},
	} {
		if !absolutePath(o.OS, field.value) {
			return fmt.Errorf("%s %q is not an absolute path", field.name, field.value)
		}
	}
	if o.OS != "windows" && (acc.Name == "" || acc.Group == "") {
		return errors.New("install needs the account that runs the agent, with its primary group")
	}
	return nil
}

// absolutePath reports whether path is absolute on the named operating
// system. filepath.IsAbs is not enough: a test asserts the Windows plan on
// every platform.
func absolutePath(goos, path string) bool {
	if goos != "windows" {
		return strings.HasPrefix(path, "/")
	}
	if strings.HasPrefix(path, `\\`) {
		return true
	}
	return len(path) >= 3 && isDriveLetter(path[0]) && path[1] == ':' &&
		(path[2] == '\\' || path[2] == '/')
}

// isDriveLetter reports whether c starts a Windows drive path.
func isDriveLetter(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// joinPath joins a directory and a file name on the named operating
// system, so a plan for another OS renders the path that OS expects.
func joinPath(goos, dir, name string) string {
	if goos == "windows" {
		return strings.TrimRight(dir, `\/`) + `\` + name
	}
	return strings.TrimRight(dir, "/") + "/" + name
}

// dirPath returns the directory that holds a file on the named operating
// system.
func dirPath(goos, path string) string {
	if goos == "windows" {
		if cut := strings.LastIndexAny(path, `\/`); cut > 0 {
			return path[:cut]
		}
		return path
	}
	if cut := strings.LastIndexByte(path, '/'); cut > 0 {
		return path[:cut]
	}
	return path
}

// Access is what the service account may do with a file or directory on
// Windows. POSIX systems use the file's mode and group instead.
type Access int

const (
	// AccessRead lets the account read a file.
	AccessRead Access = iota + 1
	// AccessModify lets the account read and write a file or a directory.
	AccessModify
)

// Spec is the Windows service that install registers. Windows
// describes a service as a set of properties; Linux and macOS describe it
// as a file.
type Spec struct {
	// Name is the service name, which is also the name of its virtual
	// account.
	Name string
	// DisplayName is the name that the service manager's console shows.
	DisplayName string
	// Description explains the service in one sentence.
	Description string
	// Exe is the absolute path of the hanse-node executable.
	Exe string
	// Args are the arguments the service manager passes to Exe.
	Args []string
	// Account is the service's logon account.
	Account string
	// Recovery is the restart delay after each failure, in order. An
	// empty list disables the recovery actions.
	Recovery []time.Duration
	// RecoveryReset is the period after which the failure count returns
	// to the first recovery action.
	RecoveryReset time.Duration
}

// quoteArg quotes one command line argument the way the Windows service
// manager stores it: CommandLineToArgvW reads a quoted argument, and a
// backslash only escapes a quote that follows it.
func quoteArg(arg string) string {
	if arg != "" && !strings.ContainsAny(arg, " \t\"") {
		return arg
	}
	var b strings.Builder
	b.WriteByte('"')
	slashes := 0
	for i := 0; i < len(arg); i++ {
		switch arg[i] {
		case '\\':
			slashes++
			b.WriteByte('\\')
		case '"':
			// Double every backslash that precedes the quote, then escape
			// the quote itself.
			for ; slashes > 0; slashes-- {
				b.WriteByte('\\')
			}
			b.WriteString(`\"`)
		default:
			slashes = 0
			b.WriteByte(arg[i])
		}
	}
	// A trailing backslash would escape the closing quote.
	for ; slashes > 0; slashes-- {
		b.WriteByte('\\')
	}
	b.WriteByte('"')
	return b.String()
}

// CommandLine is the command line that the service manager starts, with
// every argument quoted.
func (s Spec) CommandLine() string {
	parts := make([]string, 0, len(s.Args)+1)
	parts = append(parts, quoteArg(s.Exe))
	for _, arg := range s.Args {
		parts = append(parts, quoteArg(arg))
	}
	return strings.Join(parts, " ")
}

// accountOwner is the owner of a file that the service account owns.
func accountOwner(acc Account) *Owner {
	return &Owner{UID: acc.UID, GID: acc.GID}
}

// rootOwner is the owner of a file that only the administrator owns.
func rootOwner() *Owner {
	return &Owner{UID: 0, GID: 0}
}
