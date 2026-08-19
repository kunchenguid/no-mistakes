<!-- execplan_template_version: 4 -->
---
execplanTemplateVersion: 4
deliveryShape: standalone
---

# ExecPlan — Native Grok pipeline agent

## Purpose / Big Picture

Make Grok Build a first-class No Mistakes pipeline backend so repositories can
configure `agent: [codex, grok]`. The Grok adapter must use Grok's current
default model unless an operator explicitly supplies a model override, support
native structured output, preserve subprocess and retry safety, and honestly
report whether it can neutralize target-repository instructions when trusted
policy sets `disable_project_settings: true`. If complete isolation cannot be
proved, No Mistakes must keep Grok available for ordinary repositories while
failing closed under that trusted opt-out.

## Authority and Read Order

1. `AGENTS.md`, especially **Repo Config Trust Boundary (security)**,
   **Context, Concurrency, and Processes**, and **When Making Changes**, owns
   the repository implementation and verification rules.
2. `internal/agent/agent.go` owns the adapter interface and native construction.
3. `internal/config/config.go` owns agent selection, probing, binary paths, and
   argument override policy.
4. `internal/types/types.go` owns supported agent names.
5. `docs/src/content/docs/reference/global-config.md` and
   `docs/src/content/docs/reference/repo-config.md` own public configuration
   documentation; `docs/src/content/docs/guides/agents.md` owns agent guidance.
6. Base checkpoint:
   `origin/main@6859d1e827f5ab2592a4703d3bab8734a38c9aa5`.

acceptedCheckpoint: `6859d1e827f5ab2592a4703d3bab8734a38c9aa5`

## Reference Index

| Owner | Kind | Authority ref |
|---|---|---|
| Repository rules | git | `AGENTS.md` |
| Agent contract | code | `internal/agent/agent.go` |
| Verified instruction neutralization | code/tests | `internal/agent/gateneutralize_test.go` |
| Adapter resolution | code | `internal/config/config.go` |
| Agent names | code | `internal/types/types.go` |
| User configuration | docs | `docs/src/content/docs/reference/global-config.md` |
| Repo configuration | docs | `docs/src/content/docs/reference/repo-config.md` |
| Agent behavior | docs | `docs/src/content/docs/guides/agents.md` |
| Verification procedure | git | `AGENTS.md` verification sequence |
| Plan authority | installed contract | `/Users/boriza/.codex/.agent/PLANS.md` |

## Program Integration Line Strategy

Implement on `feat/native-grok-agent` in
`/Users/boriza/Documents/dev/tmp/no-mistakes`, based on the immutable checkpoint
above. Commit only the Grok adapter, its tests, configuration/docs projections,
and this plan. Push to a PR branch rather than upstream `main`. Agent Platform
consumes the feature only after a tested binary is available locally and on the
remote host; its independent activation plan owns its repo policy change.

## Milestones

| Id | Outcome | Status |
|---|---|---|
| M0 | Baseline, security boundary, and Grok CLI wire format measured | complete |
| M1 | Failing tests pin native selection, arguments, parsing, and neutralization | complete |
| M2 | Native Grok adapter and configuration support implemented | complete |
| M3 | Documentation and fake-agent/e2e coverage updated | complete |
| M4 | Repository verification suite passes; provider-backed smoke is truthfully dispositioned | complete |
| M5 | Branch published; Agent Platform activation is either proven safe or explicitly held fail-closed | complete |

## Progress

- **2026-08-19:** Confirmed No Mistakes v1.40.3 source at the accepted
  checkpoint supports dynamic ACP targets but has no `grok` native agent name,
  binary mapping, probe order, adapter, or verified project-instruction
  suppression. Confirmed local Grok Build 1.0.5 is authenticated and exposes
  headless JSON/JSON-Schema operation plus `grok agent stdio`.
- **2026-08-19:** Added the native adapter, first-class config/type/probe
  resolution, managed-argument validation, structured event parsing, role-safe
  session resume, fake-agent support, full Grok e2e journey coverage, and
  public docs. The first focused test run passed. The first Grok e2e run then
  exposed a missing `doctor` row; added a targeted regression and corrected the
  supported-agent projection.
- **2026-08-19:** `go test ./cmd/fakeagent ./internal/agent ./internal/config
  ./internal/daemon ./internal/types` passed. `go test ./internal/cli -run
  TestDoctorAgentChecksIncludesGrok -count=1` passed. `go test -tags=e2e
  ./internal/e2e -run 'TestUserJourney/grok' -count=1` passed after the doctor
  correction and exercised every agent-driven pipeline step.
- **2026-08-19:** A standalone Grok 1.0.5 inspection probe in
  `/tmp/no-mistakes-grok-isolation.FZ9kYU` showed that Grok still discovers the
  repository's native `Agents.md` even with `--system-prompt-override` and all
  exposed Claude/Cursor compatibility discovery disabled. Changed
  `NeutralizesGateInstructions()` to return false and added negative gate
  regressions so `disable_project_settings: true` refuses Grok.
