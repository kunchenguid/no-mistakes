package steps

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// reviewerReport is one reviewer's parsed findings after its IDs have been
// namespaced (review-<name>-N) and every item's Source stamped with the
// reviewer name, so the merged union stays attributable to its origin.
type reviewerReport struct {
	Name     string
	Findings types.Findings
}

// runReviewPanel fans the review prompt out across every reviewer concurrently
// and merges their reports into a single attributed union. opts carries the
// shared review prompt/schema/CWD; its OnChunk is forced to nil because
// StepContext.Log/LogChunk mutate shared state and are not goroutine-safe, so
// all logging and merging happen serially on this goroutine after FanOut
// returns. It enforces the fail policy: a reviewer error fails the step unless
// review.fail_open is set.
func runReviewPanel(sctx *pipeline.StepContext, reviewers []agent.Agent, opts agent.RunOpts) (Findings, error) {
	opts.OnChunk = nil
	reviewers = labelReviewers(reviewers)
	results := agent.FanOut(sctx.Ctx, reviewers, opts, sctx.Config.Review.MaxParallel)

	reports, err := processReviewerResults(results, sctx.Config.Review.FailOpen, sctx.Log, sctx.LogFile)
	if err != nil {
		return Findings{}, err
	}

	// Per-reviewer user-visible summary, emitted serially from the main
	// goroutine now that every reviewer has finished.
	for _, r := range reports {
		risk := r.Findings.RiskLevel
		if risk == "" {
			risk = "none"
		}
		sctx.Log(fmt.Sprintf("[reviewer %s] %d finding(s), risk=%s", r.Name, len(r.Findings.Items), risk))
	}

	return combineReviewerFindings(reports), nil
}

// processReviewerResults turns FanOut results into attributed reviewer reports,
// in reviewer (input) order. Each successful reviewer's findings are parsed with
// the same parser the single-reviewer path uses, ID-rewritten to
// review-<name>-N (collision-free across reviewers), Source-stamped with the
// reviewer name, and its raw report written to the file-only audit log.
//
// Fail policy: when failOpen is false (the default) the first reviewer error
// fails the step with an error naming that reviewer family. When failOpen is
// true a failed reviewer is dropped with a loud, user-visible warning and the
// step continues only if at least one reviewer succeeded. log is the
// user-visible callback; logFile is the file-only audit callback. Both run on
// the caller's goroutine.
func processReviewerResults(results []agent.FanOutResult, failOpen bool, log, logFile func(string)) ([]reviewerReport, error) {
	reports := make([]reviewerReport, 0, len(results))
	var dropped []string
	for _, res := range results {
		name := res.Agent.Name()
		if res.Err != nil {
			if !failOpen {
				return nil, fmt.Errorf("review panel: reviewer %q failed: %w", name, res.Err)
			}
			dropped = append(dropped, name)
			log(fmt.Sprintf("WARNING: reviewer %q failed and was DROPPED (review.fail_open=true): %v", name, res.Err))
			if logFile != nil {
				logFile(fmt.Sprintf("[reviewer %s] ERROR: %v", name, res.Err))
			}
			continue
		}
		parsed := parseReviewFindings(res.Result, log)
		parsed = rewriteReviewerFindingIDs(parsed, "review-"+name)
		for i := range parsed.Items {
			parsed.Items[i].Source = name
		}
		reports = append(reports, reviewerReport{Name: name, Findings: parsed})
		if logFile != nil {
			if raw, mErr := json.Marshal(parsed); mErr == nil {
				logFile(fmt.Sprintf("[reviewer %s] report: %s", name, string(raw)))
			}
		}
	}
	if len(reports) == 0 {
		return nil, fmt.Errorf("review panel: all reviewers failed (%s)", strings.Join(dropped, ", "))
	}
	return reports, nil
}

type labeledReviewer struct {
	agent.Agent
	name string
}

func (l labeledReviewer) Name() string { return l.name }

func labelReviewers(reviewers []agent.Agent) []agent.Agent {
	counts := make(map[string]int, len(reviewers))
	seen := make(map[string]bool, len(reviewers))
	labeled := make([]agent.Agent, 0, len(reviewers))
	for _, reviewer := range reviewers {
		base := sanitizeReviewerLabel(reviewer.Name())
		name := base
		for {
			counts[base]++
			if counts[base] == 1 {
				name = base
			} else {
				name = fmt.Sprintf("%s-%d", base, counts[base])
			}
			if !seen[name] {
				break
			}
		}
		seen[name] = true
		labeled = append(labeled, labeledReviewer{Agent: reviewer, name: name})
	}
	return labeled
}

func sanitizeReviewerLabel(name string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		valid := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if valid {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	label := strings.Trim(b.String(), "-")
	if label == "" {
		return "reviewer"
	}
	return label
}

func rewriteReviewerFindingIDs(findings types.Findings, prefix string) types.Findings {
	for i := range findings.Items {
		findings.Items[i].ID = fmt.Sprintf("%s-%d", prefix, i+1)
	}
	return findings
}

// combineReviewerFindings merges reviewer reports into a plain attributed union.
// Items are concatenated in reviewer (input) order, each keeping the
// review-<name>-N id and Source set by processReviewerResults - there is NO
// fingerprint dedup, agreement-collapse, or severity-escalation. The scalar
// fields are reconciled: RiskLevel is the maximum (low < medium < high) across
// reports, while RiskRationale and Summary become per-reviewer labeled
// concatenations ("[codex] ...; [claude] ...") so the fix agent and human can
// see who said what.
func combineReviewerFindings(reports []reviewerReport) types.Findings {
	var merged types.Findings
	rationales := make([]string, 0, len(reports))
	summaries := make([]string, 0, len(reports))
	for _, r := range reports {
		merged.Items = append(merged.Items, r.Findings.Items...)
		if types.RiskRank(r.Findings.RiskLevel) > types.RiskRank(merged.RiskLevel) {
			merged.RiskLevel = r.Findings.RiskLevel
		}
		if s := strings.TrimSpace(r.Findings.RiskRationale); s != "" {
			rationales = append(rationales, fmt.Sprintf("[%s] %s", r.Name, s))
		}
		if s := strings.TrimSpace(r.Findings.Summary); s != "" {
			summaries = append(summaries, fmt.Sprintf("[%s] %s", r.Name, s))
		}
	}
	merged.RiskRationale = strings.Join(rationales, "; ")
	merged.Summary = strings.Join(summaries, "; ")
	return merged
}
