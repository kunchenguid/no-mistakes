# Force-push safety evidence

Scenario: the gate last observed feature at H1, a reviewer then pushed approved.txt directly to origin/feature, and the gated worktree rewrote feature without that file.

last observed feature head: 2a2265df752e
origin-only approved commit: 0e1c558a43c0
rewritten gated head: 24c6ea165e56
rewritten gated head contains approved.txt: false
origin/feature before guarded push contains approved.txt: true

Step logs:
- fetching latest upstream state...
- force push detected, skipping origin/feature sync
- already ahead of origin/main
- pushing to /var/folders/5x/4nqprlbx0518k3ybcb1sz6gr0000gn/T/nm-forcepush-evidence-432353561/origin.git (refs/heads/feature)...

Push result:
- refused: true
- error: push to upstream: refusing to force-push refs/heads/feature: remote head 0e1c558a43c0 carries 1 commit(s) the pipeline never incorporated (e.g. 0e1c558a43c0); pushing would discard upstream work. Re-fetch and rebase onto the current remote, or push manually if this overwrite is intended.

Remote after push attempt:
- origin/feature SHA: 0e1c558a43c0
- origin/feature still equals approved commit: true
- origin/feature contains approved.txt: true
