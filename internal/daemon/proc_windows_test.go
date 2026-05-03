//go:build windows

package daemon

import (
	"os/exec"
	"testing"

	"golang.org/x/sys/windows"
)

func TestProcessRunningReturnsFalseForMissingPID(t *testing.T) {
	running, err := processRunning(999999)
	if err != nil {
		t.Fatalf("processRunning returned error: %v", err)
	}
	if running {
		t.Fatal("expected missing pid to be reported as not running")
	}
}

// TestSetSysProcAttrDetachesDaemonFromConsole guards the fix for issue #164:
// without DETACHED_PROCESS and CREATE_NEW_PROCESS_GROUP the daemon child
// inherits the installer's console, which causes PowerShell's
// `Start-Process -Wait -NoNewWindow` to hang waiting on the (long-lived)
// daemon instead of returning when the CLI exits.
func TestSetSysProcAttrDetachesDaemonFromConsole(t *testing.T) {
	cmd := exec.Command("does-not-need-to-exist.exe")
	setSysProcAttr(cmd)

	attr := cmd.SysProcAttr
	if attr == nil {
		t.Fatal("setSysProcAttr did not assign SysProcAttr")
	}
	wantFlags := uint32(windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP)
	if attr.CreationFlags&wantFlags != wantFlags {
		t.Fatalf("CreationFlags = %#x, want flags %#x set", attr.CreationFlags, wantFlags)
	}
	if !attr.HideWindow {
		t.Fatal("HideWindow = false, want true")
	}
}
