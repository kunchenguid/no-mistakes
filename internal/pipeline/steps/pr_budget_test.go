package steps

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/scm/plugin/fakeplugin"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func productionBudgetStep(t *testing.T, name types.StepName) pipeline.Step {
	t.Helper()
	for _, step := range AllSteps() {
		if step.Name() == name {
			return step
		}
	}
	t.Fatalf("missing step %s", name)
	return nil
}

func seedOwnedBudgetPR(t *testing.T, sctx *pipeline.StepContext, maxPRBodyChars int) string {
	t.Helper()
	content, _ := ownedFixture(t)
	_, logPath := fakeProviderPlugin(t, sctx, fakeplugin.State{
		MaxPRBodyChars: maxPRBodyChars,
		PRs: []fakeplugin.PR{{
			Number: "1", URL: "https://git.example.com/team/repo/pulls/1",
			HeadBranch: "feature", BaseBranch: "main", Body: content.Body, State: "open",
		}},
	})
	return logPath
}

func TestPRBudgetProductionStepsRefuseBeforeExpensiveWork(t *testing.T) {
	for _, name := range []types.StepName{types.StepIntent, types.StepReview, types.StepTest} {
		t.Run(string(name), func(t *testing.T) {
			sctx := newIntentStepContext(t)
			intent := strings.Repeat("large public intent ", 4000)
			sctx.Run.Intent = &intent
			seedOwnedBudgetPR(t, sctx, 100000)
			_, err := productionBudgetStep(t, name).Execute(sctx)
			if err == nil || !strings.Contains(err.Error(), "largest sections: Intent=") || !strings.Contains(err.Error(), "63488") || !strings.Contains(err.Error(), "linked file") {
				t.Fatalf("did not refuse before needing an agent/git/test command: %v", err)
			}
		})
	}
}

func TestPRBudgetRechecksRecordedEvidenceBeforeReviewAndTest(t *testing.T) {
	for _, name := range []types.StepName{types.StepReview, types.StepTest} {
		t.Run(string(name), func(t *testing.T) {
			sctx := newIntentStepContext(t)
			intent := "A short intent"
			sctx.Run.Intent = &intent
			seedOwnedBudgetPR(t, sctx, 100000)
			if _, err := productionBudgetStep(t, types.StepIntent).Execute(sctx); err != nil {
				t.Fatal(err)
			}
			insertCompletedStep(t, sctx, types.StepRebase, "", strings.Repeat("recorded failure evidence ", 4000))
			_, err := productionBudgetStep(t, name).Execute(sctx)
			if err == nil || !strings.Contains(err.Error(), "largest sections: Pipeline=") {
				t.Fatalf("recorded evidence did not stop expensive work: %v", err)
			}
		})
	}
}

func TestPRBudgetUnderLimitStillRunsReviewAndTest(t *testing.T) {
	for _, name := range []types.StepName{types.StepReview, types.StepTest} {
		t.Run(string(name), func(t *testing.T) {
			dir, base, head := setupGitRepo(t)
			output := passingScenarioFindingsJSON
			if name == types.StepReview {
				output = `{"findings":[],"summary":"clean","reviewed_paths":["feature.txt"],"risk_level":"low","risk_rationale":"bounded","risk_scope":"source-or-external"}`
			}
			ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
				return &agent.Result{Output: json.RawMessage(output)}, nil
			}}
			sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
			sctx.Env, _ = fakeGH(t, "")
			outcome, err := productionBudgetStep(t, name).Execute(sctx)
			if err != nil || outcome == nil || outcome.NeedsApproval || len(ag.calls) == 0 {
				t.Fatalf("under-budget step did not run normally: %+v %v, agent calls=%d", outcome, err, len(ag.calls))
			}
		})
	}
}

func TestPRBudgetNearLimitOnlyWarnsAndOmittedIntentIsExcluded(t *testing.T) {
	sctx := newIntentStepContext(t)
	fakeProviderPlugin(t, sctx, fakeplugin.State{MaxPRBodyChars: 100000})
	intent := strings.Repeat("public text ", 4700)
	sctx.Run.Intent = &intent
	var logs []string
	sctx.Log = func(line string) { logs = append(logs, line) }
	if _, err := productionBudgetStep(t, types.StepIntent).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(logs, "\n"), "warning: near PR body budget") {
		t.Fatalf("missing warning: %v", logs)
	}
	intent = strings.Repeat("private text ", 10000)
	sctx.Run.OmitIntent = true
	logs = nil
	if _, err := productionBudgetStep(t, types.StepIntent).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(logs, "\n"), "PR body budget") {
		t.Fatalf("omitted intent affected budget: %v", logs)
	}
}

