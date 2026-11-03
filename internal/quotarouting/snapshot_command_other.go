//go:build !windows

package quotarouting

import (
	"context"
	"os/exec"
)

func snapshotCommand(ctx context.Context, exe string) *exec.Cmd {
	return exec.CommandContext(ctx, exe, "--full", "--json")
}
