# Ticket 19 e2e journey foundation (READ FIRST)

Repo: `/home/elo/repos/no-mistakes`. Go: `/home/elo/.local/share/go-toolchains/go1.25.0/bin/go`.
Run e2e: `<go> test -tags=e2e -run '<TestName>' -count=1 -v -timeout 300s ./internal/e2e`
Build/vet: `<go> vet -tags e2e ./internal/e2e`. gofmt your files before running.
Runner binary paths: prefer relative; the go toolchain is the absolute path above.

You are writing deterministic end-to-end journeys that prove the routing contract without paid providers. The e2e drives the REAL `no-mistakes` binary + a fake agent (`cmd/fakeagent`) over `git push -> daemon`. **A WORKED, PASSING TEMPLATE already exists: `internal/e2e/cascade_journey_test.go` (TestCascadeDirectLunaSuccess, TestCascadeLunaTerraSol). READ IT — copy its structure exactly.**

## The fake agent (how to script behavior)
Scenario YAML matched against each invocation. `Scenario.Match(prompt, model, effort)` returns the FIRST action whose constraints all hold. Action fields (cmd/fakeagent/scenario.go):
- `match`: substring that must be in the PROMPT (empty = any prompt).
- `model`: substring that must be in the invoked candidate's model (e.g. "luna","terra","sol"). Empty = any.
- `effort`: substring that must be in the invoked effort ("low","medium","high","xhigh"). Empty = any.
- `structured`: the JSON structured-output map (findings/verdicts/etc). `structured_raw`: raw JSON string.
- `text`: human text. `edits: [{path,old,new}]` (empty old overwrites file). `stage: [paths]`. `delay_ms`.
- `fail`: inject a failure (cmd/fakeagent/failure.go):
  - `"operational"`: exit non-zero with a NON-transient operational needle in stderr (default "usage limit reached for this account"); the real adapter classifies an `*agent.OperationalError` on the FIRST exec and OPENS the runner's provider circuit. Override text via `fail_needle`.
  - `"transient"`: exit non-zero with a retryable needle for the first `fail_times` execs (default 2), then FALL THROUGH to the normal success emission — proves the adapter retries within ONE invocation. Counter persists per action beside $FAKEAGENT_LOG.
  - `"output"`: emit wire-valid output with NO parseable structured output -> adapter classifies a NON-operational model-output error that NEVER opens a circuit. Use codex for an immediate non-op (claude retries missing-structured).
- An action with no constraints (all empty) is the catch-all; list it LAST.

Match ordering matters (first match wins). List the most specific actions (with model/effort/unique-substring) BEFORE catch-alls.

