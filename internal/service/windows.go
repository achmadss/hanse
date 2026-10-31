package service

import (
	"time"
)

// Windows service definition of the node agent.
const (
	// WindowsDisplayName is the name that the service manager's console
	// shows.
	WindowsDisplayName = "Hanse node agent"
	// WindowsDescription explains the service in one sentence.
	WindowsDescription = "Runs Hanse capabilities on this node."
	// windowsRecoveryReset is the period after which the failure count
	// returns to the first recovery action.
	windowsRecoveryReset = 24 * time.Hour
)

// windowsRecovery is the restart delay after each failure. The service
// manager restarts the agent on the first, second, third, and later
// crashes, so an agent that fails at boot comes back without a person.
func windowsRecovery() []time.Duration {
	return []time.Duration{5 * time.Second, 10 * time.Second, 30 * time.Second}
}

// windowsPlan is the install or uninstall of the Windows service
// (docs/architecture.md section 8.4).
//
// The service runs as the virtual account NT SERVICE\hanse-node, which the
// installer adds to the local Administrators group by that group's
// well-known SID. A service started that way shows no UAC prompt and holds
// its administrator rights before anyone logs in.
func windowsPlan(o Options) Plan {
	plan := Plan{OS: o.OS, Action: o.Action}
	if o.Action == Uninstall {
		// Stopping, editing the group, and deleting a service that is not
		// installed are not failures. The configuration, the credential,
		// and the state directory stay, so a later install needs no new
		// enrollment.
		plan.Steps = []Step{
			{Kind: StepStopService, Target: WindowsName, IgnoreFailure: true,
				Description: "stop the " + WindowsName + " service"},
			{Kind: StepRemoveGroupMember, Group: AdministratorsSID, Member: WindowsAccount,
				IgnoreFailure: true,
				Description:   "remove " + WindowsAccount + " from the Administrators group"},
			{Kind: StepDeleteService, Target: WindowsName, IgnoreFailure: true,
				Description: "delete the " + WindowsName + " service"},
		}
		return plan
	}
	plan.Steps = []Step{
		{Kind: StepMkdir, Target: dirPath(o.OS, o.ConfigPath), Mode: 0o755,
			Description: "create " + dirPath(o.OS, o.ConfigPath)},
		// The service exists before the files get their access control
		// lists, because the service's virtual account has no SID until
		// the service manager has created it.
		{Kind: StepRegisterService, Service: &Spec{
			Name:          WindowsName,
			DisplayName:   WindowsDisplayName,
			Description:   WindowsDescription,
			Exe:           o.Binary,
			Args:          []string{"run", "--config", o.ConfigPath},
			Account:       WindowsAccount,
			Recovery:      windowsRecovery(),
			RecoveryReset: windowsRecoveryReset,
		}, Description: "register the " + WindowsName + " service as " + WindowsAccount},
		{Kind: StepAddGroupMember, Group: AdministratorsSID, Member: WindowsAccount,
			Description: "add " + WindowsAccount + " to the Administrators group"},
		{Kind: StepWriteFile, Target: o.ConfigPath, Content: agentYAML(o),
			Mode: 0o600, CreateOnly: true,
			Description: "write the default " + o.ConfigPath + " when there is none"},
		// An access control list replaces the inherited one: only
		// Administrators and SYSTEM may write agent.yaml, and the service
		// account may read it (docs/configuration.md section 7).
		{Kind: StepGrant, Target: o.ConfigPath, Access: AccessRead,
			Description: "let " + WindowsAccount + " read " + o.ConfigPath},
		{Kind: StepMkdir, Target: o.StateDir, Mode: 0o700,
			Description: "create the state directory " + o.StateDir},
		{Kind: StepGrant, Target: o.StateDir, Access: AccessModify,
			Description: "give the state directory " + o.StateDir + " to " + WindowsAccount},
		{Kind: StepGrant, Target: joinPath(o.OS, o.StateDir, credentialName), Access: AccessModify,
			IgnoreFailure: true,
			Description:   "give the node credential in " + o.StateDir + " to " + WindowsAccount},
	}
	if o.Start {
		plan.Steps = append(plan.Steps, Step{
			Kind: StepStartService, Target: WindowsName,
			Description: "start the " + WindowsName + " service",
		})
	}
	return plan
}
