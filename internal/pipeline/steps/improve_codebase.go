package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const (
	improveCodebaseSourceFileThreshold = 8
	improveCodebaseLineThreshold       = 800
	gitZeroSHA                         = "0000000000000000000000000000000000000000"
)

// ImproveCodebaseStep runs a conditional structural codebase-health gate.
type ImproveCodebaseStep struct{}

func (s *ImproveCodebaseStep) Name() types.StepName { return types.StepImproveCodebase }

type improveCodebaseChangedFile struct {
	Path      string
	OldPath   string
	Status    string
	Additions int
	Deletions int
}

type improveCodebaseDecision struct {
	Run    bool
	Reason string
}

type improveCodebaseReadOnlySnapshot struct {
	Head    string
	HeadRef string
	Status  string
	Refs    map[string]improveCodebaseRefSnapshot
}

type improveCodebaseRefSnapshot struct {
	Object string
	Symref string
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
	beforeSnapshot, err := snapshotImproveCodebaseReadOnly(sctx)
	if err != nil {
		return nil, err
	}
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
		historySection,
	)

	result, agentErr := sctx.Agent.Run(sctx.Ctx, agent.RunOpts{
		Prompt:     prompt,
		CWD:        sctx.WorkDir,
		JSONSchema: auditOnlyFindingsSchema,
		OnChunk:    sctx.LogChunk,
		ReadOnly:   true,
	})
	if err := enforceImproveCodebaseReadOnly(sctx, beforeSnapshot); err != nil {
		return nil, err
	}
	if agentErr != nil {
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
	numstat, err := git.Run(sctx.Ctx, sctx.WorkDir, "diff", "--numstat", diffRange)
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

func snapshotImproveCodebaseReadOnly(sctx *pipeline.StepContext) (improveCodebaseReadOnlySnapshot, error) {
	head, err := git.Run(sctx.Ctx, sctx.WorkDir, "rev-parse", "HEAD")
	if err != nil {
		return improveCodebaseReadOnlySnapshot{}, fmt.Errorf("snapshot improve-codebase HEAD: %w", err)
	}
	headRef, err := git.Run(sctx.Ctx, sctx.WorkDir, "rev-parse", "--symbolic-full-name", "HEAD")
	if err != nil {
		return improveCodebaseReadOnlySnapshot{}, fmt.Errorf("snapshot improve-codebase HEAD ref: %w", err)
	}
	status, err := git.Run(sctx.Ctx, sctx.WorkDir, "status", "--porcelain", "--ignored")
	if err != nil {
		return improveCodebaseReadOnlySnapshot{}, fmt.Errorf("snapshot improve-codebase worktree status: %w", err)
	}
	refs, err := snapshotImproveCodebaseRefs(sctx)
	if err != nil {
		return improveCodebaseReadOnlySnapshot{}, err
	}
	return improveCodebaseReadOnlySnapshot{Head: head, HeadRef: headRef, Status: status, Refs: refs}, nil
}

func snapshotImproveCodebaseRefs(sctx *pipeline.StepContext) (map[string]improveCodebaseRefSnapshot, error) {
	refs := map[string]improveCodebaseRefSnapshot{}
	output, err := git.Run(sctx.Ctx, sctx.WorkDir, "for-each-ref", "--format=%(refname)%00%(objectname)%00%(symref)", "refs")
	if err != nil {
		return nil, fmt.Errorf("snapshot improve-codebase refs: %w", err)
	}
	for _, line := range strings.Split(strings.TrimSuffix(output, "\n"), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\x00", 3)
		if len(parts) != 3 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
			return nil, fmt.Errorf("snapshot improve-codebase refs: malformed ref line %q", line)
		}
		refs[parts[0]] = improveCodebaseRefSnapshot{Object: parts[1], Symref: parts[2]}
	}
	return refs, nil
}

func enforceImproveCodebaseReadOnly(sctx *pipeline.StepContext, before improveCodebaseReadOnlySnapshot) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(sctx.Ctx), 30*time.Second)
	defer cancel()
	cleanupSctx := *sctx
	cleanupSctx.Ctx = cleanupCtx

	after, err := snapshotImproveCodebaseReadOnly(&cleanupSctx)
	if err != nil {
		return err
	}
	if improveCodebaseReadOnlySnapshotEqual(before, after) {
		return nil
	}
	if _, checkoutErr := git.Run(cleanupCtx, sctx.WorkDir, "checkout", "--force", "--detach", before.Head); checkoutErr != nil {
		return fmt.Errorf("improve-codebase gate modified the worktree or git refs and cleanup failed: %w", checkoutErr)
	}
	for ref, beforeRef := range before.Refs {
		afterRef, exists := after.Refs[ref]
		if exists && improveCodebaseRefSnapshotEqual(afterRef, beforeRef) {
			continue
		}
		if refErr := restoreImproveCodebaseRef(cleanupCtx, sctx.WorkDir, ref, beforeRef, afterRef, exists); refErr != nil {
			return fmt.Errorf("improve-codebase gate modified the worktree or git refs and cleanup failed: %w", refErr)
		}
	}
	for ref, afterRef := range after.Refs {
		if _, ok := before.Refs[ref]; ok {
			continue
		}
		if refErr := removeImproveCodebaseRef(cleanupCtx, sctx.WorkDir, ref, afterRef); refErr != nil {
			return fmt.Errorf("improve-codebase gate modified the worktree or git refs and cleanup failed: %w", refErr)
		}
	}
	if _, resetErr := git.Run(cleanupCtx, sctx.WorkDir, "reset", "--hard", before.Head); resetErr != nil {
		return fmt.Errorf("improve-codebase gate modified the worktree or git refs and cleanup failed: %w", resetErr)
	}
	if _, cleanErr := git.Run(cleanupCtx, sctx.WorkDir, "clean", "-ffdx"); cleanErr != nil {
		return fmt.Errorf("improve-codebase gate modified the worktree or git refs and cleanup failed: %w", cleanErr)
	}
	if before.HeadRef != "HEAD" {
		if _, headErr := git.Run(cleanupCtx, sctx.WorkDir, "symbolic-ref", "HEAD", before.HeadRef); headErr != nil {
			return fmt.Errorf("improve-codebase gate modified the worktree or git refs and cleanup failed: %w", headErr)
		}
	}
	return fmt.Errorf("improve-codebase gate modified the worktree or git refs despite read-only mode")
}

