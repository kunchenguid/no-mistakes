package agent

import (
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/procguard"
)

// GateRoleEnvVar is exported into every spawned gate agent's environment as an
// unspoofable-from-outside marker that the process is a no-mistakes gate agent
// (a review/fix/document/test/lint/rebase/pr/ci invocation), NOT a fleet
// operator. Its purpose is containment: when the target repository is itself an
// agent-orchestration harness (for example firstmate), the target's project
// agent-instruction file can otherwise convince the gate agent it is the fleet
// captain and drive it to spawn a crew and reset the shared branch it is
// validating (see the ambient-authority incident). A cooperating harness reads
// this marker and its fleet-lifecycle entrypoints fail closed. It is deliberately
// coarse (`=1`): presence is the whole signal.
const GateRoleEnvVar = "NO_MISTAKES_GATE"

// gitSafeEnv returns the environment for a spawned agent subprocess with git
// forced into non-interactive mode. Agents shell out to git directly (for
// example `git rebase --continue` during conflict resolution), which would
// otherwise open $EDITOR and hang in the headless subprocess until the agent
// times out.
//
// It also stamps GateRoleEnvVar so a cooperating orchestration harness in the
// target repo can recognize the gate agent and refuse to let it act as a fleet
// operator. Appended last so it wins over any ambient value.
//
// It also prepends the procguard shim directory to PATH so the agent's
// kill/pkill/killall resolve to no-mistakes' process-scope guard ahead of the
// real tools. This is the single choke point every native agent launch shares
// (claude/codex/copilot/pi/acpx/opencode all build their env here), so the
// interposition covers every supported native launch path. See internal/procguard.
//
// dir must be the value assigned to cmd.Dir so PWD stays coupled to the working
// directory; see git.NonInteractiveEnv for why this matters.
func gitSafeEnv(dir string) []string {
	env := append(git.NonInteractiveEnv(dir), GateRoleEnvVar+"=1")
	if binDir, err := procguard.DefaultBinDir(); err == nil {
		env = procguard.AugmentPATH(env, binDir)
	}
	return env
}
