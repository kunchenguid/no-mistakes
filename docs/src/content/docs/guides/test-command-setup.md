---
title: Fix missing tools in Test
description: Resolve configured Test commands that cannot find or execute a tool.
---

Run worktrees need their own dependencies. An installation in your working checkout,
such as `node_modules`, may be absent from the isolated worktree used by the gate.

When a configured Test command or machine-local additional Test check exits with
126 or 127, Test keeps the failed command, output and exit code. In an
operator-attended run, it parks for a human decision. Exit 126 conventionally
means a command could not execute. Exit 127 conventionally means a command was
not found. Test uses these exit codes,
not package-manager names or text matching. A wrapper that uses either code for
another reason also parks for inspection in an operator-attended run.

The evidence agent still gathers scenarios. A successful scenario does not clear
the failed configured check. In an operator-attended run, Test starts no automatic
code-fix round while the setup failure remains. [`axi run --yes`](/no-mistakes/reference/cli/#no-mistakes-axi-run)
and [TUI yolo mode](/no-mistakes/guides/tui/) retain their standing
consent and may still run one fix turn and approve the resulting fix-review gate.
Other nonzero exits retain the existing repair flow.

Check the command's output, executable permissions and `PATH`. If the missing tool
comes from project dependencies, declare their installation in `commands.prepare`
on the repository's trusted default branch. For example:

```yaml
commands:
  prepare: npm ci --prefer-offline --no-audit --no-fund
  test: npm run test:checkout
```

Use the installation command and focused check appropriate for your project.
`commands.prepare` runs before configured checks and its dependencies remain
available for later configured commands in the same run worktree. Test agents
are instructed to report missing setup rather than repeatedly installing and
removing dependencies as transient test artifacts.

After fixing setup, re-run validation. Approving the parked step is still an
approval over a failed configured check, subject to the existing Test override
policy. It does not turn the original failure into a pass.
