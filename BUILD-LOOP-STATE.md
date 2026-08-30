# BUILD-LOOP-STATE — Factory Publication v1 / Codex confinement

**NOW:** WRAP-UP/FREEZE · slice 1/1 N0-Confinement-Codex · iter 5/5 · last: mandatory immediate pre-push `go test -race ./... -count=1` and `make e2e` both exited 0 on current product/test bytes · next: stage/audit exact diff, Codex-authored freeze commit, create HeyHardy fork, push branch, open visibly blocked Draft PR · 2026-08-30 14:00 +0200

**Started:** 2026-08-30 · **Branch:** codex/factory-publication-v1 · **Worktree:** /Users/hardyheyde/Library/CloudStorage/Dropbox/Senox-Share/github_clones/codex-no-mistakes-n0
**Base commit:** ab2544298b745e9dc1a01fcf9a3151a247926083 · **Live checkout untouched:** yes
**Roles (resolved from environment rules):** Builder = Codex root · Reviewer = independent current-byte agent, then Senox exact-byte gate · Orchestrator = Codex root · Owner = Hardy

## Definition of Done (frozen on 2026-08-30, changes only with Owner go)
- [ ] DoD-1: production enables `factory-publication-v1` only for exact Codex on non-skipped `linux/arm64` or `linux/amd64` after a model-free capability probe; macOS and all other agents, fallbacks, sessions, platforms, builds, and unavailable primitives fail before admission/process effects.
- [ ] DoD-2: agent tools and configured Test/Lint commands share one enforced profile: candidate read and scratch/evidence write work, while candidate/source/sibling/home mutation or reads outside grants, mode/link/rename attacks, ambient credentials/proxies/askpass, TCP, and unallowlisted Unix sockets fail non-vacuously.
- [ ] DoD-3: clean exit and cancellation synchronously destroy the exact Linux PID namespace containing a detached `setsid` child after it changes CWD to `/` and closes inherited descriptors; bounded exact PID/start-identity re-enumeration proves no survivor before removal, uncertain teardown retains evidence, and an unrelated sentinel is untouched.
- [x] DoD-4: production composition remains `confinement_unavailable` unless the exact boundary passes; ordinary AXI behavior and the single Executor/state-machine contract remain unchanged.
- [x] DoD-5: focused tests, `make lint`, `go test -race ./...`, `make e2e`, `make docs`, build, and `git diff --check` are green on current product/test/doc bytes with no skip credited for required platform gates.
- [ ] DoD-6: Mac negative plus OrbStack/VPS Linux canaries bind one canonical in-process manifest (logical entry provenance plus directly executed native Codex, actual sandbox/bubblewrap helpers, private canary, and lifecycle sentinel, each realpath/raw SHA/mode/owner/file identity; native version; GOOS/GOARCH; policy digest; fixed probe/exec/command argv hashes), revalidated before each launch; no new dependency, package extraction, sudo, container runtime, host policy, live auth/provider mutation, Ruleset, CI-policy, deploy, or merge change occurs.
- [ ] DoD-7: independent current-byte review and Senox exact-byte review return APPROVE before commit/push/PR handoff.

## Gates
`Waiting since` is the ISO timestamp the gate was opened.

| Gate | Status | Waiting since | Reference |
|------|--------|---------------|-----------|
| Pre-Build Domain Audit (per non-standard slice) | ok:EXTEND | — | `sha256:a0f6abd9cbaefe791fdb895e2dbd831c397326f522e0bb44b28acbcb74c8867d`; `build-loop-state/pre-build-audit-n0-confinement.json` |
| Parallelization scan (2c) | ok | — | sequential; one auth boundary and overlapping execution path |
| Reviewer plan review | ok:APPROVE | — | iter-3 direct-native/full executable closure, Linux namespace lifecycle, and exact file boundary approved 2026-08-30T10:40:00+02:00 |
| Independent current-byte code review | ok:APPROVE | — | `build-loop-state/reviews/n0-confinement-n0-base-binding.json` (`sha256:52a0b050e1a94dbe928bbb1159e188cfb2fa059a7d5eb8034ac046e51f08355e`); `build-loop-state/reviews/n0-confinement-n0-current-byte-review.json` (`sha256:cd022212ca8dee2744fc0b61c361eb3419bd32b9a71f4e8dbd632b489989cd0c`); `build-loop-state/reviews/n0-confinement-n0-wiring-design.json` (`sha256:32d4dff387a8f3a0d14665f258703e8e8a69f074c41584414cb01b1a52e5de2f`); all bind the same five current code hashes and exclude platform credit |
| Owner scope go | ok | — | 2026-08-30: “Go mach das” selecting the last named `GO-N0-CONFINEMENT-CODEX` path |
| Cross-slice coherence (per non-standard slice with shared seam_id) | n.a. | — | first slice for seam; no predecessor contract |
| Push go (fresh, explicit) | ok | — | Owner: “Go für A”, 2026-08-30 after Senox recommended the blocked Draft-PR option |
| Reviewer PR gate | open | — | exact pushed head required |
| Understand-it gate (bundle + attestation) | n.a. | — | no separate understand-it artifact required by this repository slice |
| Merge (role: Owner unless the environment grants otherwise) | open | — | not authorized by this slice |
| Deploy go | open | — | not authorized by this slice |

