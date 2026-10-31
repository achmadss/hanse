package service

import (
	"fmt"
	"os"
)

// StepKind is what one step of a plan does.
type StepKind string

// The kinds of step that a plan can contain. Linux and macOS use file and
// command steps; Windows uses the service steps, because its service
// manager keeps the definition itself.
const (
	// StepMkdir creates a directory and its parents.
	StepMkdir StepKind = "mkdir"
	// StepWriteFile writes a file.
	StepWriteFile StepKind = "write-file"
	// StepChown sets the mode and the owner of an existing file.
	StepChown StepKind = "chown"
	// StepRemoveFile removes a file.
	StepRemoveFile StepKind = "remove-file"
	// StepRun runs a service manager command.
	StepRun StepKind = "run"
	// StepRegisterService creates or updates a Windows service.
	StepRegisterService StepKind = "register-service"
	// StepStartService starts a Windows service.
	StepStartService StepKind = "start-service"
	// StepStopService stops a Windows service.
	StepStopService StepKind = "stop-service"
	// StepDeleteService removes a Windows service.
	StepDeleteService StepKind = "delete-service"
	// StepAddGroupMember adds the service account to a local group.
	StepAddGroupMember StepKind = "add-group-member"
	// StepRemoveGroupMember removes the service account from a local group.
	StepRemoveGroupMember StepKind = "remove-group-member"
	// StepGrant gives the service account access to a file or directory.
	StepGrant StepKind = "grant"
)

// Step is one action of a plan.
type Step struct {
	// Kind is what the step does.
	Kind StepKind
	// Target is the file, directory, unit, or service the step acts on.
	Target string
	// Args is the command line of a [StepRun].
	Args []string
	// Content is the complete content of a [StepWriteFile].
	Content string
	// Mode is the POSIX permission of a file or directory the step
	// writes. Windows ignores it.
	Mode os.FileMode
	// Owner is the POSIX owner of a file or directory the step writes.
	// Nil leaves the owner as it is.
	Owner *Owner
	// CreateOnly keeps an existing file and its content.
	CreateOnly bool
	// IgnoreFailure marks a step that may find nothing to do, such as
	// removing a service that is not installed, so a second install or
	// uninstall still succeeds.
	IgnoreFailure bool
	// Quiet keeps a step out of the progress report and discards the
	// output of its command. It marks a step that usually finds nothing
	// to do, such as unloading a daemon that was never loaded.
	Quiet bool
	// Service is the definition of a [StepRegisterService].
	Service *Spec
	// Group is the local group's SID in a group membership step.
	Group string
	// Member is the account in a group membership step.
	Member string
	// Access is what a [StepGrant] allows.
	Access Access
	// Description is the step in an operator's words, printed as it
	// succeeds.
	Description string
}

// Plan is one install or uninstall.
type Plan struct {
	// OS is the target operating system.
	OS string
	// Action is what the plan does.
	Action Action
	// Service is the definition that the plan installs or removes: the
	// unit file, the launchd plist, or the Windows service name.
	Service string
	// Account is the account the service runs as, when the plan knows it.
	Account string
	// Steps run in order.
	Steps []Step
}

// Apply carries out every step in order. A step that fails stops the plan
// and reports the reason, so a half-installed service is never started.
// progress, when it is not nil, is called after each successful step.
func (p Plan) Apply(h Host, progress func(Step)) error {
	for _, step := range p.Steps {
		if err := step.apply(h); err != nil {
			if step.IgnoreFailure {
				report(step, progress)
				continue
			}
			return fmt.Errorf("%s: %w", step.Description, err)
		}
		report(step, progress)
	}
	return nil
}

// report tells the caller that a step succeeded, unless the step is quiet.
func report(step Step, progress func(Step)) {
	if progress != nil && !step.Quiet {
		progress(step)
	}
}

// apply carries out one step through the host.
func (s Step) apply(h Host) error {
	switch s.Kind {
	case StepMkdir:
		return h.MkdirAll(s.Target, s.Mode, s.Owner)
	case StepWriteFile:
		return h.WriteFile(s.Target, []byte(s.Content), s.Mode, s.Owner, s.CreateOnly)
	case StepChown:
		return h.Chown(s.Target, s.Mode, s.Owner)
	case StepRemoveFile:
		return h.RemoveFile(s.Target)
	case StepRun:
		if s.Quiet {
			return h.RunQuiet(s.Args)
		}
		return h.Run(s.Args)
	case StepRegisterService:
		if s.Service == nil {
			return fmt.Errorf("the service definition is missing")
		}
		return h.RegisterService(*s.Service)
	case StepStartService:
		return h.StartService(s.Target)
	case StepStopService:
		return h.StopService(s.Target)
	case StepDeleteService:
		return h.DeleteService(s.Target)
	case StepAddGroupMember:
		return h.AddGroupMember(s.Group, s.Member)
	case StepRemoveGroupMember:
		return h.RemoveGroupMember(s.Group, s.Member)
	case StepGrant:
		return h.Grant(s.Target, s.Access)
	}
	return fmt.Errorf("unknown step %q", s.Kind)
}
