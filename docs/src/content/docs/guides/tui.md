---
title: TUI
description: Terminal UI layout, keybindings, and approval workflow.
---

The TUI is how you interact with running pipelines. Launch it with `no-mistakes` or `no-mistakes attach`.

## Layout

The layout adapts to terminal width:

- **Wide (100+ columns):** pipeline box on the left, findings/log/diff panel on the right, side by side
- **Narrow (<100 columns):** pipeline box stacked above the findings panel

### Pipeline box

Shows the branch name and run status in the header, followed by each step:

```
  feature/login-fix  running
  ────────────────────────────
  ✓ Rebase            320ms
  │
  ⏸ Review     - awaiting approval
  │
  ○ Test
  │
  ○ Lint
  │
  ○ Push
  │
  ○ PR
  │
  ○ CI
```

Step status icons:

| Icon | Status |
|---|---|
| `○` | Pending |
| (spinner) | Running / Fixing |
| `⏸` | Awaiting approval / Fix review |
| `✓` | Completed |
| `–` | Skipped |
| `✗` | Failed |

Completed steps show their duration. Connectors (`│`) between steps are hidden when the terminal height is under 30 lines.

### Findings panel

When a step pauses for approval, the findings panel shows structured results:

```
  Risk: MEDIUM
  Potential null pointer in error path

  > [x] E  src/handler.go:42
         Missing nil check before dereferencing resp.Body
    [x] W  src/handler.go:78
         Error string should not be capitalized
    [ ] I  src/handler.go:95
         Consider extracting this into a helper function
```

- Severity icons: `E` (error, red), `W` (warning, yellow), `I` (info, blue)
- Checkboxes: `[x]` (selected, green), `[ ]` (deselected, dim)
- Blue `>` marks the focused finding
- Bottom hint shows `↑ N above / ↓ N more below (j/k)` when scrolling, or `(j/k)` whenever there are multiple findings

### Diff panel

After a fix cycle, press `d` to toggle the diff view:

- Stats header showing files changed, additions, and deletions
- Syntax-colored unified diff with line number gutter
- Finding context line showing which finding you're viewing
- Scroll position in the box title: `Diff (45/312)`

### Log tail

During running steps, shows streaming agent output. Lines starting with `PASS` are green, `FAIL` are red, everything else is dim.

On narrow terminals, the log panel expands to fill the remaining vertical space below the pipeline box instead of staying at the compact fixed height used in shorter layouts.

### Footer

The footer shows detach/help actions and, when `no-mistakes attach` has a cached newer release available, a right-aligned `<version> available` indicator. That update indicator stays visible after reruns in the same TUI session.

## Keybindings

### Navigation

| Key | Action |
|---|---|
| `j` / `k` | Scroll down / up |
| `g` / `G` | Jump to start / end |
| `Ctrl+d` / `Ctrl+u` | Half-page down / up |
| `n` / `p` | Next / previous finding |

### Actions (when a step is awaiting approval)

| Key | Action |
|---|---|
| `a` | Approve - continue to next step |
| `f` | Fix - send selected findings to agent for fixing |
| `s` | Skip - skip this step and continue |
| `x` | Abort - press twice to confirm (first press shows warning) |
| `o` | Open PR URL in browser (when available) |

### Selection

| Key | Action |
|---|---|
| `space` | Toggle current finding |
| `A` | Select all findings |
| `N` | Deselect all findings |

### View

| Key | Action |
|---|---|
| `d` | Toggle diff view (after fix cycle) |
| `esc` | Exit diff view back to findings |
| `?` | Toggle help overlay |
| `r` | Start a rerun after a failed or cancelled run |
| `q` | Detach from TUI (or quit if run is done) |

In diff view, `n`/`p` jumps the viewport to the file and line of the next/previous finding.

## Action bar

The action bar appears below the pipeline box when a step is awaiting approval:

```
Review awaiting action:
 a approve  f fix (3/5)  s skip  x abort  d diff
 [space] toggle  A all  N none
```

The `f fix (3/5)` label shows how many findings are selected out of the total.

## Outcome banner

When a run finishes, a one-line banner appears:

- `✓ Pipeline passed  4.2s` (green) - the run finished successfully, even if later steps were auto-skipped
- `✗ Review failed  1.8s` (red) - names the failing step
- `✗ Pipeline cancelled` (red) - user aborted

After a failed or cancelled run, press `r` to start a rerun. The TUI switches to the new run automatically.

## Detaching

Press `q` to detach from the TUI. The pipeline continues running in the background. Run `no-mistakes` again to reattach.

If the run is already finished, `q` exits the TUI.
