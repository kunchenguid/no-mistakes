# Review and fix invocation routes

Status: implemented.

## Configuration

Review and repair can select different harnesses as well as different models:

```yaml
agent: codex

agent_config:
  codex:
    effort: low
  pi:
    effort: medium

invocations:
  review:
    agent: pi
    model: openrouter/z-ai/glm-5.3-flash
    effort: high
  review-fix:
    agent: codex
    model: gpt-5.6-sol
    effort: medium
```

`review` is the read-only assessment call. `review-fix` is the separate call
that applies accepted findings. These are invocation keys because both calls
occur inside the Review step, but they have different responsibilities.

The `invocations` block is top-level and global-only. Nesting it under
`agent_config.<agent>` would make the parent harness authoritative and could
only change model or effort within that harness. It could not express the
required Pi reviewer and Codex fixer. Repository config cannot select these
operator-credentialed processes or models.

Each route requires `agent`. Omitted `model` and `effort` fields inherit
independently from `agent_config.<selected-agent>`. An unconfigured invocation
uses the existing `agent` selection unchanged, including its ordered fallback
list. With no `invocations` block, construction and runtime behavior stay on
the original path.

An explicit route is deterministic. It does not enter or fall back to the
default agent list. Missing binaries and unsupported ACP commands fail during
pipeline setup before a step starts. Unknown agents, invalid fields, invalid
effort values, and model or effort values the selected harness cannot express
fail while loading global config.

Authentication is the one setup property no-mistakes cannot prove generically
without making a provider request. The supported harnesses do not expose one
common, side-effect-free authentication probe, and a successful local token
check would not prove the selected provider and model are authorized. If an
explicit route is not authenticated, its adapter error is returned from that
invocation and the default fallback list is not used. This prevents a
configured review or repair duty from silently running under another identity.

Raw native pins keep their existing precedence. The route uses
`agent_args_override.<selected-agent>`, and a raw model or effort argument wins
over both `agent_config` and the route for the same knob. Unpinned knobs still
come from the route's effective profile.

## Runtime attachment

Global config resolves each route to an agent plus an effective
`internal/agentcfg.Profile`. The daemon verifies every named harness during
agent resolution, then constructs it through `agent.NewWithOptions`. This keeps
`internal/agentcfg` as the only harness-specific model and effort mapping and
preserves raw-argument precedence.

An invocation router selects the explicit adapter from
`agent.RunOpts.Purpose`. Empty and unconfigured purposes use the original
default adapter or fallback roster. Routed results carry the selected provider
identity so role-scoped session reuse resumes only with the harness that minted
the session. Fresh and recovered pipelines use the same construction path, and
closing the router closes both the default roster and every explicit route.

## Extension path

Additional duties can use the same shape after their `RunOpts.Purpose` values
are complete and stable. Add each new purpose to the config whitelist and
document whether it is an assessment or mutation call. Pipeline steps must not
translate models into harness flags themselves.
