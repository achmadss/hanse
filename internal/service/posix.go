package service

// posixFileSteps are the steps that Linux and macOS share: the directory
// that holds agent.yaml, the default agent.yaml itself, the state
// directory, and the credential inside it.
//
// agent.yaml is mode 0640 and owned by root with the service account's
// group, so the administrator changes it and the agent only reads it
// (docs/configuration.md section 7). The state directory and the
// credential belong to the service account, so the agent can read the
// credential that `hanse-node join` wrote as root.
func posixFileSteps(o Options, acc Account) []Step {
	configDir := dirPath(o.OS, o.ConfigPath)
	return []Step{
		{Kind: StepMkdir, Target: configDir, Mode: 0o755, Owner: rootOwner(),
			Description: "create " + configDir},
		{Kind: StepWriteFile, Target: o.ConfigPath, Content: agentYAML(o),
			Mode: 0o640, Owner: &Owner{UID: 0, GID: acc.GID}, CreateOnly: true,
			Description: "write the default " + o.ConfigPath + " when there is none"},
		{Kind: StepMkdir, Target: o.StateDir, Mode: 0o700, Owner: accountOwner(acc),
			Description: "create the state directory " + o.StateDir},
		{Kind: StepChown, Target: joinPath(o.OS, o.StateDir, credentialName),
			Mode: 0o600, Owner: accountOwner(acc), IgnoreFailure: true,
			Description: "give the node credential in " + o.StateDir + " to " + acc.Name},
	}
}
