package steps

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func TestDecodePRContent_KeepsLegacyTitleBodyWhenOverviewFieldsAreMalformed(t *testing.T) {
	t.Parallel()

	content, err := decodePRContent(json.RawMessage(`{
		"title":"feat(pipeline): make PR descriptions concise",
		"body":"## What Changed\n\n- Render concise PR descriptions.",
		"intent":{"wrong":"type"},
		"acceptance_criteria":"wrong type"
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if content.Title != "feat(pipeline): make PR descriptions concise" || !strings.Contains(content.Body, "Render concise PR descriptions") {
		t.Fatalf("legacy title/body were not retained: %#v", content)
	}
	if content.Intent != "" || len(content.AcceptanceCriteria) != 0 {
		t.Fatalf("malformed overview fields must be ignored independently: %#v", content)
	}
}

func TestRenderConcisePRNarrative_GitHubUsesBalancedNestedDetailsAndEscrowsIntent(t *testing.T) {
	t.Parallel()

	sctx := &pipeline.StepContext{
		UserIntent:   "Make generated PR descriptions concise.\n\nAcceptance criteria:\n- AC1 — Keep compatibility: Existing providers continue to render safely.\n- AC2 — Keep evidence useful: Show screenshots when available.",
		IntentSource: db.RunIntentSourceAgent,
	}
	content := prContent{
		Intent: "Make generated PR descriptions concise without hiding reviewer-critical context.",
		AcceptanceCriteria: []prAcceptanceCriterion{
			{Summary: "Keep provider compatibility </summary>", Details: "Do not break <details> or the pipeline attestation."},
			{Summary: "Show useful visual evidence", Details: "Render trusted screenshots when available."},
		},
		Body: "## What Changed\n\n- Add concise rendering.\n- Add nested acceptance criteria.\n- Add visual evidence.\n- This fourth bullet must be omitted.\n\n## Extra\n\nnot allowed",
	}

	got := renderConcisePRNarrative(content, sctx, scm.ProviderGitHub, "M\tinternal/pipeline/steps/pr.go")
	for _, want := range []string{
		"## Intent",
		"<summary><strong>Acceptance criteria</strong>",
		"<strong>AC1</strong>",
		"Keep provider compatibility &lt;/summary&gt;",
		"<strong>Complete acceptance context</strong>",
		"Make generated PR descriptions concise.",
		"## What Changed",
		"- Add concise rendering.",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("rendered narrative missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "fourth bullet") || strings.Contains(got, "## Extra") {
		t.Fatalf("What Changed was not normalized to three bullets:\n%s", got)
	}
	if strings.Count(got, "<details>") != strings.Count(got, "</details>") {
		t.Fatalf("nested disclosure tags are unbalanced:\n%s", got)
	}
}

func TestRenderConcisePRNarrative_BitbucketUsesBoundedPlainMarkdown(t *testing.T) {
	t.Parallel()

	sctx := &pipeline.StepContext{UserIntent: "Keep the change concise and compatible.", IntentSource: db.RunIntentSourceAgent}
	content := prContent{
		Intent:             "Keep the change concise.",
		AcceptanceCriteria: []prAcceptanceCriterion{{Summary: "Keep compatibility", Details: "No raw HTML."}},
		Body:               "## What Changed\n\n- Add concise output.",
	}

	got := renderConcisePRNarrative(content, sctx, scm.ProviderBitbucket, "M\tinternal/pipeline/steps/pr.go")
	if strings.Contains(got, "<details>") || strings.Contains(got, "<summary>") {
		t.Fatalf("Bitbucket narrative contains raw disclosure HTML:\n%s", got)
	}
	for _, want := range []string{"## Intent", "### Acceptance criteria", "- **AC1 — Keep compatibility:** No raw HTML.", "### Complete acceptance context", "## What Changed"} {
		if !strings.Contains(got, want) {
			t.Fatalf("Bitbucket narrative missing %q:\n%s", want, got)
		}
	}
}

func TestRenderConcisePRNarrative_FallsBackWithoutLosingValidLegacyBody(t *testing.T) {
	t.Parallel()

	sctx := &pipeline.StepContext{
		UserIntent:   "Add a concise PR renderer.\n\nAcceptance criteria:\n- AC1 — Keep titles: Conventional titles survive.\n- AC2 — Keep pipeline: Pipeline output is unchanged.",
		IntentSource: db.RunIntentSourceAgent,
	}
	content := prContent{Body: "## What Changed\n\n- Add the renderer."}

	got := renderConcisePRNarrative(content, sctx, scm.ProviderGitHub, "M\tinternal/pipeline/steps/pr.go")
	for _, want := range []string{"Add a concise PR renderer.", "Keep titles", "Conventional titles survive.", "Keep pipeline", "Pipeline output is unchanged.", "Complete acceptance context"} {
		if !strings.Contains(got, want) {
			t.Fatalf("fallback narrative missing %q:\n%s", want, got)
		}
	}
}

func TestAssembleConcisePRBody_AzureCapKeepsBalancedStructureAndHonestContextMarker(t *testing.T) {
	t.Parallel()

	sctx := &pipeline.StepContext{UserIntent: strings.Repeat("A material acceptance constraint must remain reviewable. ", 300), IntentSource: db.RunIntentSourceAgent}
	narrative := renderConcisePRNarrative(prContent{
		Intent: "Keep the generated pull request concise and reviewable.",
		AcceptanceCriteria: []prAcceptanceCriterion{{
			Summary: "Preserve every material requirement",
			Details: strings.Repeat("This detailed requirement has important edge cases. ", 80),
		}},
		Body: "## What Changed\n\n- Render concise pull request descriptions.",
	}, sctx, scm.ProviderAzureDevOps, "M\tinternal/pipeline/steps/pr.go")
	pipelineMD := pipelineMarkdownForTest("latest pipeline detail")
	got := assemblePRBodyWithoutIntent(narrative, "⚠️ Medium: generated description behavior changed", "## Testing\n\nverbose", pipelineMD, scm.MaxPRBodyChars(scm.ProviderAzureDevOps))

	if scm.PRBodyLen(got) > scm.MaxPRBodyChars(scm.ProviderAzureDevOps) {
		t.Fatalf("Azure body exceeded cap: %d\n%s", scm.PRBodyLen(got), got)
	}
	narrativePart := strings.Split(got, "## Pipeline")[0]
	if strings.Count(narrativePart, "<details>") != strings.Count(narrativePart, "</details>") {
		t.Fatalf("capped narrative contains unbalanced details:\n%s", got)
	}
	for _, want := range []string{"## Intent", "## What Changed", "## Pipeline", "Complete acceptance context omitted to fit the provider description limit."} {
		if !strings.Contains(got, want) {
			t.Fatalf("capped body missing %q:\n%s", want, got)
		}
	}
}

func TestRenderConcisePRNarrative_FallbackKeepsHyphenatedWordsInsideCriterionText(t *testing.T) {
	t.Parallel()

	sctx := &pipeline.StepContext{
		UserIntent:   "Keep gates advisory.\n\nAcceptance criteria:\n- AC1 Keep non-blocking gates: parked findings stay parked.\n- AC2 - Keep well-known defaults: unchanged behaviour.",
		IntentSource: db.RunIntentSourceAgent,
	}

	got := renderConcisePRNarrative(prContent{Body: "## What Changed\n\n- Keep gates advisory."}, sctx, scm.ProviderGitHub, "M\tinternal/pipeline/steps/pr.go")
	for _, want := range []string{"Keep non-blocking gates", "parked findings stay parked.", "Keep well-known defaults", "unchanged behaviour."} {
		if !strings.Contains(got, want) {
			t.Fatalf("fallback criterion lost text to a word-internal hyphen, missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "<strong>AC1</strong> — blocking gates") || strings.Contains(got, "<strong>AC2</strong> — known defaults") {
		t.Fatalf("a word-internal hyphen was treated as the ACn label delimiter:\n%s", got)
	}
}

func TestRenderConcisePRNarrative_RedactsHomePathsThatEscapingWouldHideFromTheBoundary(t *testing.T) {
	t.Parallel()

	sctx := &pipeline.StepContext{
		UserIntent:   `Keep the worktree under C:\Users\bob\.no-mistakes\worktrees\run usable.`,
		IntentSource: db.RunIntentSourceAgent,
	}
	content := prContent{
		Intent:             `Evidence is written beneath /Users/dana_lee/src/no-mistakes.`,
		AcceptanceCriteria: []prAcceptanceCriterion{{Summary: "Keep operator paths private", Details: `Never publish C:\Users\bob\.no-mistakes or /Users/dana_lee/src.`}},
		Body:               "## What Changed\n\n- Redact operator paths.",
	}

	for _, provider := range []scm.Provider{scm.ProviderGitHub, scm.ProviderBitbucket} {
		narrative := renderConcisePRNarrative(content, sctx, provider, "M\tinternal/pipeline/steps/pr.go")
		got := redactPRContent(prContent{Body: narrative}).Body
		for _, leak := range []string{"bob", "dana", "lee", "Users"} {
			if strings.Contains(got, leak) {
				t.Fatalf("provider %s published operator identity %q:\n%s", provider, leak, got)
			}
		}
		if !strings.Contains(got, "Evidence is written beneath") {
			t.Fatalf("provider %s lost the surrounding prose:\n%s", provider, got)
		}
	}
}

func TestRenderConcisePRNarrative_FallbackKeepsURLColonsInsideCriterionText(t *testing.T) {
	t.Parallel()

	sctx := &pipeline.StepContext{
		UserIntent:   "Keep documentation current.\n\nAcceptance criteria:\n- AC1 — Docs live at https://example.test/x: keep them current.",
		IntentSource: db.RunIntentSourceAgent,
	}

	got := renderConcisePRNarrative(prContent{Body: "## What Changed\n\n- Update the documentation."}, sctx, scm.ProviderGitHub, "M\tinternal/pipeline/steps/pr.go")
	if !strings.Contains(got, "<summary><strong>AC1</strong> — Docs live at https://example.test/x</summary>") {
		t.Fatalf("fallback criterion summary was split at a URL scheme colon:\n%s", got)
	}
	if !strings.Contains(got, "keep them current.") {
		t.Fatalf("fallback criterion lost its detail text:\n%s", got)
	}
}

func TestRenderConciseRisk_BitbucketDoesNotRepeatTheVisibleSentence(t *testing.T) {
	t.Parallel()

	risk := "Medium: the published description renderer changed. Attestation integrity and redaction are unchanged. Provider caps were re-verified."
	got := renderConciseRisk(risk, scm.ProviderBitbucket)

	if count := strings.Count(got, "the published description renderer changed."); count != 1 {
		t.Fatalf("Bitbucket risk repeated its visible sentence %d times:\n%s", count, got)
	}
	for _, want := range []string{"Attestation integrity and redaction are unchanged.", "Provider caps were re-verified."} {
		if !strings.Contains(got, want) {
			t.Fatalf("Bitbucket risk dropped rationale %q:\n%s", want, got)
		}
	}
}

func TestAssembleConcisePRBody_KeepsTestingWhenOmittingAcceptanceContextMakesRoom(t *testing.T) {
	t.Parallel()

	sctx := &pipeline.StepContext{UserIntent: strings.Repeat("Complete acceptance context sentence. ", 40), IntentSource: db.RunIntentSourceAgent}
	narrative := renderConcisePRNarrative(prContent{
		Intent:             "Keep evidence when context can be shed.",
		AcceptanceCriteria: []prAcceptanceCriterion{{Summary: "Keep Testing above complete context", Details: "Testing outranks the complete acceptance context under a cap."}},
		Body:               "## What Changed\n\n- Shed acceptance context before Testing.",
	}, sctx, scm.ProviderAzureDevOps, "M\tinternal/pipeline/steps/pr.go")
	pipelineMD := pipelineMarkdownForTest("newest pipeline update")
	testingMD := "## Testing\n\nFocused checks passed."
	bodyLimit := 1200

	if scm.PRBodyLen(narrative) <= bodyLimit {
		t.Fatalf("narrative must overflow the cap for this regression, got %d units", scm.PRBodyLen(narrative))
	}
	got := assemblePRBodyWithoutIntent(narrative, "⚠️ Medium: cap behavior", testingMD, pipelineMD, bodyLimit)

	if scm.PRBodyLen(got) > bodyLimit {
		t.Fatalf("capped body exceeded its limit: %d\n%s", scm.PRBodyLen(got), got)
	}
	for _, want := range []string{"Complete acceptance context omitted to fit the provider description limit.", "## Testing", "Focused checks passed.", "newest pipeline update"} {
		if !strings.Contains(got, want) {
			t.Fatalf("capped body dropped %q while budget remained:\n%s", want, got)
		}
	}
}

func TestBuildConcisePRBody_GitHubSafetyCapKeepsNarrativeBalanced(t *testing.T) {
	t.Parallel()

	sctx := &pipeline.StepContext{UserIntent: strings.Repeat("Full acceptance context. ", 6000), IntentSource: db.RunIntentSourceAgent}
	narrative := renderConcisePRNarrative(prContent{
		Intent:             "Keep the body concise.",
		AcceptanceCriteria: []prAcceptanceCriterion{{Summary: "Keep context reviewable", Details: "Details remain hidden."}},
		Body:               "## What Changed\n\n- Bound generated pull request bodies.",
	}, sctx, scm.ProviderGitHub, "M\tinternal/pipeline/steps/pr.go")
	got := buildPRBodyWithoutIntent(narrative, "⚠️ Medium: body assembly changed", "## Testing\n\nverbose", pipelineMarkdownForTest("latest"))

	assertGitHubBodyLimitForTest(t, got)
	narrativePart := strings.Split(got, "## Pipeline")[0]
	if strings.Count(narrativePart, "<details>") != strings.Count(narrativePart, "</details>") {
		t.Fatalf("GitHub-capped narrative contains unbalanced details:\n%s", got)
	}
	if !strings.Contains(got, "Complete acceptance context omitted to fit the provider description limit.") {
		t.Fatalf("GitHub-capped narrative omitted context without the honest marker:\n%s", got)
	}
	if !strings.Contains(got, "## Testing") || !strings.Contains(got, "verbose") {
		t.Fatalf("Testing should return after impossible context is omitted and before older Pipeline history:\n%s", got)
	}
}

func TestBuildConcisePRBody_DropsTestingBeforeCompleteAcceptanceContext(t *testing.T) {
	t.Parallel()

	sctx := &pipeline.StepContext{UserIntent: "The complete acceptance context must survive when only Testing causes overflow.", IntentSource: db.RunIntentSourceAgent}
	narrative := renderConcisePRNarrative(prContent{
		Intent:             "Keep acceptance context reviewable.",
		AcceptanceCriteria: []prAcceptanceCriterion{{Summary: "Keep complete context", Details: "Do not omit it just to retain testing output."}},
		Body:               "## What Changed\n\n- Prioritize acceptance context under body caps.",
	}, sctx, scm.ProviderGitHub, "M\tinternal/pipeline/steps/pr.go")
	got := buildPRBodyWithoutIntent(narrative, "⚠️ Medium: body assembly changed", "## Testing\n\n"+strings.Repeat("large testing output ", 6000), pipelineMarkdownForTest("latest pipeline update"))

	if !strings.Contains(got, "The complete acceptance context must survive") {
		t.Fatalf("complete context was dropped before lower-priority Testing:\n%s", got)
	}
	if strings.Contains(got, "large testing output") {
		t.Fatalf("oversized lower-priority Testing should have been dropped:\n%s", got)
	}
}

func TestAssembleConcisePRBody_ReservesNewestPipelineUpdate(t *testing.T) {
	t.Parallel()

	narrative := "## Intent\n\nKeep the newest pipeline update.\n\n## What Changed\n\n- Reserve pipeline update space."
	got := assemblePRBodyWithoutIntent(narrative+strings.Repeat("\ncontext", 600), "⚠️ Medium: cap behavior", "", pipelineMarkdownForTest("newest pipeline proof"), 1800)
	if !strings.Contains(got, "newest pipeline proof") {
		t.Fatalf("capped body kept the Pipeline heading but lost its newest update:\n%s", got)
	}
}

func TestAssembleConcisePRBody_AzureKeepsTestingBeforeOlderPipelineHistory(t *testing.T) {
	t.Parallel()

	narrative := "## Intent\n\nKeep concise evidence.\n\n## What Changed\n\n- Prioritize Testing over old Pipeline rounds."
	pipelineMD := pipelineMarkdownForTest("old update "+strings.Repeat("x", 3000), "newest update")
	got := assemblePRBodyWithoutIntent(narrative, "⚠️ Medium: cap behavior", "## Testing\n\nFocused checks passed.", pipelineMD, 1800)
	for _, want := range []string{"## Testing", "Focused checks passed.", "newest update"} {
		if !strings.Contains(got, want) {
			t.Fatalf("Azure cap dropped higher-priority %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "old update") {
		t.Fatalf("Azure kept older Pipeline history before Testing:\n%s", got)
	}
}

func TestAssembleConcisePRBody_AzureRetainsNewestOfMultiplePipelineUpdates(t *testing.T) {
	t.Parallel()

	narrative := "## Intent\n\nKeep the newest pipeline update.\n\n## What Changed\n\n- Reserve newest update space." + strings.Repeat("\ncontext", 500)
	pipelineMD := pipelineMarkdownForTest(
		"oldest pipeline update "+strings.Repeat("x", 500),
		"middle pipeline update "+strings.Repeat("y", 500),
		"newest pipeline update "+strings.Repeat("z", 500),
	)
	got := assemblePRBodyWithoutIntent(narrative, "⚠️ Medium: cap behavior", "", pipelineMD, 1800)
	if !strings.Contains(got, "newest pipeline update") {
		t.Fatalf("Azure cap lost the newest update:\n%s", got)
	}
	if strings.Contains(got, "oldest pipeline update") {
		t.Fatalf("Azure cap retained the oldest update before the newest:\n%s", got)
	}
}

func TestAssembleConcisePRBody_AzureDropsAcceptanceDetailBeforeTruncatingFullNewestUpdate(t *testing.T) {
	t.Parallel()

	sctx := &pipeline.StepContext{UserIntent: "Keep the newest pipeline update complete.", IntentSource: db.RunIntentSourceAgent}
	narrative := renderConcisePRNarrative(prContent{
		Intent:             "Prioritize newest pipeline evidence.",
		AcceptanceCriteria: []prAcceptanceCriterion{{Summary: "Keep newest update", Details: strings.Repeat("lower priority acceptance detail ", 80)}},
		Body:               "## What Changed\n\n- Reserve the full newest pipeline update.",
	}, sctx, scm.ProviderAzureDevOps, "M\tpr.go")
	newest := "full newest update " + strings.Repeat("z", 900)
	pipelineMD := pipelineMarkdownForTest("old update "+strings.Repeat("x", 1500), newest)
	got := assemblePRBodyWithoutIntent(narrative, "⚠️ Medium: provider limits", "", pipelineMD, 2200)
	if !strings.Contains(got, newest) {
		t.Fatalf("Azure truncated the newest update before lower-priority acceptance detail:\n%s", got)
	}
	if strings.Contains(got, pipelineLatestUpdateTruncationMarker()) {
		t.Fatalf("Azure emitted a truncated newest update that could fit after detail compaction:\n%s", got)
	}
}

func TestAssembleConcisePRBody_AzureKeepsUnicodeNewestUpdateThatFitsUTF16(t *testing.T) {
	t.Parallel()

	narrative := "## Intent\n\nKeep Unicode pipeline evidence.\n\n## What Changed\n\n- Measure pipeline updates in UTF-16."
	unicodeUpdate := "newest Unicode proof " + strings.Repeat("界", 700)
	pipelineMD := pipelineMarkdownForTest("old update "+strings.Repeat("x", 2500), unicodeUpdate)
	got := assemblePRBodyWithoutIntent(narrative, "⚠️ Medium: provider limits", "", pipelineMD, 2200)
	if !strings.Contains(got, unicodeUpdate) {
		t.Fatalf("Azure omitted a Unicode newest update that fits its UTF-16 budget:\n%s", got)
	}
}

func TestAssembleConcisePRBody_AzureUsesUTF16UnitsForNarrativeFit(t *testing.T) {
	t.Parallel()

	sctx := &pipeline.StepContext{UserIntent: strings.Repeat("界", 900), IntentSource: db.RunIntentSourceAgent}
	narrative := renderConcisePRNarrative(prContent{
		Intent:             "Keep Unicode-safe provider limits.",
		AcceptanceCriteria: []prAcceptanceCriterion{{Summary: "Keep Unicode context", Details: "Do not drop context based on UTF-8 byte length."}},
		Body:               "## What Changed\n\n- Measure Azure descriptions in UTF-16 units.",
	}, sctx, scm.ProviderAzureDevOps, "M\tinternal/pipeline/steps/pr.go")
	got := assemblePRBodyWithoutIntent(narrative, "⚠️ Medium: provider limits", "", pipelineMarkdownForTest("latest"), scm.MaxPRBodyChars(scm.ProviderAzureDevOps))
	if strings.Contains(got, "Complete acceptance context omitted") {
		t.Fatalf("Azure context was prematurely omitted by UTF-8 byte measurement:\n%s", got)
	}
}

func TestAssembleConcisePRBody_PreservesAllACSummariesBeforeDetails(t *testing.T) {
	t.Parallel()

	criteria := make([]prAcceptanceCriterion, 0, 7)
	for i := 1; i <= 7; i++ {
		criteria = append(criteria, prAcceptanceCriterion{Summary: fmt.Sprintf("Criterion summary %d", i), Details: strings.Repeat(fmt.Sprintf("detail-%d ", i), 120)})
	}
	sctx := &pipeline.StepContext{UserIntent: "All seven criterion summaries remain reviewable.", IntentSource: db.RunIntentSourceAgent}
	narrative := renderConcisePRNarrative(prContent{Intent: "Keep all criterion summaries.", AcceptanceCriteria: criteria, Body: "## What Changed\n\n- Compact criterion details."}, sctx, scm.ProviderAzureDevOps, "M\tpr.go")
	got := assemblePRBodyWithoutIntent(narrative, "⚠️ Medium: cap behavior", "", pipelineMarkdownForTest("latest"), 2600)
	for i := 1; i <= 7; i++ {
		if !strings.Contains(got, fmt.Sprintf("Criterion summary %d", i)) {
			t.Fatalf("criterion summary %d was dropped before lower-priority detail:\n%s", i, got)
		}
	}
}

func TestAssembleConcisePRBody_WithoutPipelineKeepsBalancedDisclosuresAtAzureCap(t *testing.T) {
	t.Parallel()

	sctx := &pipeline.StepContext{UserIntent: strings.Repeat("Complete context. ", 800), IntentSource: db.RunIntentSourceAgent}
	narrative := renderConcisePRNarrative(prContent{Intent: "Keep disclosures balanced.", AcceptanceCriteria: []prAcceptanceCriterion{{Summary: "Stay balanced", Details: strings.Repeat("detail ", 200)}}, Body: "## What Changed\n\n- Balance capped disclosures."}, sctx, scm.ProviderAzureDevOps, "M\tpr.go")
	got := assemblePRBodyWithoutIntent(narrative, "⚠️ Medium: cap behavior", "", "", 1400)
	if strings.Count(got, "<details>") != strings.Count(got, "</details>") {
		t.Fatalf("pipeline-unavailable cap path cut a disclosure mid-block:\n%s", got)
	}
	if !strings.Contains(got, "Complete acceptance context omitted to fit the provider description limit.") {
		t.Fatalf("pipeline-unavailable cap path omitted context without the marker:\n%s", got)
	}
}

func TestRenderConcisePRNarrative_BitbucketEscapesStructureAndBoundsCriterionBullet(t *testing.T) {
	t.Parallel()

	sctx := &pipeline.StepContext{UserIntent: "Safe context.\n## injected heading\nInjected setext\n===\n<https://tracker.example/pixel>\nMore context.", IntentSource: db.RunIntentSourceAgent}
	got := renderConcisePRNarrative(prContent{
		Intent:             "Keep Bitbucket structure safe.",
		AcceptanceCriteria: []prAcceptanceCriterion{{Summary: strings.Repeat("**[summary]** ", 30), Details: strings.Repeat("![detail](https://tracker.example/pixel) ", 100)}},
		Body:               "## What Changed\n\n- Escape provider-sensitive text.",
	}, sctx, scm.ProviderBitbucket, "M\tpr.go")
	if strings.Contains(got, "\n## injected heading") || strings.Contains(got, "\n===") || strings.Contains(got, "<https://tracker.example/pixel>") {
		t.Fatalf("Bitbucket source intent injected active Markdown structure:\n%s", got)
	}
	for _, line := range strings.Split(got, "\n") {
		if strings.HasPrefix(line, "- **AC1") && len([]rune(line)) > 302 {
			t.Fatalf("Bitbucket criterion bullet exceeded its 300-character content bound (%d): %s", len([]rune(line)), line)
		}
	}
}

func TestBuildConcisePRBody_TruncatesOldPipelineBeforeAcceptanceContext(t *testing.T) {
	t.Parallel()

	sctx := &pipeline.StepContext{UserIntent: "Complete acceptance context must survive while older pipeline rounds are optional.", IntentSource: db.RunIntentSourceAgent}
	narrative := renderConcisePRNarrative(prContent{
		Intent:             "Preserve reviewer-critical acceptance context.",
		AcceptanceCriteria: []prAcceptanceCriterion{{Summary: "Keep complete context", Details: "Older pipeline history is lower priority."}},
		Body:               "## What Changed\n\n- Prioritize concise review context.",
	}, sctx, scm.ProviderGitHub, "M\tpr.go")
	pipelineMD := pipelineMarkdownForTest(
		"old pipeline update "+strings.Repeat("x", 80000),
		"newest pipeline update",
	)
	got := buildPRBodyWithoutIntent(narrative, "⚠️ Medium: cap behavior", "## Testing\n\nFocused checks passed.", pipelineMD)
	for _, want := range []string{"Complete acceptance context", "Complete acceptance context must survive", "Focused checks passed.", "newest pipeline update"} {
		if !strings.Contains(got, want) {
			t.Fatalf("capped body dropped higher-priority %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "old pipeline update") {
		t.Fatalf("older Pipeline history survived ahead of acceptance context:\n%s", got)
	}
}

func TestNormalizeWhatChanged_DoesNotPromoteNestedBullets(t *testing.T) {
	t.Parallel()

	got := normalizeWhatChanged("## What Changed\n\n- Real top-level change.\n  - Nested implementation note.\n- Second top-level change.", "M\tpr.go", prBodyHTML)
	if strings.Count(got, "\n- ") != 2 {
		t.Fatalf("nested bullet was promoted into the top-level three-bullet budget:\n%s", got)
	}
	if !strings.Contains(got, "Nested implementation note") || !strings.Contains(got, "Additional change detail") {
		t.Fatalf("nested bullet was not retained as supporting detail:\n%s", got)
	}
}

func TestNormalizeWhatChanged_PreservesNonBulletProseInFallbackDetail(t *testing.T) {
	t.Parallel()

	got := normalizeWhatChanged("## What Changed\n\nImportant compatibility note from a legacy agent response.", "M\tinternal/pipeline/steps/pr.go", prBodyHTML)
	for _, want := range []string{"Updated 1 file", "Additional change detail", "Important compatibility note", "internal/pipeline/steps/pr.go"} {
		if !strings.Contains(got, want) {
			t.Fatalf("fallback What Changed lost %q:\n%s", want, got)
		}
	}
}

func TestRenderConcisePRNarrative_EscapesAgentAuthoredMarkdownImagesAndLinks(t *testing.T) {
	t.Parallel()

	sctx := &pipeline.StepContext{UserIntent: "Do not load third-party tracking images.", IntentSource: db.RunIntentSourceAgent}
	got := renderConcisePRNarrative(prContent{
		Intent:             "Show ![tracker](https://tracker.example/pixel), ``` fences, and ~~~ fences as text.\n## not a heading",
		AcceptanceCriteria: []prAcceptanceCriterion{{Summary: "Reject [unsafe](https://example.test) markup", Details: "Never render ![pixel](https://tracker.example/pixel), ```, or ~~~ as structure."}},
		Body:               "## What Changed\n\n- Escape ![change](https://tracker.example/pixel), ```, and ~~~ markup.",
	}, sctx, scm.ProviderGitHub, "M\tpr.go")
	for _, unsafe := range []string{"![tracker]", "![pixel]", "![change]", "[unsafe](https://example.test)", "\n## not a heading", "```", "~~~"} {
		if strings.Contains(got, unsafe) {
			t.Fatalf("agent-authored Markdown remained active as %q:\n%s", unsafe, got)
		}
	}
}

