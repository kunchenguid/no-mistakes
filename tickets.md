# Tickets: purpose-aware multi-model routing

These tickets implement the approved purpose-aware routing Task spec as an internal expand–contract followed by a clean routing-only cutover.

Work the **frontier**: any ticket whose blockers are all done.
Complete one frontier ticket per fresh context with `/implement`.

## Establish durable invocation context

**What to build:** Introduce the stable internal vocabulary and durable ownership needed before any model-routing behavior changes.
A caller must provide a registered semantic Purpose and invocation payload without knowing runner, model, effort, provider, or command-line mechanics.
The executor must reserve a Step round before a model can launch, and each selected Candidate must have append-only start and terminal facts tied to either a real pipeline scope or a standalone utility scope.
Existing gates must retain their current model selection and approval behavior during this prefactor.

**Blocked by:** None — can start immediately.

- [x] The Purpose registry covers every pipeline, verification, repair, intent, PR, documentation, and Wizard invocation required by the approved spec.
- [x] Every pipeline invocation has stable run, Step-result, and pre-reserved round identities before process launch; standalone utility invocations use an explicit non-pipeline scope without fabricated gate rows.
- [x] The current round is excluded from its own prior-round prompt history, while success, failure, cancellation, and approval parking preserve existing completed-round ordering and meaning.
- [x] Candidate-attempt start and terminal facts are append-only, secret-free, recoverable after interruption, and sufficient to project one active or completed attempt without changing current routing behavior.

## Resolve and launch normalized Profile candidates

**What to build:** Add strict but initially additive Runners, Profiles, Candidates, and semantic Routes, then make Codex and Claude execute a normalized runner/model/effort request through fresh native processes.
Global configuration owns execution mechanics.
A trusted default-branch repository configuration may map Purposes only to existing global Profiles.
Production callers remain on the legacy constructor until their vertical slice migrates.

**Blocked by:** Establish durable invocation context.

- [x] The six default Profiles resolve exactly to Luna-medium/Sonnet-medium, Luna-low/Sonnet-low, Terra-medium/Opus-medium, Terra-high/Opus-high, Sol-high/Fable-high, and Sol-xhigh/Fable-xhigh in that order.
- [x] Strict validation rejects incomplete or non-finite Routes, unknown Purposes or Profiles, empty Candidates, unsupported runners, invalid efforts, and repository attempts to define execution mechanics; Pro model selectors remain valid but are absent from defaults.
- [x] Repository Route overrides come only from the pinned trusted default-branch copy and cannot be controlled by the pushed branch or by the repository-command opt-in.
- [x] Codex and Claude translate normalized model and effort values to exact native arguments while preserving fresh sessions, schemas, streaming, token accounting, cancellation, process cleanup, and structured operational-failure classification after adapter retries.

## Route and surface the initial review

**What to build:** Deliver the first complete routing tracer bullet.
Initial Review must resolve to `review_strong`, run Sol-high or Fable-high in a fresh invocation, persist every Candidate attempt and durable root finding lineage, and expose active and completed routing state through AXI and TUI.
All unmigrated calls continue using the temporary internal legacy path.

**Blocked by:** Establish durable invocation context; Resolve and launch normalized Profile candidates.

- [ ] Initial Review always uses `review_strong`; an initially high-risk result records risk but does not upgrade the discovery invocation to xhigh.
- [ ] Every attempted Candidate is recorded before launch and terminally finalized with Purpose, Route, Profile, tier, Candidate, runner, model, effort, failure domain, timing, token usage, role, outcome, and root-lineage relationships without storing prompts, reasoning, credentials, or environment values.
- [ ] Every returned finding receives a stable run-wide root lineage independent of model-generated display IDs or finding prose, and malformed review output cannot count as a clean strong review.
- [ ] AXI and TUI reconstruct active and completed review routing from durable structured state after reattach or restart without parsing logs; missing or unknown routing data fails before model launch.

## Open run-wide provider circuits

**What to build:** Separate provider availability from repair quality.
After a native adapter exhausts its retries with a classified operational failure, open that provider family for the rest of the gate, execute the equivalent Candidate from the backup family for the current Profile, and skip the failed family on every later Purpose and tier.