func TestPRBudgetOrdinaryDraftDoesNotRefuseOverLimit(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{false: "new PR", true: "unowned existing PR"}[existing], func(t *testing.T) {
			sctx := newIntentStepContext(t)
			state := fakeplugin.State{MaxPRBodyChars: 100000}
			if existing {
				state.PRs = []fakeplugin.PR{{
					Number: "1", URL: "https://git.example.com/team/repo/pulls/1",
					HeadBranch: "feature", BaseBranch: "main", Body: "Author-owned narrative", State: "open",
				}}
			}
			fakeProviderPlugin(t, sctx, state)
			intent := strings.Repeat("public text ", 7000)
			sctx.Run.Intent = &intent
			var logs []string
			sctx.Log = func(line string) { logs = append(logs, line) }
			if _, err := productionBudgetStep(t, types.StepIntent).Execute(sctx); err != nil {
				t.Fatalf("ordinary draft was refused: %v", err)
			}
			if !strings.Contains(strings.Join(logs, "\n"), "warning: near PR body budget") {
				t.Fatalf("over-limit ordinary draft did not only warn: %v", logs)
			}
		})
	}
}

func TestPRBudgetRecordedSkippedPRDoesNotEstimateOrRefuse(t *testing.T) {
	sctx := newIntentStepContext(t)
	prStep, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepPR)
	if err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.CompleteStepWithStatus(prStep.ID, types.StepStatusSkipped, 0, 0, ""); err != nil {
		t.Fatal(err)
	}
	intent := strings.Repeat("large public intent ", 4000)
	sctx.Run.Intent = &intent
	logPath := seedOwnedBudgetPR(t, sctx, 100000)
	var logs []string
	sctx.Log = func(line string) { logs = append(logs, line) }
	if _, err := productionBudgetStep(t, types.StepIntent).Execute(sctx); err != nil {
		t.Fatalf("explicit PR skip was refused: %v", err)
	}
	if strings.Contains(strings.Join(logs, "\n"), "PR body budget") {
		t.Fatalf("explicit PR skip ran budget check: %v", logs)
	}
	if _, err := os.Stat(logPath); err == nil {
		if ops := pluginOperations(t, logPath); len(ops) != 0 {
			t.Fatalf("explicit PR skip queried the provider: %v", ops)
		}
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

func TestPRBudgetExistingOwnedBitbucketBodyIncludesAttestation(t *testing.T) {
	sctx := newIntentStepContext(t)
	if _, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepReview); err != nil {
		t.Fatal(err)
	}
	steps, err := sctx.DB.GetStepsByRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	content, _ := ownedFixture(t)
	parts, err := parsePROwnedBody(content.Body)
	if err != nil {
		t.Fatal(err)
	}
	body, sections, err := estimatePRBody(sctx, scm.ProviderBitbucket, &parts, steps)
	if err != nil {
		t.Fatal(err)
	}
	fence := "```text\n" + pipelineAttestationCommentPrefix
	if !strings.Contains(body, fence) {
		t.Fatal("owned Bitbucket body estimate omitted visible attestation")
	}
	for _, section := range sections {
		if section.name == "Pipeline" {
			if !strings.Contains(section.text, fence) {
				t.Fatal("owned Bitbucket pipeline estimate omitted visible attestation")
			}
			return
		}
	}
	t.Fatal("pipeline section missing from PR body estimate")
}

func TestPRBudgetPluginStatusProtocolViolationFailsClosed(t *testing.T) {
	sctx := newIntentStepContext(t)
	_, logPath := fakeProviderPlugin(t, sctx, fakeplugin.State{ProtocolVersion: 99})
	_, err := productionBudgetStep(t, types.StepReview).Execute(sctx)
	if err == nil || !pluginContractBroken(err) {
		t.Fatalf("invalid plugin status handshake did not fail the early check: %v", err)
	}
	if ops := pluginOperations(t, logPath); len(ops) != 1 || ops[0] != "status" {
		t.Fatalf("provider calls after invalid status handshake: %v", ops)
	}
}

