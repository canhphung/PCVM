//go:build windows

package pcvm

import (
	"fmt"
	"os"
	"path/filepath"
)

type ProcessLock struct {
	file *os.File
	path string
}

func AcquireProcessLock(control string) (*ProcessLock, error) {
	path := filepath.Join(control, "lock")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if os.IsExist(err) {
		return nil, fmt.Errorf("PCVM-E4001 OPERATION_BUSY: another PCVM launcher owns .pcvm/lock")
	}
	if err != nil {
		return nil, err
	}
	return &ProcessLock{file: file, path: path}, nil
}

func (l *ProcessLock) Release() error {
	if l == nil || l.file == nil {
		return nil
	}
	closeErr := l.file.Close()
	removeErr := os.Remove(l.path)
	if closeErr != nil {
		return closeErr
	}
	return removeErr
}
