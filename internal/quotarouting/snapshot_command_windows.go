package quotarouting

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

func snapshotCommand(ctx context.Context, exe string) *exec.Cmd {
	direct := exec.CommandContext(ctx, exe, "--full", "--json")
	if direct.Err != nil {
		return direct
	}
	switch strings.ToLower(filepath.Ext(direct.Path)) {
	case ".cmd", ".bat":
		cmd := exec.CommandContext(ctx, "cmd.exe")
		cmd.Env = append(os.Environ(), "NM_QUOTA_AXI_EXECUTABLE="+direct.Path)
		cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: `cmd.exe /d /v:off /s /c ""%NM_QUOTA_AXI_EXECUTABLE%" --full --json"`}
		return cmd
	default:
		return direct
	}
}
