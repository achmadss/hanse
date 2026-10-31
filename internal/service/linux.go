package service

import (
	"strconv"
	"strings"
)

// LinuxUnitPath is where the systemd system unit lives. It is a system
// unit, not a user unit, so it starts at boot with nobody logged in.
const LinuxUnitPath = "/etc/systemd/system/hanse-node.service"

// LinuxUnitName is the unit name that systemctl addresses.
const LinuxUnitName = "hanse-node.service"

// linuxPlan is the install or uninstall of the systemd unit
// (docs/architecture.md section 8.4).
func linuxPlan(o Options, acc Account) (Plan, error) {
	plan := Plan{OS: o.OS, Action: o.Action}
	if o.Action == Uninstall {
		// Disabling a unit that is not installed is not a failure. The
		// configuration, the credential, and the state directory stay, so
		// a later install needs no new enrollment.
		plan.Steps = []Step{
			{Kind: StepRun, Args: []string{"systemctl", "disable", "--now", LinuxUnitName},
				IgnoreFailure: true,
				Description:   "stop " + LinuxUnitName + " and disable it at boot"},
			{Kind: StepRemoveFile, Target: LinuxUnitPath, IgnoreFailure: true,
				Description: "remove " + LinuxUnitPath},
			{Kind: StepRun, Args: []string{"systemctl", "daemon-reload"},
				Description: "reload the systemd manager configuration"},
		}
		return plan, nil
	}
	plan.Steps = posixFileSteps(o, acc)
	plan.Steps = append(plan.Steps,
		Step{Kind: StepWriteFile, Target: LinuxUnitPath, Content: linuxUnit(o, acc),
			Mode: 0o644, Owner: rootOwner(),
			Description: "write the systemd unit " + LinuxUnitPath},
		Step{Kind: StepRun, Args: []string{"systemctl", "daemon-reload"},
			Description: "reload the systemd manager configuration"},
		Step{Kind: StepRun, Args: []string{"systemctl", "enable", LinuxUnitName},
			Description: "enable " + LinuxUnitName + " at boot"},
	)
	if o.Start {
		plan.Steps = append(plan.Steps, Step{
			Kind: StepRun, Args: []string{"systemctl", "restart", LinuxUnitName},
			Description: "start " + LinuxUnitName,
		})
	}
	return plan, nil
}

// linuxUnit renders the systemd system unit.
//
// It runs the agent as the chosen account and restarts it after a crash.
// The hardening is deliberately light, because the unit's sandbox applies
// to the root supervisor and to the handler it starts as well: a
// privileged operation must be able to reach the whole system, and sudo
// must be able to gain root. NoNewPrivileges is therefore absent, and so
// are CapabilityBoundingSet and ProtectSystem, which would break sudo and
// privileged handlers (docs/node-setup.md explains this).
func linuxUnit(o Options, acc Account) string {
	var b strings.Builder
	b.WriteString(`[Unit]
Description=Hanse node agent
Documentation=https://github.com/achmadss/hanse/blob/main/docs/node-setup.md
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=`)
	b.WriteString(acc.Name)
	b.WriteString("\nGroup=")
	b.WriteString(acc.Group)
	b.WriteString("\nExecStart=")
	b.WriteString(systemdArg(o.Binary))
	b.WriteString(" run --config ")
	b.WriteString(systemdArg(o.ConfigPath))
	b.WriteString(`

# A crash is a failure, and systemd starts the agent again. A clean stop,
# such as a stop command from an operator, stays stopped.
Restart=on-failure
RestartSec=2s

# The agent stops its handlers on SIGTERM, so the main process gets the
# signal first. A handler that ignores it is killed with the rest of the
# control group when the stop timeout expires.
KillMode=mixed
TimeoutStopSec=30s

# Files the agent creates are private to it.
UMask=0077

# A handler gets its own /tmp, so one handler's temporary files never
# collide with another's. Nothing else is sandboxed: see the comment above.
PrivateTmp=yes

[Install]
WantedBy=multi-user.target
`)
	return b.String()
}

// systemdArg quotes one word of an ExecStart line when it needs quoting.
func systemdArg(value string) string {
	if value != "" && !strings.ContainsAny(value, " \t\n\"'\\") {
		return value
	}
	return strconv.Quote(value)
}
