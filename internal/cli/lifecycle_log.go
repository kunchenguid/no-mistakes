package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/paths"
)

func logLifecycleInvocation(command string, force bool) {
	p, err := paths.New()
	if err != nil {
		return
	}
	_ = p.EnsureDirs()

	line := fmt.Sprintf(
		"%s lifecycle command=%s force=%t pid=%d ppid=%d parent_cmdline=%q\n",
		time.Now().Format(time.RFC3339),
		command,
		force,
		os.Getpid(),
		os.Getppid(),
		parentCommandLine(os.Getppid()),
	)
	if force {
		line = strings.Replace(line, "lifecycle ", "lifecycle FORCE ", 1)
	}

	f, err := os.OpenFile(p.CLILog(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(line)
}

func parentCommandLine(ppid int) string {
	if ppid <= 0 {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-o", "command=", "-p", fmt.Sprint(ppid)).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
