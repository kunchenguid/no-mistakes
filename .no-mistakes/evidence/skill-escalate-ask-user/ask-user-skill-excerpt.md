# Ask-user Skill Escalation Evidence

Source surface: `skills/no-mistakes/SKILL.md`.

This excerpt is the generated agent skill text an end user-facing driving agent reads when a gate contains `ask-user` findings.

```markdown
## Escalate `ask-user` findings

A gate whose findings are all `auto-fix` or `no-op` is safe to drive on your
own judgment: fix or approve as appropriate. But a finding marked
`ask-user` is a decision that belongs to the user, not you - the pipeline
flagged it because it challenges their deliberate intent or changes product
behavior. Do not approve, fix, or skip it on your own. Instead, stop and bring
it to the user before you respond:

- Relay each `ask-user` finding to them as the pipeline wrote it - its
  `id`, `file`, and full `description` verbatim. Do not paraphrase,
  summarize away the detail, or pre-judge the answer.
- Ask how they want to proceed, then translate their decision into the matching
  `respond` call: `--action fix` (pass their guidance through
  `--instructions`), `--action approve`, or `--action skip`.
```
