package agent

import "github.com/kunchenguid/no-mistakes/internal/git"

// gitSafeEnv returns the environment for a spawned agent subprocess with git
// forced into non-interactive mode. Agents shell out to git directly (for
// example `git rebase --continue` during conflict resolution), which would
// otherwise open $EDITOR and hang in the headless subprocess until the agent
// times out.
//
// dir must be the value assigned to cmd.Dir so PWD stays coupled to the working
// directory; see git.NonInteractiveEnv for why this matters.
func gitSafeEnv(dir string) []string {
	return git.NonInteractiveEnv(dir)
}
