---
title: Managed Authorization
description: Opt-in external authorization for orchestrator-managed no-mistakes runs.
---

Managed authorization lets an external orchestrator decide whether one live task runtime may start or continue the no-mistakes pipeline.
It is opt-in and versioned.
Ordinary standalone use is unchanged when no managed environment is present, and unmanaged commands do not contact an authorization service.

## Protected boundaries

A managed run obtains a fresh decision at each expensive or irreversible boundary:

1. `no-mistakes axi run` authorizes `run` before a gate push or rerun request can create a run.
2. The gate receiver authorizes `gate-push` before `HandlePushReceived` inserts a run row.
3. Every concrete external-agent attempt authorizes `agent-launch` immediately before the subprocess starts.

Retries, fallback providers, and fresh or resumed agent sessions each authorize again.
Authorization decisions are never cached.

The daemon persists only a `managed_authorization` marker on the run.
It does not persist verifier URLs, credentials, task IDs, runtime generations, session IDs, project paths, repository identities, worktree paths, branches, request IDs, or decisions.
Managed runs cannot resume after a daemon restart because their in-memory capability is intentionally unrecoverable.
The replacement orchestrator runtime must start a new run with fresh live authorization.

## Protocol version 1

The canonical protocol version is `authorization.ProtocolVersion` in `internal/authorization`.
`no-mistakes --version` exposes both the build version and `authorization-protocol=1` so a distributor can reject an incompatible binary before activation.

Each request contains:

```json
{
  "protocolVersion": "1",
  "requestId": "one-use-random-identifier",
  "operation": "run",
  "taskId": "task-identifier",
  "runtimeGeneration": 7,
  "sessionId": "live-session-identifier",
  "projectPath": "/canonical/project",
  "repository": "github.com/owner/repository",
  "worktreePath": "/canonical/project-worktree",
  "branch": "feature/branch",
  "durableMode": "no-mistakes"
}
```

`operation` is `run`, `gate-push`, or `agent-launch`.
`requestId` is a one-use nonce generated for each verifier call.

The verifier response must echo every request field and add `allowed` plus a compact `reason`:

```json
{
  "protocolVersion": "1",
  "requestId": "one-use-random-identifier",
  "operation": "run",
  "taskId": "task-identifier",
  "runtimeGeneration": 7,
  "sessionId": "live-session-identifier",
  "projectPath": "/canonical/project",
  "repository": "github.com/owner/repository",
  "worktreePath": "/canonical/project-worktree",
  "branch": "feature/branch",
  "durableMode": "no-mistakes",
  "allowed": true,
  "reason": "authorized"
}
```

The client proceeds only after HTTP 200, `allowed: true`, durable mode `no-mistakes`, protocol version `1`, and an exact echo of the request scope.
Missing verifiers, timeouts, connection failures, non-200 responses, denials, malformed JSON, unsupported protocols, stale generations, replayed request IDs, and any scope mismatch fail closed.
Errors are compact and never include credentials or raw response bodies.

The verifier must atomically reject a previously used `requestId` within the live task/runtime generation.
It must also independently resolve and verify the durable task mode, current runtime generation, live session, canonical project and repository, worktree, branch, and requested operation.
Request claims are not authority.

## Configuration

The environment variables are documented in [Environment Variables](/no-mistakes/reference/environment/#managed-authorization).
The provider-neutral `NO_MISTAKES_AUTHORIZATION_*` form uses an HTTP bearer token.
The Perch adapter reuses its short-lived task hook token and sends it as `x-perch-token` with `x-perch-session`.

The presence of any recognized managed variable opts the process into managed mode.
An incomplete managed environment is a denial, not a fallback to standalone behavior.
The git hook and local daemon IPC carry the minimum transient capability needed for the receiver and agent-launch checks.
The hook does not put authorization data in git push options, git configuration, or the gate log.

Review-agent child environments remove every `PERCH_*` and `NO_MISTAKES_AUTHORIZATION_*` value.
Credentials are not included in prompts, SQLite, telemetry, snapshots, crash reports, or user-facing errors.

## Perch integration

Perch should expose the existing task and hook variables plus `PERCH_TASK_REPOSITORY` and `PERCH_RUNTIME_GENERATION` to the task worker.
`PERCH_TASK_REPOSITORY` must be the canonical credential-free repository identity, such as `github.com/owner/repository`.
`PERCH_RUNTIME_GENERATION` must be the durable generation of the live runtime that owns `PERCH_SESSION_ID`.

For protocol version 1, Perch's authorization endpoint must accept and validate `protocolVersion`, `requestId`, `operation`, `taskId`, `runtimeGeneration`, `sessionId`, `projectPath`, `repository`, `worktreePath`, `branch`, and `durableMode`.
It must echo all of those fields with `allowed` and `reason`.
It must reject reused request IDs, stale generations, non-live runtimes, session mismatches, repository or path aliases that do not canonicalize to the durable project, branch mismatches, and durable modes other than `no-mistakes`.

The companion Perch pull request must be amended before integration because its initial response omits `protocolVersion`, `requestId`, `operation`, `sessionId`, `projectPath`, `repository`, `worktreePath`, and `branch`, and its worker environment omits repository identity and runtime generation.
Those fields are required for exact response validation and replay rejection.

Perch `direct-PR` and `local-only` tasks must receive a denial before run creation, gate acceptance, and agent launch.
Only the exact live task whose durable mode is `no-mistakes` may receive an allow decision.

## Threat model

Managed authorization prevents prompt text, repository state, gate configuration, PATH selection, or a stale worker credential from granting pipeline access.
It binds each decision to a live task runtime and the exact project, repository, worktree, branch, session, operation, and protocol version.
It does not make a compromised verifier trustworthy, and it does not replace operating-system protection of the local daemon socket or orchestrator token.

The gate is inside the upstream CLI, receiver, daemon, and agent launch path.
A PATH wrapper is not sufficient because an absolute binary path could bypass it.

## Distribution and updates

A distributor should pin the first stable upstream release containing protocol version 1 by exact tag and commit.
It should verify the release asset against `checksums.txt`, record the GitHub release asset digest, and verify the macOS Developer ID signature, fixed Team ID, signing identifier, hardened runtime, secure timestamp, and architecture before repackaging.
Checksums without authenticated release metadata are not a signing mechanism.

The packaged manifest should record the upstream tag, commit, asset URL, SHA-256, release digest, signing identity, architecture, build version, and authorization protocol version.
Activation should compare that manifest with `no-mistakes --version` and refuse any mismatch.

`no-mistakes update` refuses self-update whenever managed context is present.
The orchestrator owns replacement of a pinned managed runtime.
