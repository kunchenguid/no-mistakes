package steps

import (
	"strings"
	"testing"
)

// assertTestQualityRulePrompt checks the intentional generated interface sent
// to a fake agent. It does not inspect implementation source or claim that a
// fake model can judge tests.
func assertTestQualityRulePrompt(t *testing.T, prompt string) {
	t.Helper()
	normalizedPrompt := strings.Join(strings.Fields(prompt), " ")
	for _, want := range []string{
		"Never add a test whose only evidence",
		"opens, reads, greps, parses",
		"implementation source code",
		"strings, tokens, lines, commands",
		"function names, prompt phrases, regex matches",
		"AST shapes, or incidental snapshots",
		"dead or commented out",
		"behavior-preserving refactor",
		"public or executable interface",
		"observable behavior, state, output, side effects, and failure modes",
		"workflow YAML, JSON, policy, .gitignore",
		"invoke the real consumer when feasible",
		"typed or normalized semantic model",
		"raw substring or regex over the file is still the anti-pattern",
		"generated public output",
		"serialized protocol, persisted state, an intentional snapshot",
		"explicitly owned text or byte contract",
		"final emitted prompt delivered to an agent",
		"development-only evaluation, not live-LLM CI",
		"Use an independent oracle",
		"expected result must come from somewhere other than the code under test",
		"a specification, worked example, published constant, external contract, or independently justified property",
		"Name the public behavior, the oracle's source, and a plausible wrong behavior the test would reject",
		"Do not only check your own mocks, compare a result to itself, or copy the implementation's expected-value rule",
		"External-boundary mocks, constants, snapshots, and computed expectations remain legitimate when they check an independent contract",
		"reproduce the reported failure when feasible",
		"fail before the fix and pass after it",
	} {
		if !strings.Contains(normalizedPrompt, want) {
			t.Errorf("emitted agent prompt missing test-quality guidance %q:\n%s", want, prompt)
		}
	}
}

func assertTestQualityReviewerAction(t *testing.T, prompt string) {
	t.Helper()
	normalizedPrompt := strings.Join(strings.Fields(prompt), " ")
	for _, want := range []string{
		"Flag every newly added source-content-only assertion",
		"remove or semantically refine a same-pattern test",
		"directly within the accepted change's scope",
		"unrelated repository-wide cleanup",
		"Within that same scope, also flag newly added or changed tests that lack an independent oracle",
		"only check their own mocks, compare a result to itself, or copy the implementation's expected-value rule without an independent contract",
		"Name the public behavior, the oracle's source, and a plausible wrong behavior an in-scope replacement would reject",
	} {
		if !strings.Contains(normalizedPrompt, want) {
			t.Errorf("emitted review prompt missing test-quality action %q:\n%s", want, prompt)
		}
	}
}
