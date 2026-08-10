//go:build !windows

package pcvm

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

func configureProcessGroup(command *exec.Cmd) {
	if command.SysProcAttr == nil {
		command.SysProcAttr = &syscall.SysProcAttr{}
	}
	command.SysProcAttr.Setpgid = true
}

func signalProcessGroup(command *exec.Cmd, signal os.Signal) error {
	if command == nil || command.Process == nil {
		return os.ErrProcessDone
	}
	unixSignal, ok := signal.(syscall.Signal)
	if !ok {
		return command.Process.Signal(signal)
	}
	err := syscall.Kill(-command.Process.Pid, unixSignal)
	if errors.Is(err, syscall.ESRCH) {
		return os.ErrProcessDone
	}
	return err
}

func killProcessGroup(command *exec.Cmd) error {
	return signalProcessGroup(command, syscall.SIGKILL)
}