**Blocked by:** Route and surface the initial review.

- [ ] Quota or usage limits, provider outage or overload, authentication failure, and an unavailable executable can open a circuit only after adapter retries finish.
- [ ] Malformed output, bad edits, failed deterministic checks, unresolved findings, cancellation, and workspace failures never open a provider circuit.
- [ ] Once OpenAI or Anthropic opens, no later invocation in the same gate probes that domain; a new gate starts with closed circuits.
- [ ] Candidate order, circuit transitions, and skipped-domain decisions are visible in immutable history, providers are never raced, and exhausting all Candidates for a required Profile fails the gate rather than weakening the Profile.

## Install the dormant 10-gate canary

**What to build:** Add the persistence and reporting needed to freeze the immediately preceding ten successful gates before routing activates and collect the first ten successful routed gates afterward.
The canary remains dormant until the clean cutover ticket activates the policy.
Its 30% median-latency target is advisory and must never mutate routing.

**Blocked by:** Open run-wide provider circuits.

- [ ] One idempotent activation transaction freezes the prior ten successful run IDs, completion times, workload facts, finding counts, and comparable Step-round measurements before a routed run can enter the cohort.
- [ ] The first ten successful routed runs enter the canary exactly once; failed, cancelled, duplicate, pre-activation, and later successful runs cannot replace either frozen cohort.
- [ ] The report compares the same execution-only agent-bearing Step-round metric and supplements it with exact invocation duration, escalation, failover, changed-file, changed-line, and initial-finding facts where available.
- [ ] Incomplete cohorts report their state truthfully, the 30% calculation handles zero and even-sized medians deterministically, and a missed target remains visible but never changes Profiles, Routes, circuits, or gate outcomes.

## Route gate-scoped routine work

**What to build:** Route every existing gate-scoped invocation that is neither a repair nor aggregate Verify through an explicit Purpose and the same durable router.
Routine prose must use `prose_fast`; repository-heavy evidence and disambiguation must use `tools_balanced`.
Preserve established caller fallbacks and side-effect containment as explicit behavior rather than accidental legacy routing.

**Blocked by:** Open run-wide provider circuits.

- [ ] Intent summary and PR composition use `prose_fast`, while intent disambiguation and test evidence use `tools_balanced`, with exact Purpose and ownership in invocation history.
- [ ] Disabled or inapplicable work skips without fake invocations, while an enabled required Route fails closed when all same-Profile Candidates are unavailable.
- [ ] Intent disambiguation preserves its worktree restoration guarantee and cannot leave model edits behind.
- [ ] A production-call audit proves these gate-scoped callers cannot invoke a native adapter directly or omit a registered Purpose.

## Route Wizard suggestions in standalone scope

**What to build:** Route branch and commit suggestions through `prose_fast` under one standalone Wizard scope.
The Wizard must no longer construct a single native agent directly or invent pipeline runs, Steps, or rounds.
Its provider circuit lasts only for the Wizard session.

**Blocked by:** Open run-wide provider circuits.

- [ ] Combined branch-and-commit and commit-only suggestions use the registered suggestion Purpose and `prose_fast`, preserving the existing cached commit result without a duplicate invocation.
- [ ] Candidate attempts persist under one explicit utility-scope identity and never claim nonexistent pipeline ownership.
- [ ] The Wizard uses trusted routing policy rather than current feature-branch execution settings, and cancellation closes its process, circuit, and active invocation state.
- [ ] Standalone active and completed history is available through the appropriate structured operator surface without fabricated gate records.

## Apply one fast verified blocking repair

**What to build:** Resolve one selected blocking root finding through a fresh Luna/Sonnet-medium fixer, relevant deterministic checks, and a separate fresh strong verifier.
The verifier must explicitly adjudicate the selected lineage.
Until later escalation tiers exist, every non-resolved outcome must terminate safely rather than re-entering the legacy numeric loop.

**Blocked by:** Route and surface the initial review.