## Slices
| # | Slice | DoD ref | seam_class | seam_id | Mode | Status | Iteration | Note |
|---|-------|---------|------------|---------|------|--------|-----------|------|
| 1 | N0-Confinement-Codex | DoD-1..7 | auth_boundary | factory-publication-defense-boundary | seq | platform-blocked | 5/5 | Portable implementation green; Linux runtime credit blocked by absent pre-provisioned Codex/bwrap environment |

## Parallelization verdict (2c — written before the plan gate, never skipped)
| Slice | Mode | Group | Why | Target measured before dispatch |
|-------|------|-------|-----|---------------------------------|
| 1 | sequential | — | profile, agent, command, composition, candidate disposal, and reaper all change one trust boundary; parallel edits would make falsifiers non-attributable | base `ab2544298b745e9dc1a01fcf9a3151a247926083`, 48-entry pre-slice porcelain baseline, 2026-08-30T09:55:00+02:00 |

## Pre-Build Audit records (per non-standard slice)
| Slice | verdict | seam_id | contract_id | audit_hash | runner | JSON path |
|-------|---------|---------|-------------|------------|--------|-----------|
| 1 | EXTEND | factory-publication-defense-boundary | — | sha256:a0f6abd9cbaefe791fdb895e2dbd831c397326f522e0bb44b28acbcb74c8867d | full_rotated | build-loop-state/pre-build-audit-n0-confinement.json |

