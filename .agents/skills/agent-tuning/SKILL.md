---
name: agent-tuning
description: Use when changing agent model or effort configuration, adapter mappings, or eval candidate profiles.
user-invocable: false
metadata:
  internal: true
---

**Unified Agent Tuning (`internal/agentcfg`)**

- `agentcfg` is the single owner of the harness-neutral model/effort surface and of the mapping down to each harness's native mechanism (claude/copilot `--effort`, codex `-m` + `-c model_reasoning_effort`, grok `--reasoning-effort`, pi `--thinking`, opencode's session-message `model`/`variant`, acpx `--model` for `cursor`/`acp:<target>`). Add a harness there, not in an adapter or in eval. `rovodev` and `antigravity` are deliberately declared unmappable, so a request for them is a config error rather than a flag that is silently ignored.
- `agent.NewWithOptions` is the one funnel: it validates `Options.Profile` and splices the mapped args after the operator's raw `agent_args_override` args, so both the pipeline (`cfg.AgentProfileFor`) and eval replay (`Candidate.Profile()`) reach every harness by the same path. Never re-derive a model or effort flag at a call site.
- Precedence is fixed: a raw `agent_args_override` flag that already pins a knob natively wins and the mapped value is not emitted, which is what keeps every pre-`agent_config` configuration byte-identical and stops a harness receiving one knob twice. `agent_config` is global-only for the same reason as `agent_args_override`.
- Opt-in Pi run pins (`axi run` / `rerun` `--model`, `--effort`) resolve in `config/pi_profile.go`, persist immutably in `runs.pi_profile`, and are applied before agent construction on launch and recovery. Raw selection flags conflict at creation; later raw/role/global model changes cannot replace a run pin. Unpinned runs retain the precedence above. Semantics owner: `docs/src/content/docs/reference/global-config.md` (Per-run Pi profiles); regressions: `*_pi_profile*` / `pi_profile_test.go` in config, db, daemon, CLI and e2e.
- Eval candidates are `agent,model=<model>[,effort=<level>]` (the previous `agent+model` spelling is refused with a migration message), effort is part of the persisted candidate identity, and `agentNeutralGlobalConfig` strips `agent`, `agent_args_override`, and `agent_config` so a replay never inherits the capturing machine's pins.
- Keep eval replay identity comparison centralized in `agentcfg.ServedMatchesRequested`; do not compare an adapter's reported model directly at call sites. The user-facing normalization semantics and candidate guidance live in [`docs/src/content/docs/reference/eval.md`](../../../docs/src/content/docs/reference/eval.md).
- Regressions: `internal/agentcfg`, `internal/agent/profile_test.go`, `internal/config/config_agent_config_test.go`, `internal/daemon/pipeline_agent_profile_test.go`, `TestParseCandidate*`, `TestReplayPinsCandidateModelAndEffortOnTheHarness`, `TestCaptureStripsEveryHarnessPinFromThePinnedConfig`, `TestServedMatchesRequested`, `TestReplayPiModelIdentityComparison`.
