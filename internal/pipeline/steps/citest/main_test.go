package citest

import (
	"fmt"
	"os"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps/internal/stepstest"
)

func TestMain(m *testing.M) {
	os.Unsetenv("GIT_CONFIG_COUNT")
	if err := stepstest.Init(); err != nil {
		fmt.Fprintf(os.Stderr, "init fake CLI helper: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}
