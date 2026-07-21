# Oversized Claude and configured-command validation

Validated target: `723b7c32457f6b98db06e069fc378c3fd1b8ab49`

## Direct Claude transport

The focused race-enabled adapter test exercised both a cold call and a resumed session with the same prompt:

```text
prompt bytes: 2,097,188
prompt SHA-256: cb8ec8c685522dc5c6abf9b66a7eae8775d9d9e9db6e890a9371b6060bd746d1
stdin EOF observed: yes
cold argv contains prompt marker: no
resumed argv contains prompt marker: no
resumed session flag preserved: --resume session-123
fixed flags/schema/settings preserved: yes
```

The same focused run exercised a child that exits without reading the 2 MiB stdin payload, cancellation while stdin is blocked, and a clean-exit child with a grandchild holding inherited pipes. Every case returned within its bound, and the process-tree test verified that the grandchild did not survive.

## Test and Lint through AXI

The real E2E harness built and ran the product binary, pushed separate Test and Lint branches through the daemon, waited for each configured command failure to park, invoked `no-mistakes axi status`, then submitted:

```text
no-mistakes axi respond --action fix --findings test-1
no-mistakes axi respond --action fix --findings lint-1
```

Both steps reached `fix_review`. For each 2,097,243-byte configured-command output, the checks directly observed:

```text
bounded findings: HEAD_MARKER context line ... TAIL_MARKER 最后的错误🙂
original byte count: 2097243
omitted byte count: 2032219
full-log pointer: no-mistakes axi logs --step <test|lint> --full
omitted middle marker in findings/repair prompt: no
omitted middle marker in authoritative step log: yes
authoritative step log size: at least 2 MiB
AXI scanner overflow: no
argv size failure during fix transition: no
```

No screenshot was produced because this change has no rendered UI surface. The end-user surface is the CLI and daemon pipeline, which the E2E harness exercised directly.
