---
title: Update a published PR
description: Integrate a moved base through no-mistakes while preserving the published branch history.
---

When you update an open PR and its base branch has moved, the default Rebase strategy can replay commits that are already published. To preserve the published history, a repository maintainer can put this setting in `.no-mistakes.yaml` on the trusted default branch:

```yaml
rebase:
  strategy: merge
```

On the next run, the Rebase step can integrate a diverged base with a merge commit. The published branch's commits remain ancestors of the new head, so no-mistakes can push that update as a fast-forward.

Run the full pipeline for the updated head. If your repository pins a version of `require-no-mistakes` that requires Rebase, the action checks the Rebase status in the attestation. Equal heads, branches already ahead of the base, and successful fast-forwards can all produce `rebase: completed`. Passing `--skip=rebase` records a skip and fails the check.

See [`rebase.strategy`](/no-mistakes/reference/repo-config/#rebasestrategy) for the setting's precedence and the separate CI merge-conflict repair behavior.