- [ ] The selected root finding is persisted with immutable content, action, severity, tier, remaining budget, and links to the fixer, checks, and verifier.
- [ ] The fixer receives only structured intent, diff, lineage, prior-attempt, deterministic-evidence, and remaining-budget data in a fresh invocation.
- [ ] Applicable deterministic checks run before the verifier; passing or explicitly inapplicable checks lead to a different fresh `review_strong` invocation.
- [ ] Only an explicit rationale-bearing `resolved` verdict for the selected lineage succeeds; failed checks, missing IDs, silence, malformed adjudication, `unresolved`, and `inconclusive` outcomes fail safely.

## Escalate and batch finding lineages

**What to build:** Complete the blocking repair coordinator with Luna→Terra→Sol quality escalation, checks-before-review, same-tier batching, explicit patch-caused attribution, inherited lineage budgets, unrelated-finding separation, and independent xhigh final authority.
Provider failover remains inside a Profile and never advances the quality tier.

**Blocked by:** Apply one fast verified blocking repair.

- [ ] Blocking lineages advance `fix_fast → fix_balanced → authority_strong` exactly, and a failed deterministic check advances the affected batch without spending an intermediate strong-verifier invocation.
- [ ] All unresolved selected lineages at one tier are fixed together, while resolved or differently tiered lineages do not rerun; unattributable shared-check failures conservatively advance every included lineage.
- [ ] Patch-caused verifier findings inherit their root lineage’s next tier and remaining ceiling, while unrelated findings create separate roots and cannot disappear through prose or fuzzy-ID matching.
- [ ] A Sol/Fable fixer can succeed only after a different fresh Sol/Fable-xhigh authority invocation; final-tier check failure, unresolved verdict, inconclusive verdict, or missing adjudication fails closed.

## Apply severity, consent, and unattended policies

**What to build:** Add the approved non-blocking and intent-sensitive branches to the common repair coordinator.
Informational work receives only the cheap two-tier policy.
Intent-sensitive work cannot begin before consent.
Unattended consent authorizes configured repairs but can never waive final authority.

**Blocked by:** Escalate and batch finding lineages.

- [ ] Informational auto-fixes use `fix_fast → tools_balanced`, never invoke Sol/Fable, remain visible when unresolved, and never block the gate.
- [ ] An `ask-user` finding starts no fixer before consent; explicit human or unattended consent starts at `fix_balanced` and may escalate to `authority_strong`.
- [ ] A `no-op` finding never enters a repair cascade.
- [ ] AXI `--yes` and TUI unattended behavior fail on an exhausted or inconclusive blocking lineage instead of approving after one attempted fix or a missing finding ID.

## Route Test evidence and repair

**What to build:** Give Test its complete routed behavior.
Evidence collection uses `tools_balanced`.
A failed configured test or unstructured test-log repair starts at Terra/Opus, reruns the relevant deterministic test before strong adjudication, and commits publishable source, test, and opted-in evidence changes before candidate sealing.

**Blocked by:** Apply severity, consent, and unattended policies.

- [ ] Test evidence uses `tools_balanced` and records concrete evidence without becoming final authority over a repaired blocking lineage.
- [ ] A configured test failure starts at `fix_balanced`, reruns the exact relevant check after each patch, and advances without an intermediate verifier when the check still fails.
- [ ] New tests, source fixes, and opted-in repository evidence are committed during Test, while temporary evidence remains outside the publish candidate.
- [ ] Existing generated-test safeguards, artifact handling, deterministic command behavior, and user-visible testing summaries remain intact.

## Route Lint, formatting, and repair

**What to build:** Give Lint its complete routed behavior and move all formatter-produced content changes before final verification.
No-command inspection uses `tools_balanced`.
Structured lint repair uses the common coordinator and the configured deterministic command.

**Blocked by:** Apply severity, consent, and unattended policies.

- [ ] No-command lint inspection uses `tools_balanced`, records its edits, and cannot act as the strong final verifier for its own patch.
- [ ] Configured lint failures rerun the declared deterministic command before strong adjudication and use the approved structured repair policy.
- [ ] Formatting runs before candidate sealing, and every formatter or lint change is committed and included in later aggregate verification.
- [ ] Lint completion leaves no dirty formatter or lint changes for Push to discover.