func TestRenderConcisePRNarrative_NeutralizesRemainingMarkdownConstructs(t *testing.T) {
	t.Parallel()

	intent := "Plain context.\n> quote\n+ plus list\n1. ordered list\n***\n_emphasis_\n`inline code`\n$x$"
	for _, provider := range []scm.Provider{scm.ProviderGitHub, scm.ProviderBitbucket} {
		sctx := &pipeline.StepContext{UserIntent: intent, IntentSource: db.RunIntentSourceAgent}
		got := renderConcisePRNarrative(prContent{Intent: "Keep Markdown plain.", AcceptanceCriteria: []prAcceptanceCriterion{{Summary: "Keep _text_ literal", Details: "Do not activate `code`, > quotes, or + lists."}}, Body: "## What Changed\n\n- Escape _remaining_ Markdown."}, sctx, provider, "M\tpr.go")
		for _, active := range []string{"\n> quote", "\n+ plus list", "\n1. ordered list", "\n***", "_emphasis_", "`inline code`", "$x$"} {
			if strings.Contains(got, active) {
				t.Fatalf("provider %s left active Markdown %q:\n%s", provider, active, got)
			}
		}
	}
}

func TestRenderConcisePRNarrative_HTMLEncodingDoesNotCorruptEntities(t *testing.T) {
	t.Parallel()

	sctx := &pipeline.StepContext{UserIntent: "Keep the user's #1 workflow readable.", IntentSource: db.RunIntentSourceAgent}
	got := renderConcisePRNarrative(prContent{Intent: "Keep the user's #1 workflow readable.", AcceptanceCriteria: []prAcceptanceCriterion{{Summary: "Preserve user's punctuation", Details: "Don't expose entity source text."}}, Body: "## What Changed\n\n- Preserve user's punctuation."}, sctx, scm.ProviderGitHub, "M\tpr.go")
	if strings.Contains(got, "&&#35;39;") || !strings.Contains(got, "user&#39;s") {
		t.Fatalf("HTML encoding corrupted apostrophe entities:\n%s", got)
	}
}

func TestRenderConciseRisk_BoundsVisibleRationaleAndKeepsOverflowFolded(t *testing.T) {
	t.Parallel()

	risk := "⚠️ Medium: " + strings.Repeat("calendar boundaries need careful review ", 20)
	got := renderConciseRisk(risk, scm.ProviderGitHub)
	visible := strings.Split(got, "<details>")[0]
	if len([]rune(visible)) > 260 {
		t.Fatalf("visible risk is not concise (%d chars): %s", len([]rune(visible)), visible)
	}
	if !strings.Contains(got, "<summary>More risk detail</summary>") {
		t.Fatalf("overflow risk detail was not folded:\n%s", got)
	}
}
