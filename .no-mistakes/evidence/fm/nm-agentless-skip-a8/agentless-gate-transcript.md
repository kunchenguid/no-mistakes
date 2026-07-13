# Agentless gate transcript

This manual verification used the target `no-mistakes` binary against an isolated local Git repository and daemon.
`PATH` contained only the system Git tools, with explicit nonexistent paths used where needed to guarantee that no runnable pipeline agent existed.

## Doctor reports that the configured native runner cannot validate

Configuration: `agent: claude`.

```text
$ no-mistakes doctor
  System
  ✓ git             git version 2.50.1 (Apple Git-155)
  – gh              not found (optional, needed for PR/CI)
  – az              not found (optional, needed for Azure DevOps PR/CI)
  ✓ data directory  …/.nm
  ✓ database        ok
  ✓ daemon          running

  Agents
  – claude          not found
  – codex           not found
  – rovodev         not found
  – opencode        not found
  – pi              not found
  – copilot         not found
  – acpx            not found
  ✗ gate validation  no runnable agent found for configured agent claude (looked for: claude); the gate cannot validate without an agent; install a supported native agent, choose an available agent in ~/.no-mistakes/config.yaml, or configure agent: acp:<target> with acpx installed

  some checks failed
```

## Explicit native agent fails before any validation step

```text
$ no-mistakes axi run --intent 'Verify unavailable agent blocks all validation work'
run: failed
run:
  id: "01KX4TA5HPSTR2E3HBZN4BTS8D"
  branch: feature/agentless
  status: failed
  head: c753da8d
  findings: none
  steps[0]:
outcome: failed
error: "no runnable agent found for configured agent claude (looked for: claude); the gate cannot validate without an agent; install a supported native agent, choose an available agent in ~/.no-mistakes/config.yaml, or configure agent: acp:<target> with acpx installed"
```

## Automatic selection fails before any validation step

Configuration: `agent: auto`, with every supported native runner overridden to a nonexistent `/no-runner/...` path.

```text
$ no-mistakes axi run --intent 'Verify automatic agent selection blocks all validation work when no runner is installed'
run: failed
run:
  id: "01KX4TCSJHZ4P7RPANSD4401ET"
  branch: feature/agentless
  status: failed
  head: dac55cf5
  findings: none
  steps[0]:
outcome: failed
error: "no runnable agent found for configured agent auto (looked for: /no-runner/claude, /no-runner/codex, /no-runner/opencode, /no-runner/acli, /no-runner/pi, /no-runner/copilot); the gate cannot validate without an agent; install a supported native agent, choose an available agent in ~/.no-mistakes/config.yaml, or configure agent: acp:<target> with acpx installed"
```

## ACP configuration fails before any validation step when the bridge is missing

Configuration: `agent: acp:gemini` and `acpx_path: /no-runner/acpx`.

```text
$ no-mistakes axi run --intent 'Verify a missing ACP bridge blocks all validation work'
run: failed
run:
  id: "01KX4TDE07D96QXPA83G03D46P"
  branch: feature/agentless
  status: failed
  head: e1e1b916
  findings: none
  steps[0]:
outcome: failed
error: "no runnable agent found for configured agent acp:gemini (looked for: /no-runner/acpx); the gate cannot validate without an agent; install a supported native agent, choose an available agent in ~/.no-mistakes/config.yaml, or configure agent: acp:<target> with acpx installed"
```
