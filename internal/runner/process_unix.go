//go:build unix

package runner

import (
	"os"
	"syscall"
)

// processTree is the handler's process group. The handler starts as its
// leader and its children join the group, docs/handler-contract.md section
// 7.2.
type processTree struct {
	pid int
}

// newProcessTree prepares the process group state. POSIX needs no handle
// before the process starts.
func newProcessTree() (*processTree, error) {
	return &processTree{}, nil
}

// attach records the handler's process group.
func (t *processTree) attach(pid int) error {
	t.pid = pid
	return nil
}

// terminate sends SIGTERM to the whole process group.
func (t *processTree) terminate() {
	if t == nil || t.pid <= 0 {
		return
	}
	_ = syscall.Kill(-t.pid, syscall.SIGTERM)
}

// kill sends SIGKILL to the whole process group.
func (t *processTree) kill() {
	if t == nil || t.pid <= 0 {
		return
	}
	_ = syscall.Kill(-t.pid, syscall.SIGKILL)
}

// close releases the process group handle. POSIX needs nothing.
func (t *processTree) close() {}

// exitStatus returns the exit code, or the signal that ended the process.
func exitStatus(state *os.ProcessState) (int, string) {
	if state == nil {
		return -1, ""
	}
	if status, ok := state.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		return -1, status.Signal().String()
	}
	return state.ExitCode(), ""
}
