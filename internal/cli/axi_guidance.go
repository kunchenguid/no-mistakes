package cli

// staleMonitorGuidance is the canonical, point-of-use guidance an agent reads
// when `axi run` returns `checks-passed`: what to do if that PR later falls
// behind the default branch or hits a merge conflict (commonly because another
// PR merged first). The live CI monitor keeps running after checks pass and
// auto-rebases onto the base, resolves the conflict, and re-pushes the branch
// itself, so the agent runs no command and never hand-rebases. `no-mistakes
// rerun` is only the recovery for a monitor that is no longer running.
//
// This same guidance is mirrored in the skill body (internal/skill/skill.go)
// and the published agents guide (docs/.../guides/agents.md); the repo treats
// agent-driving guidance as a multi-surface contract, and
// TestStaleMonitorGuidance_SyncedAcrossSurfaces keeps the three in sync.
const staleMonitorGuidance = "If this PR later falls behind the default branch or hits a merge conflict, the CI monitor rebases onto the base, resolves it, and re-pushes the branch automatically - run no command and never hand-rebase. Only when that monitor is no longer running (PR closed, run aborted, idle-timeout, or auto-fix exhausted) recover with `no-mistakes rerun`."

// preserveGateFixCommitsGuidance is the canonical, point-of-use guidance an
// agent reads when it needs to make another fix after a gate round already
// produced fix commits: keep those commits on the same branch and start a fresh
// validation run, instead of aborting, resetting, or switching branches in a way
// that drops prior pipeline work. This same guidance is mirrored in the skill
// body and the published agents guide, with CLI-reference coverage in
// docs/.../reference/cli.md.
const preserveGateFixCommitsGuidance = "When you make an additional fix after a gate round has already produced fix commits, commit it on top of the existing branch and run `no-mistakes axi run --intent \"...\"` with the original user intent. Never abort-and-restart, reset the branch, or open a new branch in a way that drops prior gate-fix commits. A fresh run re-validates the branch's current state, so already-resolved findings do not re-surface."
