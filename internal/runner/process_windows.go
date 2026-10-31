//go:build windows

package runner

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// processTree is the handler's job object. The handler starts inside the
// job and its children join it, docs/handler-contract.md section 7.2.
type processTree struct {
	job windows.Handle
}

// configureProcess starts the handler suspended, so the job object is
// assigned before the handler can start any child, and in its own process
// group. An ordinary handler gets a restricted copy of the agent's token,
// in which the Administrators group is deny-only and every privilege is
// removed (docs/architecture.md section 8.4). A privileged handler keeps
// the service's full rights, so Windows shows no UAC prompt.
func configureProcess(cmd *exec.Cmd, privileged bool) (*processOptions, error) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.CREATE_SUSPENDED | windows.CREATE_NEW_PROCESS_GROUP,
	}
	if privileged {
		return nil, nil
	}
	token, err := restrictedToken()
	if err != nil {
		return nil, err
	}
	cmd.SysProcAttr.Token = syscall.Token(token)
	return &processOptions{token: token}, nil
}

// newProcessTree creates the job object. Closing the handle kills every
// process that is still in the job, so a handler cannot outlive the agent.
func newProcessTree() (*processTree, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, err
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info))); err != nil {
		_ = windows.CloseHandle(job)
		return nil, err
	}
	return &processTree{job: job}, nil
}

// attach assigns the suspended process to the job and resumes it.
func (t *processTree) attach(pid int) error {
	process, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE|windows.PROCESS_QUERY_LIMITED_INFORMATION,
		false, uint32(pid))
	if err != nil {
		return err
	}
	defer func() { _ = windows.CloseHandle(process) }()
	if err := windows.AssignProcessToJobObject(t.job, process); err != nil {
		return err
	}
	return resumeProcess(uint32(pid))
}

// resumeProcess resumes every thread of a process started with
// CREATE_SUSPENDED. The process has one suspended thread at this point,
// but the snapshot keeps the call correct for every Windows version.
func resumeProcess(pid uint32) error {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return err
	}
	defer func() { _ = windows.CloseHandle(snapshot) }()
	entry := windows.ThreadEntry32{Size: uint32(unsafe.Sizeof(windows.ThreadEntry32{}))}
	resumed := false
	for err := windows.Thread32First(snapshot, &entry); err == nil; err = windows.Thread32Next(snapshot, &entry) {
		if entry.OwnerProcessID != pid {
			continue
		}
		thread, err := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, entry.ThreadID)
		if err != nil {
			continue
		}
		if _, err := windows.ResumeThread(thread); err == nil {
			resumed = true
		}
		_ = windows.CloseHandle(thread)
	}
	if !resumed {
		return errors.New("the handler process has no suspended thread to resume")
	}
	return nil
}

// terminate requests a stop. Windows has no reliable signal for a process
// without a console, so the cancel file is the only stop request.
func (t *processTree) terminate() {}

// kill ends the job object, which kills the handler and its children.
func (t *processTree) kill() {
	if t == nil || t.job == 0 {
		return
	}
	_ = windows.TerminateJobObject(t.job, 1)
}

// close releases the job object. Its kill-on-close limit takes care of a
// handler that outlived its parent.
func (t *processTree) close() {
	if t == nil || t.job == 0 {
		return
	}
	_ = windows.CloseHandle(t.job)
	t.job = 0
}

// exitStatus returns the exit code. Windows has no signal that ends a
// process, so the signal is always empty.
func exitStatus(state *os.ProcessState) (int, string) {
	if state == nil {
		return -1, ""
	}
	return state.ExitCode(), ""
}
