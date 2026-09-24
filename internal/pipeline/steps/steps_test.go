package steps

import (
	"fmt"
	"os"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps/internal/stepstest"
	"github.com/kunchenguid/no-mistakes/internal/shellenv"
)

func TestMain(m *testing.M) {
	if handled, exitCode, err := shellenv.RunWindowsCooperativeCommandHelper(os.Args[1:]); handled {
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
		os.Exit(exitCode)
	}

	// Agent harnesses inject git config (e.g. safe.bareRepository=explicit)
	// via GIT_CONFIG_COUNT/KEY_n/VALUE_n; tests that need it re-set it with
	// t.Setenv (issue #362).
	os.Unsetenv("GIT_CONFIG_COUNT")
	cleanup, err := stepstest.Init()
	if err != nil {
		fmt.Fprintf(os.Stderr, "init fake CLI helper: %v\n", err)
		os.Exit(1)
	}
	code := m.Run()
	if err := cleanup(); err != nil {
		fmt.Fprintf(os.Stderr, "cleanup fake CLI helper: %v\n", err)
		code = 1
	}
	os.Exit(code)
}