## Author and independently verify documentation

**What to build:** Give Document its approved two-model authoring and verification behavior.
Luna/Sonnet-low authors documentation, deterministic integrity and applicable configured checks run, and a fresh Terra/Opus-medium invocation verifies accuracy and completeness before commit.

**Blocked by:** Apply severity, consent, and unattended policies.

- [ ] Documentation authoring uses `prose_fast` and never self-verifies semantically.
- [ ] Deterministic documentation checks run before the fresh `tools_balanced` verifier, and no applicable check is represented explicitly rather than treated as implicit success.
- [ ] Every changed-document result receives explicit verifier adjudication before commit; blocking or inconclusive documentation findings cannot pass as successful authoring.
- [ ] A defect caused by the authoring patch inherits the next tier of its lineage rather than receiving a fresh Luna/Sonnet budget.

## Seal the publish candidate and make Push transport-only

**What to build:** Establish one immutable pre-publication candidate after Test, Document, Lint, formatting, evidence handling, staging, and commits have completed.
Push must validate a clean worktree and the exact sealed SHA, then perform transport only while preserving existing remote data-loss protections.

**Blocked by:** Route Test evidence and repair; Route Lint, formatting, and repair; Author and independently verify documentation.

- [ ] Candidate sealing occurs only after every pre-Verify content mutator has completed, and it records the exact HEAD plus clean-worktree state.
- [ ] Push does not format, stage, write evidence, create a generic catch-all commit, or otherwise mutate the sealed content.
- [ ] Push refuses a changed HEAD or dirty worktree even when the recorded commit is unchanged, and a repaired/reverified candidate creates a new seal rather than rewriting the old one.
- [ ] Existing force-with-lease and unseen-remote-commit protections remain intact and operate on the verified sealed SHA.

## Route Rebase conflict repair

**What to build:** Route Rebase and merge-conflict repair through the stronger unstructured policy.
The repair starts at Terra/Opus, validates conflict resolution and applicable deterministic commands, receives fresh strong adjudication, and updates the candidate only after success.

**Blocked by:** Apply severity, consent, and unattended policies.

- [ ] Conflict repair starts at `fix_balanced` and may escalate to `authority_strong`; it never uses the fast tier merely to infer scope.
- [ ] Git conflict state and relevant deterministic checks run before strong adjudication, and unresolved or inconclusive conflict work fails closed.
- [ ] Successful resolution updates the branch and routing history only after checks and independent verification complete.
- [ ] Existing rebase safety, abort, and remote-update behavior remain observable and no legacy direct-agent repair path survives this slice.

## Verify and republish CI repairs

**What to build:** Replace CI’s direct repair-and-push shortcut with a forward-only verified republish cycle.
A CI failure produces a new patch, runs local deterministic checks, receives fresh strong verification, seals the exact new candidate, and republishes that SHA before hosted CI resumes.

**Blocked by:** Apply severity, consent, and unattended policies; Seal the publish candidate and make Push transport-only.

- [ ] CI failure and post-publication conflict repair start at `fix_balanced`, may escalate to `authority_strong`, and use the same finding-lineage and provider-circuit rules as pre-publish repair.
- [ ] Every CI patch runs relevant local deterministic checks and a fresh strong verifier before commit or remote update.
- [ ] CI seals and republishes the exact verified SHA, never pushes an unreviewed patch, and never jumps the executor backward to the earlier aggregate Verify Step.
- [ ] Repeated remote failure consumes the existing lineage budget, while exhausted or inconclusive blocking work fails closed under unattended consent.

## Gate the final candidate with Verify

**What to build:** Add a dedicated Verify Step after Document and Lint and before Push.
Verify compares the exact finalized candidate with the latest strong-reviewed HEAD, skips only an unchanged already-reviewed candidate, and otherwise performs fresh aggregate verification with the approved high/xhigh escalation rules.

**Blocked by:** Apply severity, consent, and unattended policies; Seal the publish candidate and make Push transport-only.