func TestPRBudgetPluginPRReadProtocolViolationFailsClosed(t *testing.T) {
	sctx := newIntentStepContext(t)
	_, logPath := fakeProviderPlugin(t, sctx, fakeplugin.State{
		MaxPRBodyChars: 100000,
		PRs: []fakeplugin.PR{{
			Number: "1", URL: "https://git.example.com/other/repo/pulls/1",
			HeadBranch: "feature", BaseBranch: "main", State: "open",
		}},
	})
	_, err := productionBudgetStep(t, types.StepReview).Execute(sctx)
	if err == nil || !pluginContractBroken(err) {
		t.Fatalf("invalid plugin PR lookup did not fail the early check: %v", err)
	}
	if ops := pluginOperations(t, logPath); len(ops) != 2 || ops[0] != "status" || ops[1] != "pr find" {
		t.Fatalf("provider calls after invalid PR lookup: %v", ops)
	}
}

func TestPRBudgetPluginContentReadTimeoutFailsClosed(t *testing.T) {
	sctx := newIntentStepContext(t)
	_, logPath := fakeProviderPlugin(t, sctx, fakeplugin.State{
		MaxPRBodyChars: 100000,
		HangOn:         "pr view",
		PRs: []fakeplugin.PR{{
			Number: "1", URL: "https://git.example.com/team/repo/pulls/1",
			HeadBranch: "feature", BaseBranch: "main", State: "open",
		}},
	})
	pluginConfig := sctx.Config.ProviderPlugins["ssm"]
	pluginConfig.Timeout = time.Second
	sctx.Config.ProviderPlugins["ssm"] = pluginConfig
	_, err := productionBudgetStep(t, types.StepReview).Execute(sctx)
	if err == nil || !pluginContractBroken(err) {
		t.Fatalf("one-shot plugin PR content timeout did not fail the early check: %v", err)
	}
	if ops := pluginOperations(t, logPath); len(ops) != 3 || ops[0] != "status" || ops[1] != "pr find" || ops[2] != "pr view" {
		t.Fatalf("provider calls after PR content timeout: %v", ops)
	}
}

func TestPRBudgetOrdinaryPluginUnavailabilitySkipsEstimate(t *testing.T) {
	sctx := newIntentStepContext(t)
	_, logPath := fakeProviderPlugin(t, sctx, fakeplugin.State{Unauthenticated: true})
	if err := checkEarlyPRBudget(sctx); err != nil {
		t.Fatalf("ordinary plugin unavailability failed the estimate: %v", err)
	}
	if ops := pluginOperations(t, logPath); len(ops) != 1 || ops[0] != "status" {
		t.Fatalf("provider calls after unavailable status: %v", ops)
	}
}

func TestPRBudgetUnresolvableProviderDoesNotRefuse(t *testing.T) {
	sctx := newIntentStepContext(t)
	intent := strings.Repeat("large public intent ", 4000)
	sctx.Run.Intent = &intent
	if _, err := productionBudgetStep(t, types.StepIntent).Execute(sctx); err != nil {
		t.Fatalf("unresolvable PR provider was refused: %v", err)
	}
}

func TestPRBudgetTemplateRefusesBeforeReview(t *testing.T) {
	sctx, ag, _ := templateTestContext(t)
	sctx.Env, _ = fakeGH(t, "")
	intent := strings.Repeat("large public intent ", 4000)
	sctx.Run.Intent = &intent
	_, err := productionBudgetStep(t, types.StepReview).Execute(sctx)
	if err == nil || !strings.Contains(err.Error(), "largest sections: Intent=") {
		t.Fatalf("owned template body was not refused before review: %v", err)
	}
	if len(ag.calls) != 0 {
		t.Fatalf("preflight invoked review agent: %d calls", len(ag.calls))
	}
}

