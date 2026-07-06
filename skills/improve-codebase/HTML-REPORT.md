# HTML Audit Report

Render the audit as a self-contained HTML file in the OS temp directory. The required report must work offline with inline CSS, no external network dependencies, no remote fonts, no remote scripts, and no CDN requirements. Keep it visual and complete: the Markdown report is the full transfer surface, and the HTML report must preserve the five standalone lens reports and complete finding registry while making them easier to scan.

## Path

Resolve the temp dir from `$TMPDIR`, falling back to `/tmp` (or `%TEMP%` on Windows), and write:

```txt
<tmpdir>/improve-codebase-audit-<timestamp>.html
```

Open it for the user when feasible. In headless, remote, or restricted environments, skip this step and report that opening was not attempted:

- macOS: `open <path>`
- Linux: `xdg-open <path>`
- Windows: `start <path>`

## Contents

- Header: repo, date, scope, confidence, composite health.
- Audit Preflight: repo root, scope, execution mode, local evidence sources, local change horizon, verification commands, artifact paths, and any context/runtime limits.
- Lens execution summary: the five independent specialist lens passes completed, partial-low-confidence with missing lenses named, or a blocked note if five independent passes were unavailable.
- Five complete lens sections: Module Topology and File-Size Balance, Deepening, Operational Mechanics, Code Health and Cleanup, and Change-Set Quality.
- Module Topology and File-Size Balance report with oversized split suspects, undersized merge suspects, related-file directory clusters, workspace-folder structure, hierarchy findings, and package/home recommendations.
- Code Health prove-or-delete ledger for legacy/dead/unnecessary/speculative/compatibility-smell/over-engineered production and test code, including disposition, proof/evidence, and validation protection.
- Complete Finding Registry table: one row per finding, no suppression, including cleanup disposition/proof fields for ledger-backed findings.
- Cross-Lens Relationship Map: duplicate, overlap, support, contradiction, same-root-cause, and same-remediation clusters.
- Static facets over the full ledger (`static facets`):
  - by lens
  - by file/module
  - by related-file directory cluster
  - by severity
  - by confidence
  - by evidence grade
  - by safety class
  - by root-cause/remediation cluster
  - by validation command
- Synthesized findings table: priority, recommendation, lens IDs, health risk, safety class. This is navigation over the registry, not a replacement for it.
- Module dependency graph when honest enough to draw.
- Candidate cards with before/after visuals for deepening or shared-mechanics changes.
- Test/debt panel that links back to every relevant Code Health finding ID.
- Remediation paths, deferred/watchlist ledger, and implementation backlog.
- Not Reviewed / Continuation Ledger when material scope could not be completed, with omitted scope, reason, risk, and next validation step.
- Final Validation summary: lens report presence, registry completeness, synthesis traceability, relationship IDs, deferred blockers/triggers, artifact write/open status, and any confidence-impacting gaps.
- Status views must preserve strict status semantics: active and safe-cleanup recommendations stay visible even when broad, while deferred/watchlist entries include concrete evidence, a genuine non-actionable-yet blocker, and a concrete revisit trigger.

## Style

- Plain, editorial, navigable.
- Use glossary terms from [LANGUAGE.md](LANGUAGE.md).
- Prefer diagrams and tables for navigation, but do not drop lens findings for visual brevity.
- Do not create an app and do not require JavaScript. Facets must be static anchors, grouped tables, or index sections over the full ledger.
- Do not use CDNs, remote fonts, remote scripts, or network access for the required report.
