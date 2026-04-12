package main

import (
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/kunchenguid/no-mistakes/internal/cli"
	"github.com/kunchenguid/no-mistakes/internal/daemon"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/update"
)

func main() {
	if os.Getenv("NM_DAEMON") == "1" {
		if err := daemon.Run(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}

	if handled, err := update.MaybeHandleBackgroundCheck(os.Args[1:]); handled {
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}

	update.MaybeNotifyAndCheck(os.Args[1:], os.Stderr)

	// Redirect slog to a file for interactive CLI commands so logs never
	// leak into user-facing output. The daemon process sets up its own
	// file-based logger before reaching this point.
	slog.SetDefault(slog.New(slog.NewTextHandler(cliLogWriter(), nil)))

	cli.Execute()
}

// cliLogWriter returns a writer for CLI logs. Falls back to io.Discard
// if the log file cannot be opened (e.g. before first init).
func cliLogWriter() io.Writer {
	p, err := paths.New()
	if err != nil {
		return io.Discard
	}
	f, err := os.OpenFile(p.CLILog(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return io.Discard
	}
	return f
}
