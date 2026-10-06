package citest

import (
	"fmt"
	"os"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps/internal/stepstest"
)

func TestMain(m *testing.M) {
	os.Unsetenv("GIT_CONFIG_COUNT")
	// Fixtures must not depend on the machine's system git config: some
	// distributions ship safe.bareRepository=explicit there, which refuses
	// the in-directory use of the bare fixture repositories these tests
	// create. Mirrors internal/git's TestMain.
	os.Setenv("GIT_CONFIG_NOSYSTEM", "1")
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
