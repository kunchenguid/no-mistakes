// Package azuredevops implements scm.Host backed by the az CLI with the
// azure-devops extension.
package azuredevops

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// outputJSON runs cmd and returns its stdout alone, leaving stderr out of the
// payload so non-JSON az chatter (preview-command notices, token-refresh
// messages) cannot corrupt the bytes a caller json.Unmarshal's. On failure it
// surfaces the separately-captured stderr in the error.
func outputJSON(cmd *exec.Cmd) ([]byte, error) {
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(bytes.TrimSpace(ee.Stderr)) > 0 {
			return nil, fmt.Errorf("%s: %w", strings.TrimSpace(string(ee.Stderr)), err)
		}
		return nil, err
	}
	return out, nil
}

// clampDescription truncates body to Azure DevOps' PR-description cap. The
// pipeline already budgets the body to fit (shedding whole sections), so this
// is the connector-level backstop that guarantees `az repos pr create`/`update`
// never sees an over-length description, no matter how the body was produced.
func clampDescription(body string) string {
	return scm.ClampPRBody(body, scm.MaxPRBodyChars(scm.ProviderAzureDevOps))
}

// CmdFactory builds an exec.Cmd in the caller's workdir with the caller's env.
type CmdFactory func(ctx context.Context, name string, args ...string) *exec.Cmd

// Host talks to Azure DevOps through the az CLI (azure-devops extension).
type Host struct {
	cmd          CmdFactory
	cliAvailable func() bool
	org          string // organization URL, e.g. https://dev.azure.com/myorg
	project      string // project name (may contain spaces)
	repo         string // repository name
}

// New builds a Host. cliAvailable reports whether the az binary is resolvable
// on the caller's PATH. org is the organization URL; it is passed via
// --organization to every command so they resolve the right organization
// regardless of the process working directory. The daemon runs from a fixed,
// non-repo working dir, so without it az cannot infer the org (or repo) and
// fails on every poll. project and repo name the repository.
func New(cmd CmdFactory, cliAvailable func() bool, org, project, repo string) *Host {
	return &Host{
		cmd:          cmd,
		cliAvailable: cliAvailable,
		org:          strings.TrimSpace(org),
		project:      strings.TrimSpace(project),
		repo:         strings.TrimSpace(repo),
	}
}

func (h *Host) Provider() scm.Provider { return scm.ProviderAzureDevOps }

// Capabilities reports the Azure DevOps feature matrix. Merge status is
// reliably available from `az repos pr show`. Failed-check log fetching is not
// yet wired up - the az CLI has no first-class build-log command, so callers
// gate on FailedCheckLogs and skip it.
func (h *Host) Capabilities() scm.Capabilities {
	return scm.Capabilities{MergeableState: true, FailedCheckLogs: false}
}

// orgArgs scopes a command to the organization. The show/update/policy-list
// commands accept only --organization because the PR id is organization-unique;
// passing --project/--repository to them is rejected by az.
func (h *Host) orgArgs() []string {
	if h.org == "" {
		return nil
	}
	return []string{"--organization", h.org}
}

// scopeArgs fully scopes a command to org/project/repo. The create and list
// commands need all three to resolve the repository.
func (h *Host) scopeArgs() []string {
	args := h.orgArgs()
	if h.project != "" {
		args = append(args, "--project", h.project)
	}
	if h.repo != "" {
		args = append(args, "--repository", h.repo)
	}
	return args
}

func (h *Host) Available(ctx context.Context) error {
	if h.cliAvailable != nil && !h.cliAvailable() {
		return errors.New("az CLI is not installed")
	}
	// The azure-devops extension is separate from the az binary; without it
	// every `az repos`/`az devops` command fails. Probe it for a clear message.
	if err := scm.RunAuthProbe(func() *exec.Cmd { return h.cmd(ctx, "az", "extension", "show", "--name", "azure-devops") }); err != nil {
		return errors.New("az azure-devops extension is not installed (run: az extension add --name azure-devops)")
	}
	// Auth probe: an organization-scoped read exercises the PAT
	// (AZURE_DEVOPS_EXT_PAT, or `az devops login`) against this organization.
	args := []string{"devops", "project", "list", "--query", "value[0].id", "--output", "tsv"}
	args = append(args, h.orgArgs()...)
	if err := scm.RunAuthProbe(func() *exec.Cmd { return h.cmd(ctx, "az", args...) }); err != nil {
		return errors.New("az CLI is not authenticated for Azure DevOps")
	}
	return nil
}

func (h *Host) FindPR(ctx context.Context, branch, base string) (*scm.PR, error) {
	args := []string{"repos", "pr", "list", "--source-branch", branch, "--status", "active"}
	if strings.TrimSpace(base) != "" {
		args = append(args, "--target-branch", base)
	}
	args = append(args, h.scopeArgs()...)
	args = append(args, "--output", "json")
	out, err := outputJSON(h.cmd(ctx, "az", args...))
	if err != nil {
		return nil, fmt.Errorf("az repos pr list: %w", err)
	}
	if len(bytes.TrimSpace(out)) == 0 {
		return nil, nil
	}
	var prs []azPR
	if err := json.Unmarshal(out, &prs); err != nil {
		return nil, fmt.Errorf("az repos pr list: parse response: %w", err)
	}
	if len(prs) == 0 {
		return nil, nil
	}
	return h.toPR(&prs[0]), nil
}