func TestPRBudgetLargeRemovableTemplateFitsBeforeIntent(t *testing.T) {
	sctx, ag, _ := templateTestContext(t)
	largeTemplate := "# Summary\n\n" +
		"<!-- " + strings.Repeat("Describe the final change. ", 100) + "-->\n\n" +
		"## Details\n\nPlease remove unused options and replace this placeholder.\n" +
		strings.Repeat("- [ ] Unused option that can be removed\n", 100) +
		"\n# Validation\n\n" + strings.Repeat("Validation placeholder. ", 50)
	if len(largeTemplate) <= 4000 {
		t.Fatalf("fixture template is not larger than the forge limit: %d", len(largeTemplate))
	}
	name := sctx.Config.PR.Template
	if err := os.WriteFile(filepath.Join(sctx.WorkDir, filepath.FromSlash(name)), []byte(largeTemplate), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, sctx.WorkDir, "add", name)
	gitCmd(t, sctx.WorkDir, "commit", "-m", "large trusted template")
	sctx.Config.TrustedConfigSHA = gitCmd(t, sctx.WorkDir, "rev-parse", "HEAD")
	ag.runFn = func(context.Context, agent.RunOpts) (*agent.Result, error) {
		body := "# Summary\nA concise account of the change.\n# Validation\nValidation passed."
		data, _ := json.Marshal(prContent{Title: "fix: concise change", Body: body})
		return &agent.Result{Output: data}, nil
	}
	fakeProviderPlugin(t, sctx, fakeplugin.State{MaxPRBodyChars: 4000})

	steps, err := sctx.DB.GetStepsByRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	estimate, sections, err := estimatePRBody(sctx, scm.PluginProvider("ssm"), nil, steps)
	if err != nil {
		t.Fatal(err)
	}
	var narrative string
	for _, section := range sections {
		if section.name == "Template/narrative" {
			narrative = section.text
		}
	}
	if narrative != "# Summary\n# Validation" {
		t.Fatalf("estimated narrative = %q, want required heading skeleton", narrative)
	}
	if err := validateTemplateStructure(largeTemplate, narrative); err != nil {
		t.Fatalf("estimated narrative did not retain required structure: %v", err)
	}
	if strings.Contains(estimate, "Unused option") || scm.PRBodyLen(estimate) >= 4000 {
		t.Fatalf("minimum template estimate did not fit: body length %d", scm.PRBodyLen(estimate))
	}

	// Exercise the production Intent wrapper: it runs the same early estimate
	// before allowing the pipeline to continue.
	sctx.Config.Intent.Enabled = false
	if _, err := productionBudgetStep(t, types.StepIntent).Execute(sctx); err != nil {
		t.Fatalf("publishable template was refused by the early step: %v", err)
	}

	draft, err := (&PRStep{}).draftTemplateNarrative(sctx, "feature", "main", sctx.Run.BaseSHA, largeTemplate)
	if err != nil {
		t.Fatalf("short template draft failed validation: %v", err)
	}
	if err := validateTemplateStructure(largeTemplate, draft.Body); err != nil {
		t.Fatalf("draft omitted required headings: %v", err)
	}
	parts, err := parsePROwnedBody(estimate)
	if err != nil {
		t.Fatal(err)
	}
	published, err := composeOwnedPRContent(prOwnedBody{before: draft.Body}, draft.Title, parts.appendix, 4000)
	if err != nil {
		t.Fatalf("short template narrative did not fit publication budget: %v", err)
	}
	if scm.PRBodyLen(published.Body) > 4000 {
		t.Fatalf("composed body exceeds forge limit: %d", scm.PRBodyLen(published.Body))
	}
}

func TestPRBudgetExistingOwnedAuthorTextCounts(t *testing.T) {
	sctx := newIntentStepContext(t)
	for _, name := range types.AllSteps() {
		if _, err := sctx.DB.InsertStepResult(sctx.Run.ID, name); err != nil {
			t.Fatal(err)
		}
	}
	content, appendix := ownedFixture(t)
	parts, err := parsePROwnedBody(content.Body)
	if err != nil {
		t.Fatal(err)
	}
	padding := maxPullRequestBodyBytes - len(content.Body) + len(parts.before) - 128
	parts.before = strings.Repeat("author text\n", padding/len("author text\n")) + "\n"
	content = renderOwnedPRContent(parts, "", appendix)
	state := fakeplugin.State{
		MaxPRBodyChars: 100000,
		PRs: []fakeplugin.PR{{
			Number: "1", URL: "https://git.example.com/team/repo/pulls/1",
			HeadBranch: "feature", BaseBranch: "main", Body: content.Body, State: "open",
		}},
	}
	fakeProviderPlugin(t, sctx, state)
	if len(content.Body) >= maxPullRequestBodyBytes {
		t.Fatal("existing author body fixture exceeded its original budget")
	}
	_, err = productionBudgetStep(t, types.StepReview).Execute(sctx)
	if err == nil || !strings.Contains(err.Error(), "PR body exceeds provider budget") {
		t.Fatalf("existing author text was omitted from the estimate: %v", err)
	}
}

