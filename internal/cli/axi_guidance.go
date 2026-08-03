package cli

// staleMonitorGuidance is the canonical, point-of-use guidance an agent reads
// when `axi run` returns `checks-passed`: what to do if that PR later falls
// behind the default branch or hits a merge conflict (commonly because another
// PR merged first). The live CI monitor keeps running after checks pass and
// auto-rebases onto the base, resolves the conflict, and re-pushes the branch
// itself, so the agent runs no command and never hand-rebases. `no-mistakes
// rerun` is only the recovery for a monitor that is no longer running.
//
// Ordering matters and is not obvious. A dead run typically pushed auto-fix or
// CI-rebase commits the clone never took, so the recovery needs a
// synchronization as well as a rerun - and the synchronization has to come
// first. Once `rerun` has created its pending run, that run is the newest one
// branchsync inspects; it carries no push binding, so the state is
// `pipeline_owned` and both `Refresh` and `Apply` refuse. The clone is then
// stranded behind the gate head, and a fresh `axi run` is rejected
// non-fast-forward at its trigger push. Syncing first also establishes exactly
// the equality the later reattach needs (gate head == local HEAD).
// Proven end to end by e2e TestAxiStaleMonitorSyncBeforeRerunReattaches and,
// for the failure of the reverse order, TestAxiStaleMonitorRerunBeforeSyncStrandsTheRecovery.
//
// This same guidance is mirrored in the skill body (internal/skill/skill.go)
// and the published agents guide (docs/.../guides/agents.md); the repo treats
// agent-driving guidance as a multi-surface contract, and
// TestStaleMonitorGuidance_SyncedAcrossSurfaces keeps the three in sync.
const staleMonitorGuidance = "If this PR later falls behind the default branch or hits a merge conflict, the CI monitor rebases onto the base, resolves it, and re-pushes the branch automatically - run no command and never hand-rebase. Only when that monitor is no longer running (PR closed, run aborted, idle-timeout, or auto-fix exhausted) recover with `no-mistakes rerun`. If the dead run left auto-fix or CI-rebase commits your clone lacks, take them with the offered `branch_sync` `sync` action before the rerun, not after: the rerun's own pending run carries no push binding, so it owns the branch (`pipeline_owned`) and `no-mistakes axi sync` then refuses. `no-mistakes rerun` re-validates the head already pushed to the gate, so it is only for an unchanged local HEAD; after a local fix commit, start a fresh run with `no-mistakes axi run` instead. `no-mistakes rerun` returns immediately without driving, so something still has to answer the recovered run's gates: follow it with `no-mistakes axi run`, which reattaches and drives that run only while the gate head still equals your local HEAD, which is exactly what syncing first establishes. Then keep answering gates until an outcome."

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
const branchSyncAgentGuidance = "Before a post-pipeline local commit or fresh run, follow the structured `branch_sync.next_action`. Run `no-mistakes axi sync` only when its code is `sync`; that guarded sync may be a strict fast-forward or a content-equivalent diverged advance that anchors the pre-sync head before moving the branch with reset semantics. Run `no-mistakes axi sync --recover` only when its code is `recover_custody` (a terminal run left unpublished pipeline commits preserved in the local gate). A `user_owned` state means cancellation released the branch before changing the submitted head: the exact branch and head are yours, immediately usable, and no sync action is needed. Process blocked or pipeline-owned states instead of improvising reset, stash, merge, rebase, force, or branch replacement."
