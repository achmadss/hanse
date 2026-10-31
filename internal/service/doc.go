// Package service registers the Hanse node agent as an operating system
// service, so the agent starts at boot before anyone logs in
// (docs/architecture.md section 8.4). It also removes that service again.
//
// One install or uninstall is a [Plan]: an ordered list of [Step] values.
// Building a plan is a pure function of [Options], so a test on any
// platform asserts the systemd unit, the launchd plist, and the Windows
// service definition without touching the operating system. Every
// operating system call is a [Host] method, so the tests record the calls
// instead of making them and no test needs root.
package service
