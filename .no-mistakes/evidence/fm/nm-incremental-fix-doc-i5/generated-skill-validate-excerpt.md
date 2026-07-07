
The same applies to any additional fix that comes after a gate round has
already produced fix commits - a newly surfaced finding, a reviewer's
pre-merge request, or any other post-completion change: commit it on top of
the existing branch and re-run `no-mistakes axi run --intent "..."` with the original user intent.
Never abort-and-restart, reset the branch, or open a new branch in a way that drops the prior gate-fix commits (including the pipeline's own
`no-mistakes(review|document|lint): ...` commits) - a re-run only
re-validates the branch's current state, so those commits stay on the branch
and already-resolved findings do not re-surface.

