//go:build windows

package pcvm

import (
	"os"
	"os/exec"
)

func configureProcessGroup(*exec.Cmd) {}

func signalProcessGroup(command *exec.Cmd, signal os.Signal) error {
	if command == nil || command.Process == nil {
		return os.ErrProcessDone
	}
	return command.Process.Signal(signal)
}

func killProcessGroup(command *exec.Cmd) error {
	if command == nil || command.Process == nil {
		return os.ErrProcessDone
	}
	return command.Process.Kill()
}
