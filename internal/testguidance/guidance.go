// Package testguidance owns the default test-quality rule rendered into
// no-mistakes agents that can write, repair, or review tests.
package testguidance

// Rule is the shared default for writing or repairing tests. It is deliberately
// concrete about source-content-only assertions and circular oracles so agents
// cannot mistake implementation restatements for evidence of correct behavior.
const Rule = `
## Test-quality rule

Never add a test whose only evidence is that it opens, reads, greps, parses, or
snapshots implementation source code and finds or omits particular strings,
tokens, lines, commands, function names, prompt phrases, regex matches, AST
shapes, or incidental snapshots. That does not prove behavior: matching text
can be dead or commented out, and a behavior-preserving refactor can change it.

Instead execute a public or executable interface and assert observable behavior,
state, output, side effects, and failure modes. For machine-consumed declarative
artifacts such as workflow YAML, JSON, policy, .gitignore, or generated
configuration, invoke the real consumer when feasible or parse into a typed or
normalized semantic model and assert meaning. A raw substring or regex over the
file is still the anti-pattern.

Reading a file is legitimate when the file is itself generated public output, a
serialized protocol, persisted state, an intentional snapshot, or another
explicitly owned text or byte contract. Name that contract, and do not use its
contents as a proxy that unrelated code works. A natural-language prompt or
instruction is not proven effective because its source contains a sentence.
Deterministic CI may test the final emitted prompt delivered to an agent as an
intentional generated interface; model interpretation belongs in
development-only evaluation, not live-LLM CI.

Use an independent oracle: the expected result must come from somewhere other
than the code under test, such as a specification, worked example, published
constant, external contract, or independently justified property. Name the
public behavior, the oracle's source, and a plausible wrong behavior the test
would reject. Do not only check your own mocks, compare a result to itself, or
copy the implementation's expected-value rule: a shared mistake can pass both
sides. External-boundary mocks, constants, snapshots, and computed expectations
remain legitimate when they check an independent contract.

For a regression, reproduce the reported failure when feasible: the test should
fail before the fix and pass after it.
`

// ReviewerAction adds the review-only enforcement and scope boundary to Rule.
const ReviewerAction = `
Reviewer action: Flag every newly added source-content-only assertion. Require
the author to remove or semantically refine a same-pattern test encountered
directly within the accepted change's scope, but do not turn an ordinary change
into an unrelated repository-wide cleanup.
Within that same scope, also flag newly added or changed tests that lack an
independent oracle: they only check their own mocks, compare a result to itself,
or copy the implementation's expected-value rule without an independent
contract. Name the public behavior, the oracle's source, and a plausible wrong
behavior an in-scope replacement would reject.
`

// LateRepairPrompt prepends Rule only to centrally classified late-repair
// phases that may need to change tests. The caller supplies the phase's own
// prompt so repair roles retain their focused task instructions.
func LateRepairPrompt(phase, prompt string) string {
	switch phase {
	case "ci", "rebase":
		return Rule + "\n" + prompt
	default:
		return prompt
	}
}
