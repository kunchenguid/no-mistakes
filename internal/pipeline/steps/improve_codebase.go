package steps

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const (
	improveCodebaseSourceFileThreshold = 8
	improveCodebaseLineThreshold       = 800
)

// ImproveCodebaseStep runs a conditional structural codebase-health gate.
type ImproveCodebaseStep struct{}

func (s *ImproveCodebaseStep) Name() types.StepName { return types.StepImproveCodebase }

type improveCodebaseChangedFile struct {
	Path      string `json:"path"`
	OldPath   string `json:"old_path,omitempty"`
	Status    string `json:"status"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
}

type improveCodebaseDecision struct {
	Run    bool
	Reason string
}

func (s *ImproveCodebaseStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	mode := config.ImproveCodebaseModeAuto
	if sctx.Config != nil && strings.TrimSpace(sctx.Config.ImproveCodebase.Mode) != "" {
		mode = strings.ToLower(strings.TrimSpace(sctx.Config.ImproveCodebase.Mode))
	}

	switch mode {
	case config.ImproveCodebaseModeOff:
		sctx.Log("improve-codebase disabled by config")
		return &pipeline.StepOutcome{Skipped: true}, nil
	case config.ImproveCodebaseModeAlways, config.ImproveCodebaseModeAuto:
	default:
		return nil, fmt.Errorf("invalid improve-codebase mode %q", mode)
	}

	baseSHA := resolveBranchBaseSHA(sctx.Ctx, sctx.WorkDir, sctx.Run.BaseSHA, sctx.Repo.DefaultBranch)
	files, err := improveCodebaseChangedFiles(sctx, baseSHA)
	if err != nil {
		return nil, err
	}

	decision := improveCodebaseShouldRun(mode, files)
	if !decision.Run {
		sctx.Log("skipping improve-codebase: " + decision.Reason)
		findingsJSON, _ := json.Marshal(Findings{Summary: decision.Reason})
		return &pipeline.StepOutcome{Skipped: true, Findings: string(findingsJSON)}, nil
	}

	sctx.Log("running improve-codebase gate: " + decision.Reason)
	reviewScope := fmt.Sprintf("branch changes between %s and %s", baseSHA, sctx.Run.HeadSHA)
	if sctx.Fixing {
		reviewScope = fmt.Sprintf("current worktree and HEAD changes relative to base commit %s (starting head %s)", baseSHA, sctx.Run.HeadSHA)
	}
	auditDir, cleanupAuditDir, err := prepareImproveCodebaseAuditCheckout(sctx)
	if err != nil {
		return nil, err
	}
	changeSetSection, err := improveCodebaseChangeSetPromptSection(files)
	if err != nil {
		return nil, err
	}
	defer cleanupAuditDir()
	historySection := executionContextPromptSection() + roundHistoryPromptSection(sctx) + userIntentPromptSection(sctx)
	prompt := fmt.Sprintf(
		`Run the local improve-codebase skill as a read-only structural/change-set gate.

Context:
- branch: %s
- base commit: %s
- target commit: %s
- review scope: %s
- default branch: %s
- trigger reason: %s
- ignore patterns: %s

Changed file diff stats:
%s

Task:
- Use the installed local improve-codebase skill when available.
- Use its no-mistakes pipeline gate mode when the skill provides one.
- Narrow the audit to the current change-set and touched areas: changed files, callers, tests, configs, public surfaces, and nearby module boundaries needed to understand impact.
- Exclude files and paths matched by ignore_patterns from findings.
- Focus on structural regressions introduced or exposed by this change: shallow modules, misplaced code, duplicated operational mechanics, unsafe compatibility shims, weak boundaries, oversized or fragmented files, and test topology that makes the change hard to validate.
- Do not run the full whole-codebase audit unless the changed scope genuinely requires it to understand a blocker.
- Do not edit files, update docs, create artifacts, run formatters, commit changes, or enter a grilling loop.
- Do not run tests. The pipeline has a dedicated test step after this gate.

Rules:
- Return only findings that matter to ship readiness for this change.
- Use severity "error" for structural or codebase-health issues that should block merge.
- Use severity "warning" for material structural risks the author should decide on before merge.
- Use severity "info" only for non-blocking notes.
- Set action to "ask-user" for blockers or material warnings that need a human decision.
- Set action to "no-op" for informational notes.
- Do not use "auto-fix"; this gate is audit-only.
- Anchor every finding to a specific file and one-indexed line number when possible.
- If no material structural issue is found, return an empty findings array.%s`,
		sctx.Run.Branch,
		baseSHA,
		sctx.Run.HeadSHA,
		reviewScope,
		sctx.Repo.DefaultBranch,
		decision.Reason,
		formatImproveCodebaseIgnorePatterns(sctx.Config),
		changeSetSection,
		historySection,
	)

	result, agentErr := sctx.Agent.Run(sctx.Ctx, agent.RunOpts{
		Prompt:     prompt,
		CWD:        auditDir,
		ServerCWD:  sctx.WorkDir,
		JSONSchema: auditOnlyFindingsSchema,
		OnChunk:    sctx.LogChunk,
		ReadOnly:   true,
	})
	if agentErr != nil {
		if mode == config.ImproveCodebaseModeAuto && errors.Is(agentErr, agent.ErrReadOnlyUnsupported) {
			reason := "read-only mode unsupported by configured agent; improve-codebase auto gate skipped"
			sctx.Log("skipping improve-codebase: " + reason)
			findingsJSON, _ := json.Marshal(Findings{Summary: reason})
			return &pipeline.StepOutcome{Skipped: true, Findings: string(findingsJSON)}, nil
		}
		return nil, fmt.Errorf("agent improve-codebase: %w", agentErr)
	}

	var findings Findings
	if result.Output != nil {
		if err := json.Unmarshal(result.Output, &findings); err != nil {
			sctx.Log("could not parse structured output, using text response")
			findings = Findings{Summary: result.Text}
		}
	}
	normalizeImproveCodebaseAuditActions(&findings)

	findingsJSON, _ := json.Marshal(findings)
	return &pipeline.StepOutcome{
		NeedsApproval: hasBlockingFindings(findings.Items),
		AutoFixable:   false,
		DisableFix:    true,
		Findings:      string(findingsJSON),
	}, nil
}

func prepareImproveCodebaseAuditCheckout(sctx *pipeline.StepContext) (string, func(), error) {
	auditDir, err := os.MkdirTemp("", "no-mistakes-improve-codebase-*")
	if err != nil {
		return "", nil, fmt.Errorf("create improve-codebase audit checkout: %w", err)
	}
	cleanup := func() {
		if err := os.RemoveAll(auditDir); err != nil {
			sctx.LogFile("cleanup improve-codebase audit checkout: " + err.Error())
		}
	}
	if _, err := git.Run(sctx.Ctx, sctx.WorkDir, "clone", "--no-local", "--no-checkout", sctx.WorkDir, auditDir); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("create improve-codebase audit checkout: %w", err)
	}
	if _, err := git.Run(sctx.Ctx, auditDir, "checkout", "--detach", sctx.Run.HeadSHA); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("checkout improve-codebase audit head: %w", err)
	}
	if _, err := git.Run(sctx.Ctx, auditDir, "reset", "--hard", sctx.Run.HeadSHA); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("reset improve-codebase audit checkout: %w", err)
	}
	if _, err := git.Run(sctx.Ctx, auditDir, "clean", "-ffdx"); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("clean improve-codebase audit checkout: %w", err)
	}
	return auditDir, cleanup, nil
}

func improveCodebaseChangeSetPromptSection(files []improveCodebaseChangedFile) (string, error) {
	data, err := json.MarshalIndent(files, "", "  ")
	if err != nil {
		return "", fmt.Errorf("format improve-codebase changed files: %w", err)
	}
	return string(data), nil
}

func normalizeImproveCodebaseAuditActions(findings *Findings) {
	for i := range findings.Items {
		switch findings.Items[i].Action {
		case types.ActionAutoFix, "":
			if findings.Items[i].Severity == "info" {
				findings.Items[i].Action = types.ActionNoOp
			} else {
				findings.Items[i].Action = types.ActionAskUser
			}
		}
	}
}

func improveCodebaseChangedFiles(sctx *pipeline.StepContext, baseSHA string) ([]improveCodebaseChangedFile, error) {
	diffRange := baseSHA + ".." + sctx.Run.HeadSHA
	if sctx.Fixing {
		diffRange = baseSHA
	}
	nameStatus, err := git.Run(sctx.Ctx, sctx.WorkDir, "diff", "--name-status", "-M", diffRange)
	if err != nil {
		return nil, fmt.Errorf("get improve-codebase changed files: %w", err)
	}
	numstat, err := git.Run(sctx.Ctx, sctx.WorkDir, "diff", "--numstat", "-M", "-z", diffRange)
	if err != nil {
		return nil, fmt.Errorf("get improve-codebase diff stats: %w", err)
	}
	stats := parseImproveCodebaseNumstat(numstat)

	var files []improveCodebaseChangedFile
	for _, line := range strings.Split(nameStatus, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) < 2 {
			continue
		}
		status := parts[0]
		path := parts[len(parts)-1]
		oldPath := ""
		if (strings.HasPrefix(status, "R") || strings.HasPrefix(status, "C")) && len(parts) >= 3 {
			oldPath = parts[1]
		}
		if ignoredByConfig(path, sctx.Config) {
			continue
		}
		file := improveCodebaseChangedFile{Path: path, OldPath: oldPath, Status: status}
		if st, ok := stats[path]; ok {
			file.Additions = st.Additions
			file.Deletions = st.Deletions
		}
		files = append(files, file)
	}
	return files, nil
}

func formatImproveCodebaseIgnorePatterns(cfg *config.Config) string {
	if cfg == nil || len(cfg.IgnorePatterns) == 0 {
		return "none"
	}
	return strings.Join(cfg.IgnorePatterns, ", ")
}

func parseImproveCodebaseNumstat(text string) map[string]struct{ Additions, Deletions int } {
	out := map[string]struct{ Additions, Deletions int }{}
	if strings.Contains(text, "\x00") {
		parseImproveCodebaseNumstatZ(text, out)
		return out
	}
	for _, line := range strings.Split(text, "\n") {
		additionsText, rest, ok := strings.Cut(line, "\t")
		if !ok {
			continue
		}
		deletionsText, path, ok := strings.Cut(rest, "\t")
		if !ok {
			continue
		}
		additions := parseNumstatValue(additionsText)
		deletions := parseNumstatValue(deletionsText)
		path = normalizeImproveCodebaseNumstatPath(path)
		out[path] = struct{ Additions, Deletions int }{Additions: additions, Deletions: deletions}
	}
	return out
}

func parseImproveCodebaseNumstatZ(text string, out map[string]struct{ Additions, Deletions int }) {
	fields := strings.Split(text, "\x00")
	for i := 0; i < len(fields); {
		record := fields[i]
		i++
		if record == "" {
			continue
		}
		additionsText, rest, ok := strings.Cut(record, "\t")
		if !ok {
			continue
		}
		deletionsText, path, ok := strings.Cut(rest, "\t")
		if !ok {
			continue
		}
		if path == "" {
			if i+1 >= len(fields) {
				continue
			}
			i++
			path = fields[i]
			i++
		}
		additions := parseNumstatValue(additionsText)
		deletions := parseNumstatValue(deletionsText)
		out[path] = struct{ Additions, Deletions int }{Additions: additions, Deletions: deletions}
	}
}

func normalizeImproveCodebaseNumstatPath(path string) string {
	if !strings.Contains(path, " => ") {
		return path
	}
	open := strings.Index(path, "{")
	close := strings.Index(path, "}")
	arrow := strings.Index(path, " => ")
	if open >= 0 && close > arrow && arrow > open {
		return path[:open] + strings.TrimSpace(path[arrow+4:close]) + path[close+1:]
	}
	return strings.TrimSpace(path[arrow+4:])
}

func parseNumstatValue(value string) int {
	var n int
	if _, err := fmt.Sscanf(value, "%d", &n); err != nil {
		return 0
	}
	return n
}

func ignoredByConfig(path string, cfg *config.Config) bool {
	if cfg == nil {
		return false
	}
	for _, pattern := range cfg.IgnorePatterns {
		if matchIgnorePattern(path, pattern) {
			return true
		}
	}
	return false
}

func improveCodebaseShouldRun(mode string, files []improveCodebaseChangedFile) improveCodebaseDecision {
	if mode == config.ImproveCodebaseModeAlways {
		return improveCodebaseDecision{Run: true, Reason: "mode is always"}
	}
	if len(files) == 0 {
		return improveCodebaseDecision{Reason: "no changed files after ignore patterns"}
	}

	sourceFiles := 0
	changedLines := 0
	for _, file := range files {
		changedLines += file.Additions + file.Deletions
		if isImproveCodebaseSourceFile(file.Path) {
			sourceFiles++
		}
		if isCrossDirectoryMove(file) {
			return improveCodebaseDecision{Run: true, Reason: "file moved across directories"}
		}
		if isImproveCodebaseHighRiskPath(file.Path) {
			return improveCodebaseDecision{Run: true, Reason: "high-risk structural path changed"}
		}
		if isImproveCodebasePublicSurfacePath(file.Path) {
			return improveCodebaseDecision{Run: true, Reason: "public surface or boundary file changed"}
		}
	}
	if sourceFiles > improveCodebaseSourceFileThreshold {
		return improveCodebaseDecision{Run: true, Reason: fmt.Sprintf("%d source files changed", sourceFiles)}
	}
	if changedLines > improveCodebaseLineThreshold {
		return improveCodebaseDecision{Run: true, Reason: fmt.Sprintf("%d changed lines", changedLines)}
	}
	return improveCodebaseDecision{Reason: "change-set is small and not structurally risky"}
}

func isCrossDirectoryMove(file improveCodebaseChangedFile) bool {
	if file.OldPath == "" {
		return false
	}
	return filepath.Dir(file.OldPath) != filepath.Dir(file.Path)
}

func isImproveCodebaseSourceFile(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".go", ".js", ".jsx", ".ts", ".tsx", ".py", ".rb", ".rs", ".java", ".kt", ".c", ".cc", ".cpp", ".h", ".hpp", ".cs", ".php", ".swift":
		return !isGeneratedOrVendoredPath(path)
	default:
		return false
	}
}

func isGeneratedOrVendoredPath(path string) bool {
	lower := strings.ToLower(filepath.ToSlash(path))
	return strings.HasPrefix(lower, "vendor/") ||
		strings.HasPrefix(lower, "node_modules/") ||
		strings.Contains(lower, "/vendor/") ||
		strings.Contains(lower, "/node_modules/") ||
		strings.Contains(lower, ".generated.") ||
		strings.HasSuffix(lower, "_generated.go") ||
		strings.HasSuffix(lower, ".pb.go")
}

func isImproveCodebaseHighRiskPath(path string) bool {
	lower := strings.ToLower(filepath.ToSlash(path))
	base := filepath.Base(lower)
	if strings.HasPrefix(lower, ".github/") ||
		strings.HasPrefix(lower, ".gitlab/") ||
		strings.HasPrefix(lower, "infra/") ||
		strings.HasPrefix(lower, "terraform/") ||
		strings.HasPrefix(lower, "deploy/") ||
		strings.HasPrefix(lower, "k8s/") ||
		strings.HasPrefix(lower, "helm/") {
		return true
	}
	switch base {
	case ".no-mistakes.yaml", "go.mod", "go.sum", "package.json", "package-lock.json", "pnpm-lock.yaml", "yarn.lock", "cargo.toml", "cargo.lock", "pyproject.toml", "requirements.txt", "makefile", "dockerfile":
		return true
	default:
		return strings.HasSuffix(lower, ".github/workflows") || strings.Contains(lower, "/workflows/")
	}
}

func isImproveCodebasePublicSurfacePath(path string) bool {
	lower := strings.ToLower(filepath.ToSlash(path))
	base := strings.TrimSuffix(filepath.Base(lower), filepath.Ext(lower))
	keywords := []string{
		"api", "auth", "client", "command", "config", "handler", "interface",
		"middleware", "provider", "repository", "server", "service", "adapter",
		"pipeline", "executor", "workflow",
	}
	for _, keyword := range keywords {
		if base == keyword || strings.Contains(base, keyword) || strings.Contains(lower, "/"+keyword+"/") {
			return true
		}
	}
	return false
}
