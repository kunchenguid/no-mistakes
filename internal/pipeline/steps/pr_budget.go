package steps

import (
	"fmt"
	"sort"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Embed the concrete steps so their approval/recovery interfaces remain intact.
// Budget checks belong to the production sequence, outside Intent's best-effort
// extraction and before Review/Test can invoke an agent or repository command.
type budgetIntentStep struct{ IntentStep }
type budgetReviewStep struct{ ReviewStep }
type budgetTestStep struct{ TestStep }

func (s *budgetIntentStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	outcome, err := s.IntentStep.Execute(sctx)
	if err != nil {
		return outcome, err
	}
	if err := checkEarlyPRBudget(sctx); err != nil {
		return nil, err
	}
	return outcome, nil
}

func (s *budgetReviewStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	if err := checkEarlyPRBudget(sctx); err != nil {
		return nil, err
	}
	return s.ReviewStep.Execute(sctx)
}

func (s *budgetTestStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	if err := checkEarlyPRBudget(sctx); err != nil {
		return nil, err
	}
	return s.TestStep.Execute(sctx)
}

func checkEarlyPRBudget(sctx *pipeline.StepContext) error {
	if sctx == nil || sctx.Run == nil || sctx.Repo == nil || sctx.DB == nil || sctx.EvalReplay {
		return nil
	}
	if strings.TrimPrefix(sctx.Run.Branch, "refs/heads/") == effectivePRBaseBranch(sctx) {
		return nil
	}
	steps, err := sctx.DB.GetStepsByRun(sctx.Run.ID)
	if err != nil {
		if sctx.Log != nil {
			sctx.Log(fmt.Sprintf("PR body budget estimate unavailable: %v", err))
		}
		return nil
	}
	for _, step := range steps {
		if step.StepName == types.StepPR && step.Status == types.StepStatusSkipped {
			return nil
		}
	}
	provider := resolvedProvider(sctx)
	host, _ := buildHost(sctx, provider)
	if host == nil {
		return nil
	}
	if err := host.Available(sctx.Ctx); err != nil {
		if pluginContractBroken(err) {
			return err
		}
		if sctx.Log != nil {
			sctx.Log(fmt.Sprintf("PR body budget estimate unavailable: %v", err))
		}
		return nil
	}
	template := configuredPRTemplate(sctx)
	if template != "" {
		if _, ok := host.(scm.PRContentReader); !ok {
			return nil
		}
	}
	branch := strings.TrimPrefix(sctx.Run.Branch, "refs/heads/")
	existing, live, err := findExistingPRForPublication(sctx, host, branch)
	if err != nil {
		if pluginContractBroken(err) {
			return err
		}
		if sctx.Log != nil {
			sctx.Log(fmt.Sprintf("PR body budget estimate unavailable: %v", err))
		}
		return nil
	}
	owned := prBodyIsOwned(template, existing, live.Body)
	var existingParts *prOwnedBody
	if owned && existing != nil {
		parts, err := parsePROwnedBody(live.Body)
		if err != nil {
			if sctx.Log != nil {
				sctx.Log(fmt.Sprintf("PR body budget estimate unavailable: %v", err))
			}
			return nil
		}
		existingParts = &parts
	}
	body, sections, err := estimatePRBody(sctx, provider, existingParts, steps)
	if err != nil {
		if sctx.Log != nil {
			sctx.Log(fmt.Sprintf("PR body budget estimate unavailable: %v", err))
		}
		return nil
	}
	limit := scm.HostMaxPRBodyChars(host)
	measure, budget, units := func(s string) int { return len(s) }, maxPullRequestBodyBytes, "bytes (publication safety limit)"
	if limit > 0 && float64(scm.PRBodyLen(body))/float64(limit) > float64(len(body))/float64(maxPullRequestBodyBytes) {
		measure, budget, units = scm.PRBodyLen, limit, "UTF-16 units (forge limit)"
	}
	sort.SliceStable(sections, func(i, j int) bool { return measure(sections[i].text) > measure(sections[j].text) })
	var largest []string
	for _, section := range sections[:min(3, len(sections))] {
		largest = append(largest, fmt.Sprintf("%s=%d", section.name, measure(section.text)))
	}
	detail := fmt.Sprintf("estimated PR body %d/%d %s; largest sections: %s", measure(body), budget, units, strings.Join(largest, ", "))
	guidance := "Shorten the public intent or template narrative; move long evidence to a linked file. No author text, closing references or recorded evidence was dropped. Later drafting and evidence may increase this estimate."
	if owned {
		if err := validateOwnedPRBudget(body, limit); err != nil {
			return fmt.Errorf("%w: %s. %s", err, detail, guidance)
		}
	}
	if float64(measure(body))/float64(budget) >= 0.8 && sctx.Log != nil {
		sctx.Log("warning: near PR body budget: " + detail + ". " + guidance)
	}
	return nil
}

type prBudgetSection struct{ name, text string }

// Render the currently known public material without uploads, evidence-branch
// pushes, or drafting. Future model prose and evidence are deliberately unknown.
func estimatePRBody(sctx *pipeline.StepContext, provider scm.Provider, existingParts *prOwnedBody, steps []*db.StepResult) (string, []prBudgetSection, error) {
	rounds := make(map[string][]*db.StepRound, len(steps))
	var err error
	for _, step := range steps {
		rounds[step.ID], err = sctx.DB.GetRoundsByStep(step.ID)
		if err != nil {
			return "", nil, err
		}
	}
	// Intent extraction updates Run before the executor refreshes UserIntent.
	view := *sctx
	if sctx.Run.Intent != nil {
		view.UserIntent = *sctx.Run.Intent
	}
	// Reattachment can add --closes while a run is active. Read without
	// claiming them; publication remains the sole owner of that lock.
	currentRun, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		return "", nil, err
	}
	if currentRun == nil {
		return "", nil, fmt.Errorf("run no longer exists")
	}
	view.ClosingIssueRefs = currentRun.ClosingIssueRefs
	templateName := configuredPRTemplate(&view)
	pipelineMD, risk, testing := pipelineSectionFrom(sctx, provider, existingParts != nil || templateName != "", steps, rounds, nil, nil)
	narrative := "## What Changed\n\n"
	if templateName != "" {
		narrative, err = loadPRTemplate(sctx.Ctx, sctx.WorkDir, sctx.Config.TrustedConfigSHA, templateName)
		if err != nil {
			return "", nil, err
		}
		// Only top-level H1 headings are structurally required in the drafted
		// narrative; instructions, options, and placeholders can be removed.
		narrative = strings.Join(templateStructureLines(narrative), "\n")
	}
	var parts *prOwnedBody
	if existingParts != nil {
		copy := *existingParts
		if copy.before == "" && !copy.managed && templateName != "" {
			copy.before = narrative
		}
		parts = &copy
		narrative = parts.before + parts.after
	}
	appendix := renderPRAppendix(&view, provider, pipelineMD, risk, testing)
	authorText := narrative
	if parts != nil {
		authorText = parts.before + "\n" + parts.after
	}
	issues := issuesSection(&view, authorText)
	appendix = appendIssuesSection(appendix, issues)
	var body string
	if parts != nil {
		body = renderOwnedPRContent(*parts, "", appendix).Body
	} else if templateName != "" {
		body = renderOwnedPRContent(prOwnedBody{before: narrative}, "", appendix).Body
	} else {
		body = redactPRContent(prContent{Body: narrative + "\n\n" + appendix}).Body
	}
	sections := []prBudgetSection{
		{"Intent", publicPRIntent(&view)}, {"Template/narrative", narrative},
		{"Pipeline", pipelineMD}, {"Testing", testing}, {"Risk Assessment", risk}, {"Closing references", issues},
	}
	if appendixMode(&view) == config.PRAppendixMinimal {
		sections[2].text = minimalTail(risk, pipelineMD, provider)
		sections[3].text, sections[4].text = "", ""
	}
	for i := range sections {
		sections[i].text = redactPRContent(prContent{Body: sections[i].text}).Body
	}
	return body, sections, nil
}
