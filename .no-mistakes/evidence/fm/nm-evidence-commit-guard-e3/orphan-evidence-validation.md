# Orphan evidence publication validation

Focused behavioral tests exercised the production evidence publisher against real Git repositories and bare remotes. The observed PR-facing output was:

````markdown
## Testing

Evidence was collected.

- Evidence: [Checkout screenshot](https://github.com/example/widgets/blob/1d8c26381c74b134d5d67168492700c7ec9fdda9/.no-mistakes/evidence/feature/add-login/checkout.png)
<details>
<summary>Evidence: CLI run</summary>

Source: [CLI run](https://github.com/example/widgets/blob/1d8c26381c74b134d5d67168492700c7ec9fdda9/.no-mistakes/evidence/feature/add-login/cli-run.txt)

```text
it works
```
</details>
````

The link uses the published evidence commit SHA, not the mutable branch name. In the same end-to-end test, Git observations established that:

- `refs/heads/no-mistakes/evidence` contained the binary screenshot and CLI log.
- The evidence commit was an orphan root with no merge base with `main`.
- Neither `main` nor the feature branch contained evidence paths.
- The caller's HEAD, index, and worktree remained unchanged.

Additional real-remote cases exercised a configured `team/ci/evidence` branch, invalid-ref rejection without changing remote refs, refusal to append to unmarked or incorrectly marked existing branches, refusal when the evidence branch names a code branch, a remote push-permission denial, an unreadable remote, fast-forward appends preserving earlier commit-pinned content, and publication from a detached depth-1 clone.

No screenshot was captured because this change has no rendered UI. Its reviewer-visible surface is the generated PR Markdown above, and its persistent product state is the Git ref, commit graph, and tree verified by the behavioral tests.
