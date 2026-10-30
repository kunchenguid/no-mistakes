package github

import (
	"context"
	"encoding/json"
	"net/url"
	"strconv"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

const billingRefusalAnnotation = "The job was not started because recent account payments have failed or your spending limit needs to be increased."

type billingEvidenceKey struct {
	headSHA, runID, jobID, checkID string
}

// markBillingFailures requires evidence for the exact job, not its name or
// duration. GitHub supplies no structured billing conclusion, so only its
// billing annotation together with an explicitly empty step list qualifies.
// Missing or unreadable evidence leaves an unproven failure untouched. Once
// proven, a terminal job's diagnosis is retained for its exact identity.
func (h *Host) markBillingFailures(ctx context.Context, checks []scm.Check, headSHA string) {
	for i, check := range checks {
		if !check.Failing() || check.Kind == scm.CheckKindStatus || check.App != "" && check.App != "github-actions" {
			continue
		}
		runID, jobID, ok := h.actionsRerunTarget(check.Link)
		if !ok || jobID == "" {
			continue
		}
		key := billingEvidenceKey{headSHA, runID, jobID, check.ProviderID}
		// Only head-bound checks with a provider identity can reuse evidence.
		// A later read failure cannot undo a confirmed terminal job refusal.
		retain := headSHA != "" && check.ProviderID != ""
		if _, confirmed := h.confirmedBilling.Load(key); retain && confirmed {
			checks[i].InfrastructureFailure = billingRefusalAnnotation
			continue
		}
		var job struct {
			ID          int64            `json:"id"`
			RunID       int64            `json:"run_id"`
			HeadSHA     string           `json:"head_sha"`
			Status      string           `json:"status"`
			Conclusion  string           `json:"conclusion"`
			Steps       *[]githubJobStep `json:"steps"`
			CheckRunURL string           `json:"check_run_url"`
		}
		if !h.readBillingEvidence(ctx, "repos/"+h.repoSlug()+"/actions/jobs/"+jobID, &job) ||
			strconv.FormatInt(job.ID, 10) != jobID || strconv.FormatInt(job.RunID, 10) != runID ||
			job.Status != "completed" || job.Conclusion != "failure" ||
			job.Steps == nil || len(*job.Steps) != 0 || headSHA != "" && job.HeadSHA != headSHA {
			continue
		}
		// The job API binds the Actions job to its check run. Never read an
		// arbitrary URL from provider output, or borrow a same-named job's data.
		apiHost := normalizeGitHubHost(h.host)
		prefix := "/repos/"
		switch {
		case apiHost == "" || apiHost == "github.com":
			apiHost = "api.github.com"
		case strings.HasSuffix(apiHost, ".ghe.com"):
			apiHost = "api." + apiHost
		default:
			prefix = "/api/v3/repos/"
		}
		checkURL, err := url.Parse(job.CheckRunURL)
		if err != nil || checkURL.Scheme != "https" || !strings.EqualFold(checkURL.Host, apiHost) ||
			checkURL.User != nil || checkURL.RawQuery != "" || checkURL.ForceQuery || checkURL.Fragment != "" || !strings.HasPrefix(checkURL.Path, prefix) {
			continue
		}
		parts := strings.Split(strings.TrimPrefix(checkURL.Path, prefix), "/")
		if len(parts) != 4 || !strings.EqualFold(parts[0]+"/"+parts[1], h.repoSlug()) || parts[2] != "check-runs" || !isNumericID(parts[3]) {
			continue
		}
		checkID := parts[3]
		if check.ProviderID != "" && check.ProviderID != "github-check-run:"+checkID {
			continue
		}
		var pages [][]struct {
			Level   string `json:"annotation_level"`
			Message string `json:"message"`
		}
		if !h.readBillingEvidence(ctx, "repos/"+h.repoSlug()+"/check-runs/"+checkID+"/annotations", &pages, "--paginate", "--slurp") {
			continue
		}
		for _, annotations := range pages {
			for _, annotation := range annotations {
				if annotation.Level == "failure" && strings.HasPrefix(strings.Join(strings.Fields(annotation.Message), " "), billingRefusalAnnotation) {
					// Retain only the recognized provider diagnosis. Raw details,
					// paths and extra annotation prose are not publishable evidence.
					checks[i].InfrastructureFailure = billingRefusalAnnotation
					if retain {
						h.confirmedBilling.Store(key, struct{}{})
					}
				}
			}
		}
	}
}

func (h *Host) readBillingEvidence(ctx context.Context, endpoint string, result any, flags ...string) bool {
	args := []string{"api"}
	if h.host != "" {
		args = append(args, "--hostname", h.host)
	}
	args = append(args, endpoint)
	args = append(args, flags...)
	out, err := h.cmd(ctx, "gh", args...).Output()
	return err == nil && json.Unmarshal(out, result) == nil
}
