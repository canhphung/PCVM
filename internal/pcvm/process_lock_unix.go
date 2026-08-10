//go:build !windows

package pcvm

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

type ProcessLock struct{ file *os.File }

func AcquireProcessLock(control string) (*ProcessLock, error) {
	file, err := os.OpenFile(filepath.Join(control, "lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		return nil, fmt.Errorf("PCVM-E4001 OPERATION_BUSY: another PCVM launcher owns .pcvm/lock")
	}
	return &ProcessLock{file: file}, nil
}

func (l *ProcessLock) Release() error {
	if l == nil || l.file == nil {
		return nil
	}
	err := syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	closeErr := l.file.Close()
	if err != nil {
		return err
	}
	return closeErr
}