- **2026-08-19:** Rebased the implementation onto
  `origin/main@6859d1e827f5ab2592a4703d3bab8734a38c9aa5`, preserving upstream's
  review-session rule (fresh reviewers; reusable fixer only) and verified Pi
  neutralization. Subsequent entries record the completed post-rebase proof.
- **2026-08-19:** The first post-rebase full `make e2e` run timed out at the
  historical 300-second package limit while the fourth serial backend
  (OpenCode) was 26 seconds into an existing 60-second wait. Grok's focused
  journey had already passed. The full rerun passed under a 420-second ceiling
  (`internal/e2e` 391.176s; pipeline-step e2e 63.820s). Set the final wrapper
  budget to 480 seconds so normal local variance does not consume the entire
  margin while individual wait deadlines still surface stuck journeys.
- **2026-08-19:** Post-rebase verification passed: focused affected-package
  tests, the focused Grok e2e journey, `make lint`, `go test -race ./...`, the
  full `make e2e` rerun, and `go build -o ./bin/no-mistakes
  ./cmd/no-mistakes`. The only non-passing proof is the explicitly blocked
  provider-backed smoke (HTTP 402); no success is claimed for it.
- **2026-08-19:** Initialized the repository gate with
  `kunchenguid/no-mistakes` as upstream and
  `p3ngu1nx/no-mistakes` as the fork. AXI run
  `01M0CVF0Y466C7CRWCC3HV0ATD` reached `checks-passed` and opened
  `https://github.com/kunchenguid/no-mistakes/pull/776`. The pipeline fixed the
  missed invocation-environment propagation in commit `c60ea0d5`, added its
  process-level regression, clarified Grok's pipeline-only skill role in
  `dff83ad6`, and then passed targeted Test, documentation, lint, push, PR, and
  hosted CI. Guarded AXI sync integrated `dff83ad6` into this worktree.
- **2026-08-19:** Verified Agent Platform remains fail-closed on both machines:
  the local activation worktree uses `[codex, claude]` and the remote clone uses
  `[claude, codex]`, both with `disable_project_settings: true` and no Grok
  fallback. The separate Agent Platform activation plan owns reconciling the
  remote preference order and publishing that repository's workflow changes.
- **Next:** a maintainer reviews and merges PR #776. After a release contains
  this adapter, rerun the adversarial project-discovery probe against the then
  current Grok CLI before considering Agent Platform activation; absent new
  positive isolation proof, keep the trusted opt-out and safe fallback.

## Findings

- Grok can run through generic ACP, but generic ACP is intentionally rejected
  when `disable_project_settings: true`; Agent Platform retains that boundary.
- Leaving `-m`/`--model` absent is the required model policy: Grok selects its
  installed current default, avoiding a stale repository pin.
- Local Grok currently resolves its unpinned default to `grok-4.6`.
- Grok 1.0.5 still discovers native repository instruction surfaces despite
  full system-prompt replacement and compatibility-discovery opt-outs. Native
  Grok support is therefore valid for ordinary repositories but deliberately
  ineligible when `disable_project_settings: true`.
- A live isolated model call reached Grok but failed with HTTP 402 because the
  account's Grok Build usage balance is exhausted. Deterministic unit and e2e
  validation can proceed. The non-model inspection probe is already sufficient
  negative evidence to reject the isolation capability; replenishing usage
  cannot turn the current adapter's claim on without a new positive adversarial
  probe.

## Integrated Proof Obligations and Results

| Obligation | Evidence | Expected observation | Result |
|---|---|---|---|
| Native selection | config/types/constructor tests | `grok` parses, probes `grok`, and constructs a native adapter | passed |
| Default-model policy | argv tests | no managed `-m`/`--model` flag is emitted | passed |
| Structured output | parser + subprocess fixture tests | valid schema result becomes `Result.Output`; malformed/missing output fails closed | passed |
| Instruction isolation | standalone Grok inspect probe + neutralization/gate tests | either every project surface is inert or Grok is refused under the trusted opt-out | negative provider capability observed; fail-closed refusal implemented and focused tests previously passed |
| Process safety | cancellation/reaping tests and existing shell helper contract | invocation uses configured process groups and leaves no child behind | passed in full race suite |
| Focused verification | `go test ./cmd/fakeagent ./internal/agent ./internal/config ./internal/daemon ./internal/types`; targeted doctor and Grok e2e journey | exit 0 | passed |
| Full verification | `make lint`; `go test -race ./...`; `make e2e`; `go build -o ./bin/no-mistakes ./cmd/no-mistakes` | all exit 0 | passed |
| Live smoke | installed branch binary invokes Grok without a model override | successful structured response reports Grok backend | blocked: provider returned HTTP 402 usage balance exhausted; no success claimed |
| Consumer proof | Agent Platform trusted config remains valid locally and remotely | unsafe `[codex, grok]` is rejected while `disable_project_settings: true`; existing safe fallback remains configured | passed: Grok absent and opt-out enabled on both; activation intentionally held |
| Publication proof | AXI run `01M0CVF0Y466C7CRWCC3HV0ATD`; PR #776 | fork push, upstream PR, and hosted checks pass without direct upstream-main write | passed; `checks-passed`, PR open and mergeable |

