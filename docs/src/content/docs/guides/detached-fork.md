---
title: Detached fork
description: How this no-mistakes checkout relates to upstream.
---

This repository is a **detached fork** of upstream `kunchenguid/no-mistakes`.
Product work (including the [publish firewall](/no-mistakes/guides/publish-firewall/))
lives here. Upstream commits are pulled. Pull requests are never opened against
upstream.

## Remotes

| Remote | URL | Role |
| --- | --- | --- |
| `origin` | `git@github.com:mfreeman451/no-mistakes.git` | This fork. Push branches here. Open pull requests against this repository's default branch. |
| `upstream` | `git@github.com:kunchenguid/no-mistakes.git` | Upstream. Fetch only. Do not push. Do not open pull requests against it. |

```sh
git remote add origin git@github.com:mfreeman451/no-mistakes.git
git remote add upstream git@github.com:kunchenguid/no-mistakes.git
```

## Pull requests

Open PRs with `gh-axi` (or `gh`) against **origin**, never against
`kunchenguid/no-mistakes`.

```sh
gh-axi pr create --repo mfreeman451/no-mistakes --base main --head <branch>
```

## Sync from upstream

```sh
git fetch upstream
git checkout main
git merge upstream/main
git push origin main
```

Do that on the fork default branch as an operator task, not by opening an
upstream pull request. Feature branches start from `origin/main`.

The pipeline daemon on an operator Mac (`~/.no-mistakes`) is unrelated to
this remotes layout and is not moved when the firewall runs in cluster.
