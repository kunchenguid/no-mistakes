package branchsync

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMain(m *testing.M) {
	configDir, err := os.MkdirTemp("", "branchsync-git-config-")
	if err != nil {
		panic(err)
	}
	_ = os.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(configDir, "gitconfig"))
	_ = os.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	_ = os.Setenv("GIT_AI_SKIP_ALL_HOOKS", "1")
	_ = os.Unsetenv("GIT_CONFIG_COUNT")
	code := m.Run()
	_ = os.RemoveAll(configDir)
	os.Exit(code)
}
