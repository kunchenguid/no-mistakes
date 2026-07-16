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
const preserveGateFixCommitsGuidance = "Commit post-pipeline follow-up work on top of the existing branch so every pipeline fix commit remains present. Never abort-and-restart, reset, or replace the branch in a way that drops prior gate-fix commits."

// branchSyncAgentGuidance is emitted only when a relevant branch_sync object
// is present. Keeping it conditional avoids flooding ordinary runs whose local
// and pipeline heads never differed.
const branchSyncAgentGuidance = "Before a post-pipeline local commit or fresh run, follow the structured `branch_sync.next_action`. Run `no-mistakes axi sync` only when its code is `sync`, and `no-mistakes axi sync --recover` only when its code is `recover_custody` (a terminal run left unpublished pipeline commits preserved in the local gate); process blocked or pipeline-owned states instead of improvising reset, stash, merge, rebase, force, or branch replacement."
