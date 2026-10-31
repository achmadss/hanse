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

// darwinPlan is the install or uninstall of the launchd daemon
// (docs/architecture.md section 8.4).
func darwinPlan(o Options, acc Account) (Plan, error) {
	plan := Plan{OS: o.OS, Action: o.Action}
	if o.Action == Uninstall {
		// Unloading a daemon that is not loaded is not a failure. The
		// configuration, the credential, and the state directory stay, so
		// a later install needs no new enrollment.
		plan.Steps = []Step{
			{Kind: StepRun, Args: []string{"launchctl", "bootout", DarwinJobTarget},
				IgnoreFailure: true,
				Description:   "unload " + DarwinLabel},
			{Kind: StepRemoveFile, Target: DarwinPlistPath, IgnoreFailure: true,
				Description: "remove " + DarwinPlistPath},
		}
		return plan, nil
	}
	plan.Steps = posixFileSteps(o, acc)
	plan.Steps = append(plan.Steps,
		// An installed daemon keeps the definition it was loaded with, so
		// unload the old one before the new plist replaces it.
		Step{Kind: StepRun, Args: []string{"launchctl", "bootout", DarwinJobTarget},
			IgnoreFailure: true,
			Description:   "unload an earlier " + DarwinLabel},
		Step{Kind: StepWriteFile, Target: DarwinPlistPath, Content: darwinPlist(o, acc),
			Mode: 0o644, Owner: rootOwner(),
			Description: "write the launchd daemon " + DarwinPlistPath},
		Step{Kind: StepRun, Args: []string{"launchctl", "bootstrap", "system", DarwinPlistPath},
			Description: "load " + DarwinLabel + " into launchd"},
		Step{Kind: StepRun, Args: []string{"launchctl", "enable", DarwinJobTarget},
			Description: "enable " + DarwinLabel + " at boot"},
	)
	if o.Start {
		plan.Steps = append(plan.Steps, Step{
			Kind: StepRun, Args: []string{"launchctl", "kickstart", "-k", DarwinJobTarget},
			Description: "start " + DarwinLabel,
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
	<string>/var/log/hanse-node.log</string>
	<key>StandardErrorPath</key>
	<string>/var/log/hanse-node.log</string>
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
