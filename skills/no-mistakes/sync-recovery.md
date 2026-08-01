# Branch synchronization and custody recovery

```sh
no-mistakes axi sync --check    # freshly verify an offered synchronization plan
no-mistakes axi sync            # apply only an offered guarded synchronization
no-mistakes axi sync --recover  # return custody after a terminal run left unpublished pipeline commits
```

Every `next_action` carries both a `code` and the exact `command` to run, and an object with no `next_action` needs nothing from you (the branch is already synchronized, or the state is purely informational). Run the reported command for whatever code you get, including any code not listed below; the notes here only add what the command alone does not tell you:

- `sync` - the guarded sync may be a strict fast-forward or a content-equivalent diverged advance that anchors the pre-sync head before moving the branch with reset semantics; genuine divergence stays blocked.
- `check_sync` and `retry` - the reported state is not trustworthy yet (the pipeline-pushed commit is missing locally, or the push target could not be refreshed). Run the reported check and re-read the result before deciding anything.
- `run_pipeline` - your local head is ahead of the pipeline-pushed head, so the way forward is a new run, not a synchronization.
- `continue_active_run` - the pipeline still owns the branch: keep driving the active run rather than making local follow-up commits.
- `recover_custody` - a terminal run left unpublished pipeline commits preserved in the local gate. Recover custody first with `no-mistakes axi sync --recover`: it returns custody and moves a clean worktree to the preserved pipeline head, by fast-forward or by adopting a diverged preserved head proven to carry every local change - the ordinary result of the pipeline rebasing your commits onto a newer base - after anchoring your pre-recovery head under `refs/no-mistakes/recover-local/<run>`. That proof is deliberately narrow, so a rebase whose fix rounds also rewrote your own lines refuses instead of being adopted: when nothing can tell a deliberate pipeline fix from a dropped change, the decision is yours. Then validate that head with `no-mistakes axi run --intent "..."`, which starts and drives the run in one command. `no-mistakes rerun` also re-runs the preserved pipeline head, but it returns immediately without driving, and a following `no-mistakes axi run` reattaches only while your local HEAD equals that preserved head - so use it only after the recovery moved your worktree there. A dirty worktree, or divergence that cannot be proven contained, makes the recovery refuse with explicit choices; `--keep-local` keeps your current head while the preserved commits stay anchored under `refs/no-mistakes/recover/<run>`.
- `inspect_worktree` and `inspect_and_reconcile_manually` - the operation refused and changed nothing. The reported command only shows you the situation; it does not resolve it.

A `branch_sync.state` of `user_owned` means the run went terminal before changing the submitted head and cancellation released the branch: the exact branch and head are yours and immediately usable for whichever delivery path is authorized - no sync action is needed, and a repeated `--recover` there is a harmless no-op.

When synchronization is blocked, relay that structured state and its offered choices to the user and act only on them, instead of improvising reset, stash, merge, rebase, force, or branch replacement.

After synchronization, commit the post-pipeline follow-up work on top of the existing branch so every pipeline fix commit remains present, then re-run `no-mistakes axi run --intent "..."` with the original user intent.
