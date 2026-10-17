package branchsync

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMain(m *testing.M) {
	// Fixtures must not depend on the machine's system git config: some
	// distributions ship safe.bareRepository=explicit there, which refuses
	// the in-directory use of the bare fixture repositories these tests
	// create. Mirrors internal/git's TestMain.
	os.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	// Fixtures must not inherit the developer's global git config: a
	// commit.gpgsign=true there makes every fixture commit ask gpg to sign.
	globalConfigDir, err := os.MkdirTemp("", "no-mistakes-branchsync-tests-")
	if err != nil {
		panic(err)
	}
	globalConfig := filepath.Join(globalConfigDir, "gitconfig")
	// Every fixture repository reads its identity from here: three per-repo
	// `git config` spawns per repository were ~8% of the package's git.exe
	// spawns, which set its Windows wall time.
	identity := "[core]\n\tautocrlf = false\n[user]\n\temail = test@example.com\n\tname = Test User\n"
	if err := os.WriteFile(globalConfig, []byte(identity), 0o644); err != nil {
		panic(err)
	}
	os.Setenv("GIT_CONFIG_GLOBAL", globalConfig)
	gitFixtureRoot = filepath.Join(globalConfigDir, "fixtures")
	code := m.Run()
	os.RemoveAll(globalConfigDir)
	os.Exit(code)
}
