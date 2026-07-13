package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// WorktreeSteering is prepended to every pipeline agent prompt. It keeps the
// agent's writes inside the git worktree and steers it away from mutating
// system state outside the workspace (installing/upgrading system packages,
// modifying apps in /Applications, changing global config). Those out-of-tree
// writes are what trigger macOS "App Management" / Privacy notifications and
// risk surprising side effects on the user's machine.
var WorktreeSteering = fmt.Sprintf(`Workspace boundary (important):
- Confine source, project, user-data, and system file changes to the current working directory, which is a git worktree. Do not intentionally create, modify, move, or delete those files anywhere outside it.
- Do not modify system state outside the worktree. In particular, do not install or upgrade system packages (for example brew install/upgrade, or other system package managers), do not modify applications under /Applications, and do not change global or user-level tool configuration.
- This is prompt steering, not true enforcement: treat the worktree boundary as a soft boundary you must follow.
- The only allowed out-of-worktree writes are test evidence files under %s when a testing prompt explicitly asks for them.
- Ephemeral temp/cache writes that are incidental side effects of running the project development toolchain are allowed outside the worktree for tests, linters, formatters, builds, and manual verification commands.
- You may read files outside the worktree and run read-only commands, but every other intentional write must stay inside the worktree.

`, filepath.Join(os.TempDir(), "no-mistakes-evidence"))

// steeredAgent wraps an Agent and prepends WorktreeSteering to each prompt.
type steeredAgent struct {
	Agent
}

// Run prepends the worktree steering preamble before delegating to the
// wrapped agent. All other RunOpts fields pass through unchanged.
func (s steeredAgent) Run(ctx context.Context, opts RunOpts) (*Result, error) {
	opts.Prompt = WorktreeSteering + opts.Prompt
	return s.Agent.Run(ctx, opts)
}

func (s steeredAgent) SupportsSessionResume() bool {
	return SupportsSessionResume(s.Agent)
}

func (s steeredAgent) SupportsSessionProvider(provider string) bool {
	return SupportsSessionProvider(s.Agent, provider)
}

func (s steeredAgent) ReportsAgentAttempts() bool {
	return ReportsAgentAttempts(s.Agent)
}

func (s steeredAgent) NeutralizesGateInstructions() bool {
	return NeutralizesGateInstructions(s.Agent)
}

// WithSteering wraps an agent so every invocation is steered to keep writes
// inside the worktree. Wrapping is idempotent: an already-steered agent is
// returned unchanged so the preamble is never added twice.
func WithSteering(a Agent) Agent {
	if a == nil {
		return nil
	}
	if _, ok := a.(steeredAgent); ok {
		return a
	}
	return steeredAgent{a}
}
