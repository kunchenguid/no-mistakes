Use `no-mistakes axi abort --run <id>` only when you need to cancel a specific active run by id from outside its worktree.

When an agent makes an additional fix after a gate round has already produced fix commits - a newly surfaced finding, a reviewer or pre-merge request, or any other post-completion change - it should commit the fix on top of the existing branch and run `no-mistakes axi run --intent "..."` with the original user intent.
Never abort-and-restart, reset the branch, or open a new branch in a way that drops prior gate-fix commits, including the pipeline's own `no-mistakes(review|document|lint): ...` commits.
A fresh run re-validates the branch's current state, so already-resolved findings do not re-surface.

When an agent starts a new run, `--intent` is required and should describe what the user wanted to accomplish, not what files changed.