## Prompts you can match on (all include the finding/verdict content; NONE include the branch)
- Review: `"Review the code changes and return structured findings with a risk assessment.\n\nContext:\n- branch: <branch>"` (this one DOES carry the branch).
- Fixer (repair): `"Fix the following N code-review finding(s)..."` then `- lineage <id>, severity <sev> (file:line): <description>`. Also includes the base..HEAD diff.
- Verifier (repair): `"Independently verify whether each of the following code-review findings has been resolved..."` then `- lineage <id>, severity <sev>: <description>`, then `"Changes to adjudicate:\n<diff>"` where diff is base..HEAD (cumulative NET delta — a file overwrite shows only the final content).
- Verdict structured shape: `verdicts: [{lineage_id: "PROMPT_LINEAGE_ID", status: "resolved"|"unresolved"|"inconclusive", rationale: "..."}]`, `new_findings: [...]`. The sentinel `PROMPT_LINEAGE_ID` (anywhere, incl nested) is auto-substituted with the lineage id parsed from the verifier prompt. A resolved verdict requires a NON-EMPTY rationale.
- new_findings entry fields (patch-caused): `{severity, action, description, caused_by_lineage_id: "PROMPT_LINEAGE_ID"}` (VERIFY exact JSON field names in internal/pipeline batch-verdict parsing before relying on them).
- Test prompt: `"You are validating a code change by testing it..."`. Document/Lint/PR/CI/Intent prompts each start with a distinctive sentence (grep internal/pipeline/steps/*.go and review.go for the exact leading text).

## Routing (config.DefaultRoutingConfig, internal/config/routing.go)
Runners: `codex`->FailureDomainOpenAI (candidate index 0, primary), `claude`->FailureDomainAnthropic (index 1, backup). Same-Profile candidates are tried in order; providers are never raced.
Profiles (name -> codex model / claude model, effort):
- fix_fast: gpt-5.6-luna / claude-sonnet-5, EffortMedium
- prose_fast: gpt-5.6-luna / claude-sonnet-5, EffortLow
- fix_balanced: gpt-5.6-terra / claude-opus-4-8, EffortMedium
- tools_balanced: gpt-5.6-terra / claude-opus-4-8, EffortHigh
- review_strong: gpt-5.6-sol / claude-fable-5, EffortHigh
- authority_strong: gpt-5.6-sol / claude-fable-5, EffortXHigh
Per-Purpose routes (a Route is an ordered Profile list; request.Tier indexes it):
- PurposeInitialReview -> [review_strong]
- PurposeStructuredFindingRepair -> [fix_fast, fix_balanced, authority_strong]  (blocking fixer cascade)
- PurposeNormalAggregateVerification -> [review_strong]  (sub-max repair verifier + normal Verify step)
- PurposeEscalatedAggregateVerification -> [authority_strong]  (final-tier repair verifier + escalated Verify)
- PurposeIntentSensitiveRepair -> [fix_balanced, authority_strong]  (consented ask-user repair)
- PurposeUnstructuredTestRepair -> [fix_balanced, authority_strong]
- PurposeUnstructuredCIRepair -> [fix_balanced, authority_strong]
- PurposeUnstructuredConflictRepair -> [fix_balanced, authority_strong]
- PurposeTestEvidence -> [tools_balanced]; PurposeLintInspection -> [tools_balanced]
- PurposeDocumentationAuthoring -> [prose_fast]; PurposeDocumentationVerification -> [tools_balanced]
- PurposePRComposition -> [prose_fast]; PurposeIntentSummarization -> [prose_fast]; PurposeIntentDisambiguation -> [tools_balanced]
- PurposeInformationalRepair -> [fix_fast, tools_balanced] (NEVER authority_strong); PurposeInformationalRepairVerification -> [tools_balanced]

## Provider circuit (internal/pipeline/circuits.go, routing_invoker.go)
- `providerCircuits{open map[FailureDomain]bool}`, run-wide. A launched candidate that fails with a classified `*agent.OperationalError` calls `circuits.markOpen(domain)` and fails over to the next same-Profile candidate (Anthropic backup). Every subsequent candidate in that domain is SKIPPED (InvocationOutcomeSkipped, its terminal FailureDomain names the open circuit) without launching — including in LATER steps of the same run (run-wide). If all candidates unavailable -> fail closed ("has no available candidate: all provider circuits are open" or "exhausted every candidate after operational failures").
- Non-operational failures (malformed/missing structured output, cancellation) are returned as-is and NEVER open a circuit.
- Adapter retry: within ONE Invoke the adapter re-execs the CLI up to 4 attempts on transient needles (overloaded/rate_limit/429/500/502/503/504/529/connection reset/i-o timeout/...). Use `fail: transient, fail_times: N` to fail N execs then succeed.

## Verify / Push / CI (internal/pipeline/steps/verify.go, push.go, forcepush.go, ci.go, ci_fix.go)
- Verify SKIPS when the latest 'reviewed' seal SHA == the current sealed candidate SHA (unchanged, already strong-reviewed). Otherwise it runs; effort is XHIGH (EscalatedAggregateVerification/authority_strong) when UserIntent!="" OR sctx.Fixing OR the initial review was high-risk, else review_strong.
- Push is transport-only: refuses a dirty worktree; refuses HEAD != seal SHA ("must be resealed before publishing"); force-with-lease anchored to last-seen remote; refuses to discard unincorporated remote commits (forcePushWouldDiscardError = drift refusal).
- CI: ciRepairBudget=3; autoFixCI runs verifyCIPatch (local checks) + a fresh strong verifier BEFORE any commit; an unverified/failing patch is reverted and fails closed; a checked+verified patch is committed with a `ci_republish` seal and does NOT re-enter the Verify step. ciRepairTier escalates fix_balanced->authority_strong.
- To drift a branch: make an out-of-band commit to the upstream/remote after the pipeline's last-seen, then verify Push refuses.

## DB / persistence (source of truth for assertions)
- Each InvocationAttempt.Start.Candidate = {Profile, Tier, CandidateIndex, Runner, Model, Effort}; Start.Purpose, Start.Role (fixer/verifier); Terminal.Outcome (succeeded/failed/skipped/...), Terminal.FailureDomain.
- Circuits are NOT persisted as a table; reconstruct from attempt terminals (skipped rows carry FailureDomain; candidate_index>0 = a failover; tier>0 = escalation).
- FindingRepair rows: one per lineage per tier, keyed by lineage_id; fields Tier, RemainingBudget, Verdict (resolved/unresolved/inconclusive), Status (resolved/unresolved/failed/pending), FixerAttemptID, VerifierAttemptID, Description (the finding at that tier — patch-caused inheritance REPLACES it), Severity, Action.
- run_seals table: sha + reason ('reviewed','ci_republish',...). Seal accessors in internal/pipeline/steps (LatestSeal/LatestSealByReason) and db.

## Harness API (internal/e2e/harness.go, harness_db.go, routing_assert_test.go)
- `NewHarness(t, SetupOpts{Agent:"codex", Scenario: writeScenario(t, yaml)})`; then `initGate(t, h)` (runs `no-mistakes init`, creates the gate remote + daemon). ALWAYS init before pushing.
- `h.CommitChange(branch, path, content, msg)`, `h.PushToGate(branch)` (fires the pipeline), `h.WaitForRun(branch, timeout)` -> *ipc.RunInfo (blocks to terminal), `h.WaitForRunRunning`, `waitForStepStatus(t,h,branch,types.StepReview,types.StepStatusAwaitingApproval,timeout)`.
- `h.Respond(runID, step, action)` drives approval gates (types.ActionAbort/ActionApprove/ActionFix/ActionSkip). Use axi via `h.Run("axi","run","--yes",...)` for unattended consent.
- `h.RestartDaemon(t)` restarts + verifies liveness (for reconnect). Every IPC call re-dials the socket, so reconnect is implicit.
- `h.InvocationAttempts(t, runID) []*db.InvocationAttempt` (all pipeline attempts, start order). `h.FindingRepairs(t, runID) []*db.FindingRepair`. `h.OpenDB(t) *db.DB` (escape hatch for any db accessor; caller Closes).
- Shared helpers (routing_assert_test.go): `writeScenario`, `initGate`, `attemptsForPurpose(attempts,purpose)`, `succeededAttemptsFor(attempts,purpose)`, `circuitSkips(attempts,domain)`, `candidateModels(attempts)`, `assertCandidate(t,attempt,profile,tier,modelSubstr,effort)`.
- `h.AgentInvocations() []Invocation` reads $FAKEAGENT_LOG (Invocation now has Model/Effort fields too), but the DB is the durable source of truth — prefer DB assertions.

## Rules for your journeys
- ONE finding-repair collision rule: within a SINGLE scenario, `match: "Fix the following"` and `match: "Independently verify..."` are unambiguous only when there is ONE repair finding. If a scenario needs multiple distinct repairs, isolate them into SEPARATE test functions (separate Harness + scenario), OR distinguish by model/effort/diff-marker. The template uses one repair per test.
- Each test: own Harness, own scenario, unique branch. Assert via DB (InvocationAttempts + FindingRepairs), not just prompt logs. Abort the run at the end if it parked at a gate (`h.Respond(run.ID, step, types.ActionAbort)` then `h.WaitForRun`).
- Do NOT edit files outside your assigned test file. Do NOT run the full suite/lint (the integrator does that). Run ONLY your test file's tests to green.
- Match existing conventions in cascade_journey_test.go exactly (build tag `//go:build e2e`, package e2e, imports).
