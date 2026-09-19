package cli

// staleMonitorGuidance is the canonical, point-of-use guidance an agent reads
// when `axi run` returns `checks-passed`: what to do if that PR later falls
// behind the default branch or hits a merge conflict (commonly because another
// PR merged first). The live CI monitor keeps running after checks pass and
// auto-rebases onto the base, resolves the conflict, revalidates from Review,
// and re-pushes itself, so the agent runs no command and never hand-rebases. `no-mistakes
// rerun` is only the recovery for a monitor that is no longer running, and a
// known clean caller head must match the head rerun already selects.
//
// The skill body (internal/skill/skill.go) owns the full driving guidance; the
// agents guide (docs/.../guides/agents.md) keeps the monitor invariant and links
// to the CLI reference for restart conditions.
// TestStaleMonitorGuidance_SyncedAcrossSurfaces guards that contract.
const staleMonitorGuidance = "If this PR later falls behind the default branch or hits a merge conflict, the CI monitor rebases onto the base, resolves it, revalidates from Review because rebasing cannot prove continuity with the reviewed head, and re-pushes it through Push automatically - run no command and never hand-rebase. Only when that monitor is no longer running (PR closed, run aborted, idle-timeout, or auto-fix exhausted) use `no-mistakes rerun` to validate the selected gate or preserved head; it refuses a known clean caller HEAD mismatch. If heads differ, inspect `no-mistakes axi status` and follow its exact `branch_sync.next_action.command` for custody or synchronization, then submit intended local commits with a fresh `no-mistakes axi run` once custody permits."

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
const branchSyncAgentGuidance = "Before a post-pipeline local commit or fresh run, follow the exact structured `branch_sync.next_action.command`. A `sync` action may be a strict fast-forward or a content-equivalent diverged advance that anchors the pre-sync head before moving the branch with reset semantics. A `recover_custody` action may be ordinary `no-mistakes axi sync --recover` when unpublished pipeline commits are preserved in the local gate, or exact `no-mistakes axi sync --recover --keep-local` when a bound archive preserves a divergent later head while custody returns at the reported required head, or when that preserved head is unavailable and you are explicitly discarding the missing commits; never substitute one recovery command for the other. A `user_owned` state means cancellation released the branch before changing the submitted head: the exact branch and head are yours, immediately usable, and no sync action is needed. For a known GitHub owner/repository rename, complete the offered synchronization while the old target remains configured. Refresh the canonical push target with `no-mistakes init`; for a fork, use `--fork-url` and keep `origin` at the parent. `no-mistakes axi sync --accept-repository-rename <previous-credential-free-target>` atomically migrates the selected terminal run's fingerprint and records exact rename provenance for a contained rerun lineage when authenticated same-host immutable repository identity and every head/ownership guard pass. Earlier fingerprints and all validation history remain unchanged; a failed run remains failed. Follow its exact `rerun_pipeline` action for new validation only after successful acceptance; a final state check can block after migration commits with `changed: true`. Full guards and refusals: https://kunchenguid.github.io/no-mistakes/reference/cli/#repository-rename-continuation. Process every other blocked or pipeline-owned state instead of improvising reset, stash, merge, rebase, force, or branch replacement."

// repositoryRenameSyncGuidance keeps the human and AXI sync help on the same
// explicit migration contract; the CLI reference owns the detailed proof.
const repositoryRenameSyncGuidance = "--accept-repository-rename <previous-credential-free-target> explicitly accepts\n" +
	"a known GitHub rename. Complete the offered synchronization while the old target\n" +
	"remains configured, then refresh the canonical push target with no-mistakes init;\n" +
	"for a fork, use --fork-url and keep origin at the parent (see the init reference).\n" +
	"Acceptance requires a terminal run, exact clean local/pushed/live heads, unchanged\n" +
	"target kind/ref, no active same-branch run, and authenticated same-host immutable\n" +
	"repository identity. It atomically migrates the selected terminal run's fingerprint\n" +
	"and records exact rename provenance for a contained rerun lineage. Earlier\n" +
	"fingerprints and all validation history remain unchanged; a failed run remains failed.\n" +
	"Successful acceptance returns rerun_pipeline (no-mistakes rerun) for new validation.\n" +
	"A final state check can block after migration commits with changed: true; honor\n" +
	"the reported state. Full guards and refusals:\n" +
	"https://kunchenguid.github.io/no-mistakes/reference/cli/#repository-rename-continuation"
