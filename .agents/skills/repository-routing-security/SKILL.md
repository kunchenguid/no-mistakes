---
name: repository-routing-security
description: Use when changing fork routing, forge-profile identity, repository URL persistence, or credential redaction.
user-invocable: false
metadata:
  internal: true
---

**Fork Routing**

- `repos.upstream_url` is the parent repository used for PR base routing; `repos.fork_url` is an optional GitHub fork push target.
- `no-mistakes init --fork-url <url>` expects `origin` to point at the GitHub parent repository and `<url>` at the contributor fork; plain `no-mistakes init` preserves an existing fork URL on idempotent refresh.
- Push code must resolve the push URL via `resolvePushURL` (`internal/pipeline/steps/common_git.go`) so configured forks still receive branch updates, including after a CI repair restarts validation; the non-fork path recovers the credentialled upstream from the worktree's `origin` remote at run time because the DB `upstream_url` is stored redacted (see Credential Redaction below). `Repo.PushURL()` remains correct only for fork-only callers (e.g. `rebase.go`), since fork URLs carry no embedded credentials.
- GitHub PR code must keep `--repo` pointed at the parent and use `--head <fork_owner>:<branch>` when `fork_url` is set; existing-PR lookup must list by the bare branch and filter head-owner fields, never pass `<owner>:<branch>` to `gh pr list --head`. After provider availability succeeds and before any PR operation, `PRStep` requires durable proof that the run started through `axi run --intent` plus an authoritative intent that starts with an unindented `PR destination: owner/repo` line matching that parent and whose persistence proves its leading structure was preserved; direct push options, standalone reruns, incidental repository mentions, and missing, malformed, conflicting, fork, or migrated unproven destinations never authorize publication.
- Non-GitHub fork MR/PR routing is intentionally out of scope until implemented end to end; if a legacy row has `fork_url` for another provider, PR creation must skip instead of opening a self PR.
- Every new run best-effort refreshes registered upstream/fork URLs from the working clone through `gate.RefreshRepoURLs`: origin is the upstream authority, an existing fork requires one uniquely matching clone remote, both DB fields replace atomically, and every discovery/validation/write failure logs only a bounded reason and continues with the exact old registration. The refresh never rewrites clone or gate remotes; `Repo.URLsVerified` is run-scoped evidence that trusted fetch/push may use the refreshed DB URL instead of an inherited stale gate origin.

**Repository Forge Identity (`internal/forgecontext`)**

- Optional global `forge_profiles` map raw remote host tokens/SSH aliases to one isolated `gh` or `glab` config directory, plus an optional `expected_login` pin. The resolver owns profile selection, validation, parent/fork ambiguity, provider-specific fail-closed activation, and the immutable run environment; do not add ambient account switching or per-step routing. Profile identity for the parent/fork same-profile check is the config directory AND the pin, so conflicting pins fail as ambiguous instead of silently picking one account (`sameProfile`/`expectLogin` own the rationale).
- A resolved context must reach built-in provider commands, configured shell commands, native agents, managed agent servers, and recovered approval reconciliation. Never mutate the daemon environment or persist credentials/profile selection in the DB; recovery re-resolves from current global config.
- No configured profiles means exact legacy ambient behavior. Online auth failures keep provider steps' existing skip behavior; deterministic config/routing errors fail before the pipeline. The public contract lives in `docs/src/content/docs/reference/global-config.md`.

**Credential Redaction in Stored URLs and Errors (security)**

- `gate.InitWithFork` runs the upstream URL through `safeurl.Redact` before every DB persist (`UpdateRepoMetadata*`, `InsertRepoWithIDAndFork`) and the "gate initialized" log line; the bare gate's `origin` remote still carries the full credentialled URL (via `provisionGate`) so carved worktrees authenticate. Because the DB copy is redacted, push and branch-sync code must recover the credential from the worktree's `origin` remote at run time (`resolvePushURL`/`resolveUpstreamURL`), never from `Repo.UpstreamURL`/`Repo.PushURL()`.
- Step-failure errors (`executor.go` `FailStep`/log/IPC emit) and the Bitbucket resolve-repo error are redacted via `safeurl.RedactText`/`safeurl.Redact` so a credentialled URL wrapped into an error can never reach a step log or `runs.error`. Reuse `internal/safeurl` for new redaction sites rather than adding a git-local helper; it is already wired into `git.Run`/step git-run error formatting.
- Regressions: `TestInitRedactsCredentialURL`, `TestResolveUpstreamURL_PreservesCredential`, `TestResolveUpstreamURL_FallsBackToRecordedURL`, `TestResolvePushURL_ForkWinsOverCredential`.
