//go:build windows

package winproc

import (
	"os/exec"
	"syscall"
	"testing"

	"golang.org/x/sys/windows"
)

func TestHardenAllocatesSysProcAttrAndSetsFlags(t *testing.T) {
	cmd := exec.Command("cmd.exe", "/c", "echo hi")
	Harden(cmd)

	if cmd.SysProcAttr == nil {
		t.Fatal("Harden did not allocate SysProcAttr")
	}
	if cmd.SysProcAttr.CreationFlags&windows.CREATE_NO_WINDOW == 0 {
		t.Fatalf("CreationFlags = %#x, want CREATE_NO_WINDOW (%#x) set",
			cmd.SysProcAttr.CreationFlags, windows.CREATE_NO_WINDOW)
	}
	if !cmd.SysProcAttr.HideWindow {
		t.Fatal("HideWindow = false, want true")
	}
}

func TestHardenPreservesExistingFlags(t *testing.T) {
	const createNewProcessGroup = 0x00000200
	cmd := exec.Command("cmd.exe", "/c", "echo hi")
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNewProcessGroup}

	Harden(cmd)

	if cmd.SysProcAttr.CreationFlags&createNewProcessGroup == 0 {
		t.Fatalf("CreationFlags = %#x, want pre-existing flag %#x preserved",
			cmd.SysProcAttr.CreationFlags, createNewProcessGroup)
	}
	if cmd.SysProcAttr.CreationFlags&windows.CREATE_NO_WINDOW == 0 {
		t.Fatalf("CreationFlags = %#x, want CREATE_NO_WINDOW (%#x) set",
			cmd.SysProcAttr.CreationFlags, windows.CREATE_NO_WINDOW)
	}
}

func TestHardenNilCmdDoesNotPanic(t *testing.T) {
	Harden(nil)
}
