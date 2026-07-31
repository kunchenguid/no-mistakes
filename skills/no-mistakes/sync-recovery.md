# Branch synchronization and custody recovery

Before any post-pipeline local commit or fresh run, read the structured `branch_sync` object returned by AXI home, status, or a drive result, and act on its `next_action.code`.

```sh
no-mistakes axi sync --check    # freshly verify an offered synchronization plan
no-mistakes axi sync            # apply only an offered guarded synchronization
no-mistakes axi sync --recover  # return custody after a terminal run left unpublished pipeline commits
```

- `sync` - run `no-mistakes axi sync` first. That guarded sync may be a strict fast-forward or a content-equivalent diverged advance that anchors the pre-sync head before moving the branch with reset semantics; genuine divergence stays blocked.
- `continue_active_run` - the pipeline still owns the branch: run the reported command and keep driving the active run rather than making local follow-up commits.
- `recover_custody` - a terminal run left unpublished pipeline commits preserved in the local gate: run `no-mistakes axi sync --recover` to return custody and take the preserved head, or `no-mistakes rerun` to resume validating it instead. Recovery takes that head by fast-forward, or by adopting a diverged preserved head proven to carry every local change - the ordinary result of the pipeline rebasing your commits onto a newer base - after anchoring your pre-recovery head under `refs/no-mistakes/recover-local/<run>`. That proof is deliberately narrow, so a rebase whose fix rounds also rewrote your own lines refuses instead of being adopted: when nothing can tell a deliberate pipeline fix from a dropped change, the decision is yours. A dirty worktree, or divergence that cannot be proven contained, makes the recovery refuse with explicit choices; `--keep-local` keeps your current head while the preserved commits stay anchored under `refs/no-mistakes/recover/<run>`.

A `branch_sync.state` of `user_owned` means the run went terminal before changing the submitted head and cancellation released the branch: the exact branch and head are yours and immediately usable for whichever delivery path is authorized - no sync action is needed, and a repeated `--recover` there is a harmless no-op.

When synchronization is blocked, relay that structured state and its offered choices to the user and act only on them, instead of improvising reset, stash, merge, rebase, force, or branch replacement.

After synchronization, commit the post-pipeline follow-up work on top of the existing branch so every pipeline fix commit remains present, then re-run `no-mistakes axi run --intent "..."` with the original user intent.
