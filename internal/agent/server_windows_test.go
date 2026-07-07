//go:build windows

package agent

import (
	"os/exec"
	"testing"
)

func TestConfigureManagedServerCmdHidesWindows(t *testing.T) {
	cmd := exec.Command("cmd", "/c", "exit", "0")

	configureManagedServerCmd(cmd)

	if cmd.SysProcAttr == nil {
		t.Fatal("configureManagedServerCmd did not assign SysProcAttr")
	}
	if !cmd.SysProcAttr.HideWindow {
		t.Fatal("HideWindow = false, want true")
	}
	if cmd.SysProcAttr.CreationFlags&managedServerCreateNoWindow == 0 {
		t.Fatalf("CreationFlags = %#x, want CREATE_NO_WINDOW set", cmd.SysProcAttr.CreationFlags)
	}
}
