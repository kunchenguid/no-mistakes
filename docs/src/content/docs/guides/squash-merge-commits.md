---
title: Readable Squash-Merge Commits
description: Make the squash commit on your default branch carry the PR description instead of a list of pipeline fix commits.
---

When GitHub squash-merges a pull request with its default settings, the commit on the default branch is the PR title plus the message of every commit on the branch. On a no-mistakes branch most of those commits are pipeline fixes, so the commit lists the steps that ran and not what the change added:

```text
fix(shellenv): scope macOS native completion inspection to its process group (#1753)

* fix(shellenv): scope macOS native completion inspection

Select the invocation process group before reading command details and ...

* no-mistakes(document): Clarify native completion inspection

* no-mistakes(ci): Restore independent snapshot reader timeout coverage
```

The PR description already holds the summary a reader wants: the PR step drafts `## What Changed` from the final diff. This guide makes that description the commit message. It takes two settings: one on the forge and one in no-mistakes.

## 1. Set the squash commit format on the forge

no-mistakes does not merge pull requests, so it does not write the squash commit. The repository's merge settings do. A repository admin sets them once.

In the GitHub UI, open **Settings -> General -> Pull Requests**, keep **Allow squash merging** on, and set the default commit message to **Pull request title and description**.

Or use the API:

```sh
gh api -X PATCH repos/<owner>/<repo> \
  -f squash_merge_commit_title=PR_TITLE \
  -f squash_merge_commit_message=PR_BODY
```

GitHub requires `squash_merge_commit_title` whenever `squash_merge_commit_message` is set. Two things override this setting:

- A merge queue writes its own commit message.
- A client that passes its own message at merge time, such as `gh pr merge --subject/--body` or the REST merge endpoint's `commit_title`/`commit_message`, uses that text instead.

## 2. Trim the PR body for history

With the setting above, the whole PR body becomes the commit body. The default body includes the full `## Intent` section and the Risk, Testing, and Pipeline sections with their evidence. Those sections help a reviewer, but they add noise to `git log`. Trim them in the repository's `.no-mistakes.yaml` on the default branch:

```yaml
pr:
  appendix: minimal       # one risk line, the signature, and the attestation
  publish_intent: false   # or keep a short Intent: intent: { format: brief }
```

- [`pr.appendix: minimal`](/no-mistakes/reference/repo-config/#prappendix) replaces the Risk, Testing, and Pipeline sections with a single risk line.
- [`pr.publish_intent: false`](/no-mistakes/reference/repo-config/#prpublish_intent) leaves the Intent section out of the body. Review still receives the full intent.
- To keep a short Intent instead, use [`pr.intent.format: brief`](/no-mistakes/reference/repo-config/#printent). It renders Why, Scope, and Out of scope bullets.

These fields are trusted-default-branch only, so the change takes effect after it lands on the default branch.

`pr.appendix` also has a machine-local default (see [appendix precedence](/no-mistakes/reference/global-config/#prappendix)), but a non-empty repository value on the trusted default branch outranks it, so set it there for this recipe to apply to every contributor's runs.

## What stays in the commit

The body keeps the pipeline signature and the `<!-- no-mistakes-pipeline-attestation:v1 ... -->` HTML comment. The [`require-no-mistakes`](/no-mistakes/reference/pipeline-steps/#pipeline-step-attestation) check binds to that marker in the PR body, so no setting removes it. GitHub hides the comment in the rendered PR, but `git log` shows it.

With both settings, the squash commit is the PR title, then `## What Changed`, the one-line risk assessment, the signature, and the attestation comment.