- [ ] Every execution, persistence, AXI, and TUI representation uses the fixed sequence `Intent → Rebase → Review → Test → Document → Lint → Verify → Push → PR → CI`.
- [ ] Verify skips only when the sealed candidate exactly matches the latest successful strong-reviewed candidate and otherwise reviews the aggregate later diff plus deterministic evidence in a fresh invocation.
- [ ] Normal verification uses `review_strong`; initially high risk with later changes, intent-sensitive work, authority-tier work, a blocking finding surviving balanced repair, or inconclusive evidence uses `authority_strong`.
- [ ] Verify findings enter the common lineage coordinator, unresolved or inconclusive blocking work prevents Push, and a successful repair produces a newly sealed and reviewed candidate.

## Contract to routing-only configuration

**What to build:** Activate the frozen routing policy only after every production invocation and repair path has migrated, then remove the temporary expansion and make Runners, Profiles, Candidates, and Routes the sole model-selection contract.
Reject legacy configuration instead of rewriting or aliasing it.

**Blocked by:** Install the dormant 10-gate canary; Route gate-scoped routine work; Route Wizard suggestions in standalone scope; Apply severity, consent, and unattended policies; Route Test evidence and repair; Route Lint, formatting, and repair; Author and independently verify documentation; Seal the publish candidate and make Push transport-only; Route Rebase conflict repair; Verify and republish CI repairs; Gate the final candidate with Verify.

- [ ] The effective configuration, generated defaults, runtime construction, and Wizard contain no single-agent selector, fallback-agent list, arbitrary agent argument or path override, repository agent selector, or numeric per-Step auto-fix limit.
- [ ] Legacy keys produce strict actionable errors, and no alias, compatibility spelling, automatic rewrite, implicit Profile, sticky fallback, or default-Purpose bridge remains reachable.
- [ ] Doctor validates normalized runner executables, Profile Candidates, model/effort readiness, failure domains, trusted Route coverage, and provider availability instead of recommending a legacy Agent.
- [ ] The canary baseline and policy fingerprint commit before the first clean routed gate is accepted, and every subsequent native launch is reachable only through a registered Purpose and immutable Candidate record.

## Prove the complete cutover end to end

**What to build:** Extend the deterministic fake-agent journey so real configuration loading, native arguments, daemon execution, persistence, AXI/TUI projections, git publication, and CI behavior prove the complete routing contract without paid providers.

**Blocked by:** Contract to routing-only configuration.

- [ ] Journeys prove direct Luna success, Luna→Terra, Luna→Terra→Sol with a separate xhigh final reviewer, same-tier batching, patch-caused budget inheritance, informational termination, consented Terra→Sol, and terminal fail-closed exhaustion.
- [ ] Provider journeys prove adapter retries, run-wide OpenAI circuit opening, Anthropic same-Profile backup, no later OpenAI probe, all-domain failure, and non-operational controls that never open a circuit.
- [ ] Publication journeys prove immutable candidate sealing, unchanged Verify skip, every xhigh Verify trigger, Push refusal after drift, and checked/verified CI republish without Verify re-entry.
- [ ] Reconnect snapshots and histories match persisted attempts, circuits, and lineages, while existing happy-path, approval, cancellation, intent, command, and remote-safety journeys remain green.

## Publish the public routing contract

**What to build:** Update every public and agent-facing contract to describe only the proven routing system, clean configuration cutover, provider circuits, repair lineages, consent, Verify, observability, migration, and advisory canary.
Generate the installed skill through its supported source-first workflow.

**Blocked by:** Contract to routing-only configuration.

- [ ] Configuration, provider, pipeline, repair, daemon, AXI, Wizard, troubleshooting, and migration guidance contains the exact default Profile and Route tables and no legacy single-agent, arbitrary-argument, fallback-agent, or numeric-attempt examples.
- [ ] Both top-level language variants accurately show the ten-Step pipeline and normalized routing behavior and receive independent human review.
- [ ] The installable skill source is updated first and generated output is refreshed through the supported generator, with consent and fail-closed wording matching AXI and TUI behavior.
- [ ] Canary documentation distinguishes the required report from the advisory 30% target and never claims a live result before ten successful routed gates exist.
