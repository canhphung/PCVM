//go:build windows

package pcvm

import (
	"syscall"
	"unsafe"
)

var getDiskFreeSpaceExW = syscall.NewLazyDLL("kernel32.dll").NewProc("GetDiskFreeSpaceExW")

func platformDiskAvailable(path string) (int64, error) {
	pointer, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	var available uint64
	result, _, callErr := getDiskFreeSpaceExW.Call(uintptr(unsafe.Pointer(pointer)), uintptr(unsafe.Pointer(&available)), 0, 0)
	if result == 0 {
		return 0, callErr
	}
	if available > uint64(^uint64(0)>>1) {
		return int64(^uint64(0) >> 1), nil
	}
	return int64(available), nil
}
