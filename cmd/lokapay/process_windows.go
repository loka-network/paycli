//go:build windows

package main

import (
	"os"
	"os/exec"
	"syscall"
)

// setDetached configures cmd so the child outlives this lokapay process
// on Windows. CREATE_NEW_PROCESS_GROUP puts the child into its own
// process group (so console signals to lokapay aren't propagated), and
// DETACHED_PROCESS means it doesn't inherit a console.
//
// Constants are hard-coded to avoid an explicit dependency on
// golang.org/x/sys/windows.
const (
	createNewProcessGroup = 0x00000200
	detachedProcess       = 0x00000008
)

func setDetached(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: createNewProcessGroup | detachedProcess,
	}
}

// sendStopSignal asks the child to exit. Windows has no SIGTERM
// equivalent that lnd would honor through this API, so we fall back to
// terminating outright. Callers should still try the graceful
// `lncli stop` first before reaching for this.
func sendStopSignal(p *os.Process) error {
	return p.Kill()
}

// signalProcessAlive is best-effort on Windows: OpenProcess succeeds
// for processes we can query, and a kill(0)-equivalent isn't exposed,
// so we use the file-handle round-trip via os.FindProcess.
func signalProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// On Windows, os.FindProcess returns a handle without verifying
	// liveness. Sending an empty signal here would error with
	// "not supported". Best we can do without x/sys/windows is to try
	// p.Kill() with a no-op intent — but that would kill the process.
	// Instead, accept "we have a handle" as alive; the next status
	// command will catch a stale pid file when lncli getinfo fails.
	_ = p
	return true
}
