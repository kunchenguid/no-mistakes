package eval

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMain(m *testing.M) {
	os.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	globalConfigDir, err := os.MkdirTemp("", "no-mistakes-eval-tests-")
	if err != nil {
		panic(err)
	}
	os.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(globalConfigDir, "gitconfig"))
	code := m.Run()
	os.RemoveAll(globalConfigDir)
	os.Exit(code)
}
