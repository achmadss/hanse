package service

import (
	"strconv"
	"strings"
)

// DarwinLabel is the launchd job label.
const DarwinLabel = "dev.hanse.node"

// DarwinPlistPath is where launchd reads the node daemon. The daemons
// directory, not the agents directory, is what makes it start at boot with
// nobody logged in.
const DarwinPlistPath = "/Library/LaunchDaemons/" + DarwinLabel + ".plist"

// DarwinJobTarget is the service target that launchctl addresses.
const DarwinJobTarget = "system/" + DarwinLabel

// DarwinLogPath is where launchd sends the daemon's standard output and
// standard error. launchd opens the file as the service account, so
// install must create it: that account cannot create a file in /var/log.
const DarwinLogPath = "/var/log/hanse-node.log"

// darwinPlan is the install or uninstall of the launchd daemon
// (docs/architecture.md section 8.4).
func darwinPlan(o Options, acc Account) (Plan, error) {
	plan := Plan{OS: o.OS, Action: o.Action, Service: DarwinPlistPath}
	if o.Action == Uninstall {
		// Unloading a daemon that is not loaded is not a failure. The
		// configuration, the credential, and the state directory stay, so
		// a later install needs no new enrollment.
		plan.Steps = []Step{
			{Kind: StepRun, Args: []string{"launchctl", "bootout", DarwinJobTarget},
				IgnoreFailure: true, Quiet: true,
				Description: "unloaded " + DarwinLabel},
			{Kind: StepRemoveFile, Target: DarwinPlistPath, IgnoreFailure: true,
				Description: "removed " + DarwinPlistPath},
		}
		return plan, nil
	}
	plan.Account = acc.Name
	plan.Steps = posixFileSteps(o, acc)
	plan.Steps = append(plan.Steps,
		// launchd opens StandardOutPath and StandardErrorPath as the
		// service account, and that account cannot create a file in
		// /var/log. A missing log file therefore fails the spawn with
		// EX_CONFIG, so create it as root and give it to the account
		// before the daemon loads. A re-install repairs an existing file.
		Step{Kind: StepWriteFile, Target: DarwinLogPath, Mode: 0o600,
			Owner: accountOwner(acc), CreateOnly: true,
			Description: "wrote " + DarwinLogPath +
				" when there was none, and set its owner and mode"},
		// An installed daemon keeps the definition it was loaded with, so
		// unload the old one before the new plist replaces it. A first
		// install has nothing to unload, and launchctl reports that as
		// "No such process", so the step stays quiet.
		Step{Kind: StepRun, Args: []string{"launchctl", "bootout", DarwinJobTarget},
			IgnoreFailure: true, Quiet: true,
			Description: "unloaded an earlier " + DarwinLabel},
		Step{Kind: StepWriteFile, Target: DarwinPlistPath, Content: darwinPlist(o, acc),
			Mode: 0o644, Owner: rootOwner(),
			Description: "wrote the launchd daemon " + DarwinPlistPath},
		Step{Kind: StepRun, Args: []string{"launchctl", "bootstrap", "system", DarwinPlistPath},
			Description: "loaded " + DarwinLabel + " into launchd"},
		Step{Kind: StepRun, Args: []string{"launchctl", "enable", DarwinJobTarget},
			Description: "enabled " + DarwinLabel + " at boot"},
	)
	if o.Start {
		plan.Steps = append(plan.Steps, Step{
			Kind: StepRun, Args: []string{"launchctl", "kickstart", "-k", DarwinJobTarget},
			Description: "started " + DarwinLabel,
		})
	}
	return plan, nil
}

// darwinPlist renders the launchd daemon. KeepAlive with SuccessfulExit
// false restarts the agent after a crash, and a clean exit such as
// `launchctl bootout` stays stopped. RunAtLoad starts it when launchd
// loads a plist at boot.
func darwinPlist(o Options, acc Account) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>` + xmlText(DarwinLabel) + `</string>
	<key>ProgramArguments</key>
	<array>
		<string>` + xmlText(o.Binary) + `</string>
		<string>run</string>
		<string>--config</string>
		<string>` + xmlText(o.ConfigPath) + `</string>
	</array>
	<key>UserName</key>
	<string>` + xmlText(acc.Name) + `</string>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<dict>
		<key>SuccessfulExit</key>
		<false/>
	</dict>
	<key>ProcessType</key>
	<string>Background</string>
	<key>Umask</key>
	<integer>` + strconv.Itoa(0o077) + `</integer>
	<key>StandardOutPath</key>
	<string>` + DarwinLogPath + `</string>
	<key>StandardErrorPath</key>
	<string>` + DarwinLogPath + `</string>
</dict>
</plist>
`)
	return b.String()
}

// xmlText escapes a value for a plist string element. A path with an
// ampersand would otherwise produce a plist that launchd refuses.
func xmlText(value string) string {
	var b strings.Builder
	for _, r := range value {
		switch r {
		case '&':
			b.WriteString("&amp;")
		case '<':
			b.WriteString("&lt;")
		case '>':
			b.WriteString("&gt;")
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
