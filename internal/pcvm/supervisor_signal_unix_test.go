//go:build !windows

package pcvm

import (
	"errors"
	"os"
	"os/signal"
	"syscall"
)

func signalIgnoreInterruptAndTerminate() {
	signal.Ignore(os.Interrupt, syscall.SIGTERM)
}

func processExists(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || !errors.Is(err, syscall.ESRCH)
}