## Surprises & Discoveries

- The installed Grok CLI advertises `--system-prompt-override`, `--verbatim`,
  `--json-schema`, and headless JSON output, but no Claude-style
  `--setting-sources` switch. Effective instruction isolation therefore needs
  empirical proof before the adapter can claim the neutralization capability.
- The first complete Grok e2e journey passed the pipeline but failed its doctor
  assertion because the native resolver and doctor table had separate agent
  projections. The dedicated doctor regression now pins Grok in that table.
- Live isolation proof is externally blocked by an exhausted Grok Build usage
  balance (HTTP 402); this is not a code/test failure.
- Unlike the compatibility surfaces disabled by environment variables, Grok's
  native `Agents.md` discovery remains active under
  `--system-prompt-override`. A replacement system prompt is defense in depth,
  not verified project-setting isolation.
- Adding Grok as the fourth serial user-journey backend exceeded the e2e
  wrapper's historical five-minute package timeout before an existing OpenCode
  wait received its own full deadline. The suite budget must grow with the
  intentionally serial environment-owning matrix.
- Independent review found that Grok's subprocess environment omitted
  `RunOpts.Env`. Routing it through the existing `gitSafeEnv(opts.CWD,
  opts.Env)` primitive fixed the defect and a subprocess regression now proves
  the invocation value reaches Grok.

## Decision Log

| Date | Decision | Rationale |
|---|---|---|
| 2026-08-19 | Implement a native adapter rather than relaxing Agent Platform's opt-out. | The project-instruction boundary is an intentional security feature; generic ACP cannot currently prove it. |
| 2026-08-19 | Do not set a managed Grok model. | The operator explicitly wants Grok's changing default model rather than a repository pin. |
| 2026-08-19 | Use TDD and require a real isolated instruction-loading probe. | Repository rules mandate TDD, and the security capability cannot be inferred from flag names alone. |
| 2026-08-19 | Keep the live isolation proof explicitly partial rather than weakening `disable_project_settings` or claiming fixture evidence as live proof. | The provider balance blocker is external; the trusted security boundary remains fail-closed. |
| 2026-08-19 | Report Grok as unverified and reject it whenever `disable_project_settings: true`. | The standalone inspect probe discovered native project instructions; claiming neutralization would violate the trusted repo-config boundary. |
| 2026-08-19 | Do not activate Grok in Agent Platform yet. | Agent Platform intentionally enables the trusted opt-out, so `[codex, grok]` must fail until Grok exposes complete and empirically verified isolation. |
| 2026-08-19 | Accept the AXI environment fix and documentation clarification. | Both changes reuse existing owners: invocation environment belongs to `gitSafeEnv`, and public guidance must distinguish pipeline-backend support from user-level skill installation. Fresh rereview, targeted Test, and hosted CI passed. |

## Durable Next Action / Recovery

The implementation is published at PR #776 with checks passed. A maintainer can
review and merge it without further local delivery work. Do not change Agent
Platform to `grok` while its trusted config keeps
`disable_project_settings: true`: the current adapter must be rejected there.
The provider-backed structured-response smoke remains blocked until Grok usage
is replenished; do not represent it as passed. The isolation decision is not
blocked on billing because a standalone probe already supplied negative proof.
If PR monitoring later reports a conflict, let the active AXI CI monitor own
the rebase; do not hand-rebase the branch.

## Outcomes & Retrospective

Native Grok support is implemented, independently reviewed, locally verified,
published from a fork, and green in hosted CI. The adapter leaves Grok's model
unmanaged by default, supports native structured output and fixer-session
resume, preserves invocation environment, and fails closed under the trusted
project-settings opt-out. AXI corrected one environment-propagation miss and
clarified documentation before publication.

Agent Platform activation is deliberately held, not silently incomplete:
current Grok 1.0.5 demonstrably discovers native project instructions, while
both Agent Platform machines retain `disable_project_settings: true` and a
verified non-Grok fallback. HTTP 402 still blocks a successful live response
smoke, but it does not weaken the negative isolation evidence or the shipped
fail-closed behavior. Remaining human action is review/merge of PR #776.

Revision note (2026-08-19): recorded the implemented adapter/config/docs/e2e
scope, focused proof, doctor omission and correction, and the external HTTP 402
live-provider blocker so execution can resume without chat history.

Revision note (2026-08-19): rebound the plan to the post-rebase checkpoint,
recorded the standalone negative isolation proof and fail-closed design,
replaced unsafe Agent Platform activation with an explicit held outcome, and
recorded the complete post-rebase local verification ladder.

Revision note (2026-08-19): recorded AXI fixes, fork/PR/CI publication,
guarded local synchronization, local/remote held-consumer proof, and final
outcomes before moving the plan to completed.
