# Skill Intent Guidance Evidence

This artifact captures the reviewer-visible guidance now present in the generated `no-mistakes` skill.

Source of truth checked: `internal/skill/skill.go`.
Generated skill checked: `skills/no-mistakes/SKILL.md`.
Dogfooded install copy checked: `.agents/skills/no-mistakes/SKILL.md`.

## Rendered Skill Excerpt

```md
## Intent is required

When you start a run you must pass `--intent`: **what the user set out to
accomplish** - the goal or request behind this work, in their terms. This is not
a description of the diff or the files you changed; it is the objective the
change is meant to achieve. You know it from the conversation, so pass it
directly - no-mistakes uses it verbatim instead of inferring it from local agent
transcripts (slower and flakier).

Err on the side of completeness, not brevity. The review step uses `--intent`
to tell a deliberate decision apart from a mistake, so a thin one-line summary
makes it flag things the user already chose. Capture the nuance: the user's
goal, the specific decisions and tradeoffs they made along the way, any
constraints or approaches they ruled in or out, and anything they explicitly
asked for that might otherwise look surprising in the diff. A few sentences to a
short paragraph is normal - write down what you learned from the conversation
that a reviewer reading only the diff would not know.
```

## Verification Commands

```sh
go test ./internal/skill -run 'TestMarkdownFrontmatter|TestInstallWritesBothPaths|TestInstallIsIdempotent' -v
go run ./cmd/genskill --check
```

## Verification Result

The targeted skill tests passed, including installation of the generated `SKILL.md` into both supported agent skill paths.
The generator check reported `skills/no-mistakes/SKILL.md` is up to date, demonstrating that the committed generated skill matches `internal/skill/skill.go`.
