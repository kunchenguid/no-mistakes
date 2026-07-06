//go:build windows

package update

import (
	"syscall"

	"golang.org/x/sys/windows"
)

// detachedProcAttr configures the background update process to run in its own
// process group without a visible console window. The CREATE_NO_WINDOW flag
// suppresses the console Windows would otherwise allocate for this console
// child spawned from the (console-less) daemon. See issue #287.
func detachedProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP | windows.CREATE_NO_WINDOW,
	}
}