func (h *Host) CreatePR(ctx context.Context, branch, base string, content scm.PRContent) (*scm.PR, error) {
	args := []string{"repos", "pr", "create",
		"--source-branch", branch,
		"--target-branch", base,
		"--title", content.Title,
		"--description", clampDescription(content.Body),
	}
	args = append(args, h.scopeArgs()...)
	args = append(args, "--output", "json")
	out, err := outputJSON(h.cmd(ctx, "az", args...))
	if err != nil {
		return nil, fmt.Errorf("az repos pr create: %w", err)
	}
	var pr azPR
	if err := json.Unmarshal(out, &pr); err != nil {
		return nil, fmt.Errorf("az repos pr create: parse response: %w", err)
	}
	return h.toPR(&pr), nil
}

func (h *Host) UpdatePR(ctx context.Context, pr *scm.PR, content scm.PRContent) (*scm.PR, error) {
	id := h.prID(pr)
	if id == "" {
		return nil, errors.New("az repos pr update: missing PR id")
	}
	args := []string{"repos", "pr", "update", "--id", id,
		"--title", content.Title,
		"--description", clampDescription(content.Body),
	}
	args = append(args, h.orgArgs()...)
	args = append(args, "--output", "json")
	if _, err := outputJSON(h.cmd(ctx, "az", args...)); err != nil {
		return nil, fmt.Errorf("az repos pr update: %w", err)
	}
	return pr, nil
}

func (h *Host) GetPRState(ctx context.Context, pr *scm.PR) (scm.PRState, error) {
	got, err := h.showPR(ctx, pr)
	if err != nil {
		return "", err
	}
	return normalizePRState(got.Status), nil
}

func (h *Host) GetChecks(ctx context.Context, pr *scm.PR) ([]scm.Check, error) {
	id := h.prID(pr)
	if id == "" {
		return nil, errors.New("az repos pr policy list: missing PR id")
	}
	args := append([]string{"repos", "pr", "policy", "list", "--id", id}, h.orgArgs()...)
	args = append(args, "--output", "json")
	out, err := outputJSON(h.cmd(ctx, "az", args...))
	if err != nil {
		return nil, fmt.Errorf("az repos pr policy list: %w", err)
	}
	var evals []policyEval
	if err := json.Unmarshal(out, &evals); err != nil {
		return nil, fmt.Errorf("parse policy evaluations: %w", err)
	}
	checks := make([]scm.Check, 0, len(evals))
	for _, e := range evals {
		if !e.isCICheck() {
			continue
		}
		bucket := azStatusBucket(e.Status)
		if bucket == "" {
			continue
		}
		checks = append(checks, scm.Check{
			Name:        e.checkName(),
			Bucket:      bucket,
			CompletedAt: parseAzTime(e.CompletedDate),
		})
	}
	return checks, nil
}

func (h *Host) GetMergeableState(ctx context.Context, pr *scm.PR) (scm.MergeableState, error) {
	got, err := h.showPR(ctx, pr)
	if err != nil {
		return "", err
	}
	return normalizeMergeableState(got.MergeStatus), nil
}

// FetchFailedCheckLogs is not yet implemented for Azure DevOps; callers gate on
// Capabilities().FailedCheckLogs (false) and skip it.
func (h *Host) FetchFailedCheckLogs(_ context.Context, _ *scm.PR, _ string, _ string, _ []string) (string, error) {
	return "", scm.ErrUnsupported
}

func (h *Host) showPR(ctx context.Context, pr *scm.PR) (*azPR, error) {
	id := h.prID(pr)
	if id == "" {
		return nil, errors.New("az repos pr show: missing PR id")
	}
	args := append([]string{"repos", "pr", "show", "--id", id}, h.orgArgs()...)
	args = append(args, "--output", "json")
	out, err := outputJSON(h.cmd(ctx, "az", args...))
	if err != nil {
		return nil, fmt.Errorf("az repos pr show: %w", err)
	}
	var got azPR
	if err := json.Unmarshal(out, &got); err != nil {
		return nil, fmt.Errorf("parse pull request: %w", err)
	}
	return &got, nil
}

func (h *Host) prID(pr *scm.PR) string {
	if pr == nil {
		return ""
	}
	if id := strings.TrimSpace(pr.Number); id != "" {
		return id
	}
	if num, err := scm.ExtractPRNumber(pr.URL); err == nil {
		return num
	}
	return ""
}

func (h *Host) toPR(raw *azPR) *scm.PR {
	if raw == nil {
		return nil
	}
	id := ""
	if raw.PullRequestID > 0 {
		id = strconv.Itoa(raw.PullRequestID)
	}
	return &scm.PR{
		Number: id,
		URL:    webPRURL(h.org, h.project, h.repo, raw.Repository.WebURL, id),
	}
}