## Log (append-only, write immediately)
- 2026-08-30T09:55:00+02:00 — Owner selected restricted `GO-N0-CONFINEMENT-CODEX`; ExecPlan extended before product code.
- 2026-08-30T09:55:00+02:00 — Agent-rules and full pre-build domain audits completed read-only; repository status matched the 48-entry pre-audit baseline.
- 2026-08-30T09:55:00+02:00 — Mac model-free Codex sandbox positives passed for candidate read and scratch write; candidate write/chmod, sibling read, loopback TCP, Unix socket, and nested Codex auth were denied.
- 2026-08-30T09:55:00+02:00 — Detached-child negative control failed as intended: a `setsid` child survived wrapper cancellation and wrote a late marker; exact fixture PID/process group was reaped.
- 2026-08-30T10:17:00+02:00 — The same Mac sandbox allowed `chdir /`; CWD-based `procreap` therefore cannot be the security authority. macOS production support was narrowed to fail-closed rather than adding a host/container dependency.
- 2026-08-30T10:18:00+02:00 — Independent plan review returned CHANGES_REQUIRED: probe/runtime manifest was not mechanically identical, cleanup was not decidable, and the edit boundary was broad/inexact.
- 2026-08-30T10:22:00+02:00 — ExecPlan iter 2 bound probe/exec/command to one immutable in-process binary/policy manifest, moved lifecycle authority to a non-skipped Linux PID-namespace canary, retained uncertain-cleanup evidence, and froze exact production/test paths.
- 2026-08-30T10:29:00+02:00 — Independent iter-2 review kept P0 open because the logical Codex entry is a JS launcher that resolves a separate native binary and Linux sandbox/bubblewrap helpers.
- 2026-08-30T10:34:00+02:00 — ExecPlan iter 3 directly executes the pinned native Codex and binds/revalidates the full pre-sandbox executable closure; any unenumerable helper keeps the platform unavailable.
- 2026-08-30T10:40:00+02:00 — Independent read-only plan review returned APPROVE for iteration 3; BUILD may begin red-first inside the exact boundary.
- 2026-08-30T10:44:00+02:00 — RED-FIRST froze the complete executable closure, immutable view/policy, direct-native launch, closed bootstrap, cold Codex-only composition, shared Test/Lint runner, fail-closed cleanup retention, and non-skipped Linux lifecycle contract.
- 2026-08-30T11:02:00+02:00 — Portable product implementation and falsifiers turned green; exact candidate/scratch policy hashes, schema containment, credential-free probe/command bootstrap, and pre-admission exact-config refusal are mechanically pinned.
- 2026-08-30T11:06:00+02:00 — `linux/amd64` and `linux/arm64` tagged test binaries cross-compiled. The exact arm64 binary ran on OrbStack and failed loudly before canary launch because `codex` is absent; OrbStack also has no `bwrap`. No install, dependency extraction, sudo, auth, or network setup was performed.
- 2026-08-30T11:14:00+02:00 — Focused normal/race suites, `make lint`, product build, and `make e2e` are green on current bytes.
- 2026-08-30T11:20:00+02:00 — Full `go test -race ./...`, `make docs`, and `git diff --check` are green. The rendered HTML contains the accessible Factory Publication happy-path diagram and static numbered fallback; npm reports 15 existing transitive audit findings but no source or dependency change was made.
- 2026-08-30T11:43:00+02:00 — Independent current-byte review found that the first lifecycle witness scanned unrelated host PIDs and could miss fast processes; sentinel state/ownership and the caller-controlled canary surface were also insufficient.
- 2026-08-30T12:03:00+02:00 — Lifecycle authority moved to a pinned bubblewrap pre-exec barrier (`--block-fd 3`, `--info-fd 4`). This first revision still used an owner-filtered global `/proc` scan and was superseded at 12:26 after review falsified that assumption.
- 2026-08-30T12:15:00+02:00 — Current bytes: focused tests, both tagged Linux cross-compiles, full `go test -race ./...`, `make e2e`, `make lint`, product build, `make docs`, Vet, and diff-check are green. Skill generation reports `skills/no-mistakes/SKILL.md` up to date.
- 2026-08-30T12:16:00+02:00 — The current arm64 tagged binary was executed non-skipped on OrbStack and failed loudly at exact runtime discovery because `codex` is absent. No platform credit, install, package extraction, sudo, auth, or host-policy change was taken.
- 2026-08-30T12:26:00+02:00 — Review invalidated the previous portable-green claim after newer lifecycle bytes. The boundary now binds the blocked bwrap child as namespace init via exact `NSpid` host-PID→1 before release, removes owner-based global `/proc` scans, uses a closure-bound long-lived live sentinel, and runs `setsid`/non-dumpable behavior in a separate exact private child rather than the bwrap session leader. DoD-5 reopened pending a full Current-Byte rerun.
- 2026-08-30T12:34:00+02:00 — Hash-bound OrbStack arm64 component evidence persisted at `build-loop-state/n0-confinement-orb-arm64-current.json`: NSpid refusal matrix, detached `setsid`/non-dumpable/all-FD-close including FD 300, and non-leader-thread child enumeration passed without skip. The full production canary on the same binary exited 1 before launch because both Codex and bwrap are absent; it earns no DoD-1/2/3/6 or release credit. Linux amd64 runtime remains unrun.
- 2026-08-30T12:58:00+02:00 — Current product/test/doc bytes passed focused Publication suites, both tagged Linux cross-compiles, `go test -race ./...` (including the 248s Steps suite), `make e2e` (375s real E2E plus Steps), `make lint`/Vet/skill-generation, `make build`, `make docs`, and `git diff --check`. Three independent read-only code reviews returned APPROVE with no P0-P2; none awarded the still-missing full Linux runtime credit. HTML generated 22 pages including the accessible Factory Publication Mermaid happy path and numbered fallback; npm reported the same 15 transitive findings without a dependency/source mutation.
- 2026-08-30T13:00:38+02:00 — The exact arm64 tagged binary was retransferred to OrbStack, the destination SHA-256 was verified as `5d24153b1c882f3bf842e772e687ca5b225f41095b46a4261e753c199f9297c8` immediately before both runs, and the three named component falsifiers passed again. The full production canary again exited 1 before launch because Codex is absent. Transfer, destination path/hash, commands, exit codes, and nonclaims are persisted in `build-loop-state/n0-confinement-orb-arm64-current.json` (`sha256:683724f08ae840ff45552d076bcd9175578fd63b8985459bcb1f7d0daea22cc9`).
- 2026-08-30T13:03:00+02:00 — Each of the three independent current-byte reviewers persisted its own exact-hash APPROVE artifact under `build-loop-state/reviews/`; all report no P0-P2 code findings and explicitly exclude Linux runtime/release credit. This closes the earlier unverifiable aggregate-review claim but does not substitute for the required Senox exact-byte review.
- 2026-08-30T13:05:52+02:00 — The online Linux VPS was discovered read-only, but both Tailscale SSH and batch SSH stopped before any remote command at the mandatory interactive Tailscale identity check. The waiting sessions were terminated; no VPS command, install, file transfer, host-policy, auth, or runtime mutation occurred.
- 2026-08-30T13:34:20+02:00 — Because no dedicated Codex Slack-bot identity is provisioned, no Slack/OAuth message was sent. Codex instead wrote an identity-explicit `@senox` PR-gate question to the shared handoff file `/Users/hardyheyde/Library/CloudStorage/Dropbox/Senox-Share/CODEX-TO-SENOX-factory-publication-pr-gate-2026-08-30.md` (`sha256:786ce51d87cc3825fb2e51d4f1eec2a76130ccfeb75dd9c56ebab792f4553515`), asking whether a clearly blocked Draft PR may be created as the Exact-Head review vehicle or must remain held until platform gates pass. No PR, push, commit, Slack message, or provider mutation occurred.
- 2026-08-30T13:37:12+02:00 — The Owner then explicitly instructed Codex to use Slack and prefix the question with `codex:`. Codex sent only that PR-procedure question in the existing Senox DM, tagged `<@U0B8P0P7K37>`, at Slack message `D0B8ATP35L7/1788089825.144249`. No code, Git, PR, merge, deploy, or other provider mutation accompanied the message.
- 2026-08-30T13:37:11+02:00 — Senox replied in the same DM: option A, create the Draft PR now as `platform-gate-blocked`, scored 9/10 and explicitly preferred over waiting. Conditions: Draft status, visible blocked label/status, missing arm64/amd64 canaries as checklist items, no merge/production claim, and Senox's formal review only on the final frozen head; any later code change stales it. This is procedural approval for a Draft review vehicle, not a code APPROVE, push/merge/deploy authorization, or platform credit. Senox asks for the Owner's GO for option A.
- 2026-08-30T13:42:00+02:00 — Owner granted “Go für A”. Pre-push inspection found 61 expected modified/untracked status entries, no recognized credential/private-key pattern, clean diff whitespace, upstream default `main`, read-only upstream permission for both configured GitHub accounts, and no existing fork. Option A will use an explicitly Codex-authored commit pushed through the Owner account `HeyHardy` to a new fork; the Draft PR body will begin `Codex:` and preserve every platform/review blocker.
- 2026-08-30T14:00:38+02:00 — Mandatory immediate pre-push tests completed on the frozen product/test bytes: `go test -race ./... -count=1` exited 0 (including `internal/pipeline/steps` 232.189s, `internal/publication` 43.544s, `internal/daemon` 113.193s); `make e2e` exited 0 (`internal/e2e` 372.655s, `internal/pipeline/steps` 68.600s). No product/test/doc byte changed during either run. Platform-only DoD-1/2/3/6 remains open and will be explicit in the Draft PR.

## Open questions / escalations
- The implementation remains fail-closed and cannot receive production credit until the exact tagged current-byte canary runs non-skipped on a Linux amd64/arm64 host with Codex and bwrap already provisioned.
- The known OrbStack host is not that environment: the executable canary was attempted and refused at discovery because `codex` is absent. Installing or extracting Codex/bwrap would exceed the frozen no-dependency/no-host-policy slice and needs a separate Owner decision.
- The known VPS is online but cannot be inspected or used unattended until the Owner completes the Tailscale SSH identity check. Its architecture and Codex/bwrap provisioning are therefore still unknown.

## Learnings (for the wrap-up report and skill improvement)
- A green filesystem/network sandbox is not a process-lifecycle proof; CWD/process-group reaping is evadable by `setsid` plus `chdir`, so the lifecycle authority must be a proved PID container.
- Prompt neutralization and permission profiles solve different threats and both must be mechanically pinned.
