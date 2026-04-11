package main

import (
	"fmt"
	"os"

	"github.com/kunchenguid/no-mistakes/internal/cli"
	"github.com/kunchenguid/no-mistakes/internal/daemon"
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

	cli.Execute()
}