func improveCodebaseReadOnlySnapshotEqual(a, b improveCodebaseReadOnlySnapshot) bool {
	if a.Head != b.Head || a.HeadRef != b.HeadRef || a.Status != b.Status || len(a.Refs) != len(b.Refs) {
		return false
	}
	for ref, aRef := range a.Refs {
		if !improveCodebaseRefSnapshotEqual(aRef, b.Refs[ref]) {
			return false
		}
	}
	return true
}

func improveCodebaseRefSnapshotEqual(a, b improveCodebaseRefSnapshot) bool {
	return a.Object == b.Object && a.Symref == b.Symref
}

func restoreImproveCodebaseRef(ctx context.Context, workDir, ref string, before, after improveCodebaseRefSnapshot, afterExists bool) error {
	if before.Symref != "" {
		if afterExists {
			if err := ensureImproveCodebaseRefUnchanged(ctx, workDir, ref, after); err != nil {
				return err
			}
		}
		_, err := git.Run(ctx, workDir, "symbolic-ref", ref, before.Symref)
		return err
	}
	if !afterExists {
		_, err := git.Run(ctx, workDir, "update-ref", ref, before.Object, gitZeroSHA)
		return err
	}
	if after.Symref != "" {
		if err := ensureImproveCodebaseRefUnchanged(ctx, workDir, ref, after); err != nil {
			return err
		}
		if _, err := git.Run(ctx, workDir, "update-ref", "--no-deref", "-d", ref); err != nil {
			return err
		}
		_, err := git.Run(ctx, workDir, "update-ref", ref, before.Object, gitZeroSHA)
		return err
	}
	_, err := git.Run(ctx, workDir, "update-ref", ref, before.Object, after.Object)
	return err
}

func removeImproveCodebaseRef(ctx context.Context, workDir, ref string, after improveCodebaseRefSnapshot) error {
	if after.Symref != "" {
		if err := ensureImproveCodebaseRefUnchanged(ctx, workDir, ref, after); err != nil {
			return err
		}
		_, err := git.Run(ctx, workDir, "symbolic-ref", "--delete", ref)
		return err
	}
	_, err := git.Run(ctx, workDir, "update-ref", "-d", ref, after.Object)
	return err
}

func ensureImproveCodebaseRefUnchanged(ctx context.Context, workDir, ref string, want improveCodebaseRefSnapshot) error {
	current, exists, err := currentImproveCodebaseRefSnapshot(ctx, workDir, ref)
	if err != nil {
		return err
	}
	if !exists || !improveCodebaseRefSnapshotEqual(current, want) {
		return fmt.Errorf("ref %s changed during cleanup", ref)
	}
	return nil
}

func currentImproveCodebaseRefSnapshot(ctx context.Context, workDir, ref string) (improveCodebaseRefSnapshot, bool, error) {
	output, err := git.Run(ctx, workDir, "for-each-ref", "--format=%(refname)%00%(objectname)%00%(symref)", ref)
	if err != nil {
		return improveCodebaseRefSnapshot{}, false, err
	}
	for _, line := range strings.Split(strings.TrimSuffix(output, "\n"), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\x00", 3)
		if len(parts) != 3 || parts[0] != ref || strings.TrimSpace(parts[1]) == "" {
			return improveCodebaseRefSnapshot{}, false, fmt.Errorf("snapshot improve-codebase ref %s: malformed ref line %q", ref, line)
		}
		return improveCodebaseRefSnapshot{Object: parts[1], Symref: parts[2]}, true, nil
	}
	return improveCodebaseRefSnapshot{}, false, nil
}

func formatImproveCodebaseIgnorePatterns(cfg *config.Config) string {
	if cfg == nil || len(cfg.IgnorePatterns) == 0 {
		return "none"
	}
	return strings.Join(cfg.IgnorePatterns, ", ")
}

func parseImproveCodebaseNumstat(text string) map[string]struct{ Additions, Deletions int } {
	out := map[string]struct{ Additions, Deletions int }{}
	for _, line := range strings.Split(text, "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) < 3 {
			continue
		}
		additions := parseNumstatValue(fields[0])
		deletions := parseNumstatValue(fields[1])
		path := fields[len(fields)-1]
		out[path] = struct{ Additions, Deletions int }{Additions: additions, Deletions: deletions}
	}
	return out
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
