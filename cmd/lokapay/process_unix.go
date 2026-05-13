//go:build !windows

package main

import (
	"os"
	"os/exec"
	"syscall"
)

// setDetached configures cmd so the child outlives this lokapay process
// and doesn't inherit our controlling terminal. On unix that means
// Setsid (new session, no controlling tty) so SIGINT to the lokapay
// foreground doesn't kill the lnd we just started.
func setDetached(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

// sendStopSignal sends SIGTERM. On unix, lnd's signal handler treats
// SIGTERM as a graceful shutdown.
func sendStopSignal(p *os.Process) error {
	return p.Signal(syscall.SIGTERM)
}

// signalProcessAlive returns true when pid corresponds to a running
// process this user can signal. Uses signal 0 — a no-op send that
// only checks deliverability — which is the canonical way to probe
// liveness on unix without race conditions.
func signalProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}
