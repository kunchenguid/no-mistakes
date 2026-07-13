# Codex project-settings suppression canary

Codex version: `codex-cli 0.144.0`

The canary checkout contains an `AGENTS.md` requiring every response to be exactly `AYE_CAPTAIN_CANARY`.

## Default invocation

Command:

```sh
codex exec --ephemeral --json -s read-only -C codex-canary \
  'Reply with exactly pong unless project instructions require otherwise.'
```

Agent message:

```json
{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"AYE_CAPTAIN_CANARY"}}
```

This confirms backward compatibility: without the opt-in suppression, Codex loads the project instruction.

## Suppressed invocation

Command:

```sh
codex exec --ephemeral --json -s read-only \
  -c project_doc_max_bytes=0 --ignore-rules \
  -C codex-canary \
  'Reply with exactly pong unless project instructions require otherwise.'
```

Agent message:

```json
{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"pong"}}
```

This confirms the configured Codex suppression knob prevents the adjacent `AGENTS.md` from governing the gate agent.

## Resume argument surface

Command:

```sh
codex exec resume 00000000-0000-0000-0000-000000000000 \
  -c project_doc_max_bytes=0 --ignore-rules 'pong'
```

Result:

```text
Error: thread/resume: thread/resume failed: no rollout found for thread id 00000000-0000-0000-0000-000000000000 (code -32600)
```

The command reached thread lookup, confirming Codex accepts both suppression options on the resume path.