func TestPRBudgetUsesForgeUnitsAndFinalGuardStillCatchesGrowth(t *testing.T) {
	sctx := newIntentStepContext(t)
	intent := strings.Repeat("😀", 2100)
	sctx.Run.Intent = &intent
	seedOwnedBudgetPR(t, sctx, 4000)
	_, err := productionBudgetStep(t, types.StepIntent).Execute(sctx)
	if err == nil || !strings.Contains(err.Error(), "/4000 UTF-16 units (forge limit)") {
		t.Fatalf("ignored forge limit: %v", err)
	}
	intent = "Small public intent"
	if _, err := productionBudgetStep(t, types.StepIntent).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	_, appendix := ownedFixture(t)
	if _, err := composeOwnedPRContent(prOwnedBody{before: "Small narrative"}, "", appendix+strings.Repeat("later evidence ", 6000), 0); err == nil {
		t.Fatal("final publication guard accepted later evidence growth")
	}
}

func TestPRBudgetEstimateKeepsRecoveryAndOverrideInterfaces(t *testing.T) {
	if _, ok := productionBudgetStep(t, types.StepReview).(pipeline.ApprovalGateResumer); !ok {
		t.Fatal("review lost its gate resumer")
	}
	if _, ok := productionBudgetStep(t, types.StepTest).(pipeline.ApprovalOverrideVerifier); !ok {
		t.Fatal("test lost its override verifier")
	}
}

func TestPRBudgetMinimalAppendixDoesNotCountExcludedEvidence(t *testing.T) {
	sctx := newIntentStepContext(t)
	insertCompletedStep(t, sctx, types.StepRebase, "", strings.Repeat("recorded evidence ", 5000))
	sctx.Config.PR.Appendix = config.PRAppendixMinimal
	fakeProviderPlugin(t, sctx, fakeplugin.State{MaxPRBodyChars: 100000})
	steps, err := sctx.DB.GetStepsByRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	body, _, err := estimatePRBody(sctx, scm.ProviderGitHub, nil, steps)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(body, "recorded evidence") {
		t.Fatal("minimal estimate included excluded full evidence")
	}
	if err := checkEarlyPRBudget(sctx); err != nil {
		t.Fatal(err)
	}
}

func TestPRBudgetCountsTrustedTemplateAndUnclaimedClosingReferences(t *testing.T) {
	sctx, ag, _ := templateTestContext(t)
	if err := sctx.DB.UpdateRunClosingIssueRefs(sctx.Run.ID, []string{"1471"}); err != nil {
		t.Fatal(err)
	}
	sctx.Env, _ = fakeGH(t, "")
	steps, err := sctx.DB.GetStepsByRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	body, _, err := estimatePRBody(sctx, scm.ProviderGitHub, nil, steps)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(body, "# Overview\n\n") || strings.Contains(body, "Describe the final change") || strings.Contains(body, "CONTRIBUTOR TEMPLATE MUST NOT WIN") || !strings.Contains(body, "Closes #1471") || !hasPRAppendixMarkers(body) {
		t.Fatalf("estimate lost trusted template, appendix or closing references: %s", body)
	}
	if err := checkEarlyPRBudget(sctx); err != nil {
		t.Fatal(err)
	}
	if len(ag.calls) != 0 {
		t.Fatal("preflight invoked a drafting agent")
	}
	if err := sctx.DB.UpdateRunClosingIssueRefs(sctx.Run.ID, []string{"1471", "1472"}); err != nil {
		t.Fatalf("preflight claimed the closing references: %v", err)
	}
}

func TestPRBudgetUsesPluginDeclaredLimitWithoutPublishing(t *testing.T) {
	sctx := newIntentStepContext(t)
	_, logPath := fakeProviderPlugin(t, sctx, fakeplugin.State{MaxPRBodyChars: 4000})
	intent := strings.Repeat("intent ", 700)
	sctx.Run.Intent = &intent
	var logs []string
	sctx.Log = func(line string) { logs = append(logs, line) }
	_, err := productionBudgetStep(t, types.StepIntent).Execute(sctx)
	if err != nil {
		t.Fatalf("ordinary plugin body was refused: %v", err)
	}
	joined := strings.Join(logs, "\n")
	if !strings.Contains(joined, "warning: near PR body budget") || !strings.Contains(joined, "/4000 UTF-16 units (forge limit)") {
		t.Fatalf("plugin limit was not used for the ordinary-path warning: %v", logs)
	}
	if ops := pluginOperations(t, logPath); len(ops) != 2 || ops[0] != "status" || ops[1] != "pr find" {
		t.Fatalf("preflight performed operations beyond read-only discovery: %v", ops)
	}
}
