# TUI Design System

Design direction for the no-mistakes TUI. Follow these primitives consistently across all screens.

## Color Roles

ANSI-only palette (1-8) so colors follow the user's terminal theme.

| Role | Color | Constant | Use |
|------|-------|----------|-----|
| Primary action/focus | blue | `4` | running states, interactive elements |
| Success | green | `2` | completed, additions |
| Warning/attention | yellow | `3` | awaiting states, warnings |
| Danger | red | `1` | failed, errors, deletions |
| Muted/secondary | bright black | `8` | metadata, durations, hints, borders |
| Accent | cyan | `6` | hunk headers, labels, section titles |

## Typography Scale

Three levels, used consistently:

- **Section title**: Bold + cyan. Used for section headers ("Pipeline", "Findings", "Diff").
- **Content**: Default style. Step labels, finding descriptions, diff lines.
- **Meta**: Dim (bright black). Durations, file references, counts, hints, footer.

## Boxed Sections

Every distinct content region gets a rounded-border box:

```
╭─ Section Title ──────────────────────────╮
│  content here                            │
╰──────────────────────────────────────────╯
```

- Border: `lipgloss.RoundedBorder()`, color = bright black (8)
- Padding: 0 vertical, 1 horizontal (content never touches the border edge)
- Title: rendered into the top border line
- Width: fill available terminal width

## Pipeline Connector

Steps connected by a vertical line to convey flow:

```
  ✓ Review  1.2s
  │
  ⏸ Test - awaiting approval
  │
  ○ Lint
  │
  ○ Push
```

Connector `│` in bright black. The active/awaiting step visually breaks the flow.

## Action Bar

Keybinding hints pulled out of content flow into a distinct horizontal bar:

```
 a approve  f fix  s skip  x abort  d diff │ ␣ toggle  A all  N none
```

- Keys rendered in bold
- 2-space separation between actions
- `│` separator between primary actions and selection actions
- Sits below the pipeline box, above findings/diff

## Gutter System

Fixed-width left column for icons, checkboxes, and cursor. Content never shifts when selection state changes.

```
  > [x] ● src/handler.go:42
           Missing error check on db.Close()

    [x] ▲ src/config.go:17
           Unused import "fmt"
```

- Cursor (`>`) in its own column
- Checkbox in its own column
- Severity icon in its own column
- Description indented to clear the gutter

## Diff View

Stats badge in the section, scroll indicator integrated into the bottom border:

```
╭─ Diff ───────────────────────────────────╮
│  3 files  +42  -17                       │
│                                          │
│  diff --git a/foo.go b/foo.go            │
│  ...                                     │
╰──── ↓ 23 more lines (j/k) ─────────────╯
```

## Log Tail

Dim content inside a subtle frame:

```
╭─ Log ────────────────────────────────────╮
│  running go test ./...                   │
│  PASS: TestFoo (0.3s)                    │
│  FAIL: TestBar (0.1s)                    │
╰──────────────────────────────────────────╯
```

## Footer

Minimal dim hint at the very bottom, outside all boxes:

```
  q quit  ? help
```

While the pipeline is still running, the footer shows `q detach` instead of `q quit`.

After a failed or cancelled run, it also shows `r rerun`.

## Spacing Rules

- 1-char horizontal padding inside all boxes
- 1 blank line between sections, never more than 1
- No trailing blank lines inside boxes

## Step Status Icons

| Status | Icon | Color |
|--------|------|-------|
| Pending | `○` | bright black |
| Running | `⠋` (animated) | blue |
| Awaiting approval | `⏸` | yellow |
| Fix review | `⏸` | yellow |
| Completed | `✓` | green |
| Skipped | `–` | bright black |
| Failed | `✗` | red |

## Severity Icons

| Severity | Icon | Color |
|----------|------|-------|
| Error | `E` | red |
| Warning | `W` | yellow |
| Info | `I` | blue |
