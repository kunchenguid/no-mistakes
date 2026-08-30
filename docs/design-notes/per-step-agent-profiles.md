# Review and fix agent profiles

Status: implemented.

## Configuration

The existing agent-wide model and effort remain the fallback. Two opt-in
invocation profiles can override them:

```yaml
agent_config:
  pi:
    model: openrouter/example/general
    effort: medium
    invocations:
      review:
        model: openrouter/example/strong-reviewer
        effort: high
      review-fix:
        model: openrouter/example/reliable-fixer
```

`review` is the read-only review call. `review-fix` is the separate call that
applies the review findings. They are invocation keys rather than pipeline step
keys because both calls occur inside the Review step, but their model needs are
different. The narrower name also avoids accidentally routing test, lint, or CI
repair calls to the review fixer.

An invocation profile inherits omitted fields independently. In the example,
`review-fix` inherits `effort: medium`. An unconfigured invocation uses the
agent-wide profile. A configuration with no `invocations` block takes the
existing construction path and changes no arguments or runtime behavior.

Only `review` and `review-fix` are accepted initially. Unknown invocation names,
unknown fields, invalid effort values, and knobs the selected harness cannot
express fail during global config loading. The block is global-only because it
selects a model that runs with the operator's credentials.

Raw native pins retain the existing precedence rule. If
`agent_args_override.<agent>` already pins a model or effort knob, that raw flag
wins over both the agent-wide and invocation-specific value for the same knob.

## Runtime attachment

Configuration parsing stores invocation overlays separately from the existing
agent-wide profile. `Config.AgentProfileForInvocation` overlays the requested
purpose and preserves the old `AgentProfileFor` behavior.

The daemon builds the default agent roster through `agent.NewWithOptions`. It
builds an additional roster only when a configured invocation produces a
different effective profile, again through `agent.NewWithOptions`, so
`internal/agentcfg` remains the only harness-specific model and effort mapping.
An invocation router selects that roster from `agent.RunOpts.Purpose`. Empty and
unconfigured purposes use the original default roster.

Fresh and recovered pipelines use the same construction function. Review-fix
session reuse remains role-scoped and every review-fix turn selects the same
effective profile. Closing the router closes the default and routed adapters.

## Extension path

Additional duties can use the same shape after their `RunOpts.Purpose` values
are made complete and stable. Add the new purpose to the config whitelist and
document whether it is an assessment or mutation call. No pipeline step should
translate models into harness flags itself.
