//go:build windows

package update

import (
	"fmt"
	"syscall"
	"unsafe"
)

const processQueryLimitedInformation = 0x1000

var queryFullProcessImageNameW = syscall.NewLazyDLL("kernel32.dll").NewProc("QueryFullProcessImageNameW")

func defaultWindowsExecutablePathForPID(pid int) (string, error) {
	handle, err := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(pid))
	if err != nil {
		return "", err
	}
	defer syscall.CloseHandle(handle)

	size := uint32(32768)
	buf := make([]uint16, size)
	r1, _, callErr := queryFullProcessImageNameW.Call(
		uintptr(handle),
		0,
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(&size)),
	)
	if r1 == 0 {
		if callErr != syscall.Errno(0) {
			return "", callErr
		}
		return "", fmt.Errorf("query process image path: unknown error")
	}

	return syscall.UTF16ToString(buf[:size]), nil
}
