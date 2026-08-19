package pipeline

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/intent"
	"github.com/kunchenguid/no-mistakes/internal/safeurl"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const (
	reviewFleetReadOnlySandbox      = "read-only"
	reviewFleetMaxArgBytes          = 4096
	reviewFleetMaxAuthBytes         = 4 * 1024 * 1024
	reviewFleetMaxRuntimeLogBytes   = 2048
	reviewFleetMaxExportFileBytes   = 8 * 1024 * 1024
	reviewFleetMaxExportTotalBytes  = 64 * 1024 * 1024
	reviewFleetMaxExportEntries     = 100000
	reviewFleetMaxTreeMetadataBytes = 32 * 1024 * 1024
	reviewFleetMaxReviewDataBytes   = 8 * 1024 * 1024
)

const reviewFleetContractVersion = 1

// reviewFleetSettingsFromConfig projects the trusted global-only config into
// the execution types used by the pipeline. The fixed order is deliberate:
// reviewer completion is concurrent, but configuration and test evidence stay
// deterministic.
func reviewFleetSettingsFromConfig(cfg *config.Config) (*ReviewFleetSettings, error) {
	return reviewFleetSettingsFromConfigForSource(cfg, "")
}

func reviewFleetSettingsFromConfigForSource(cfg *config.Config, sourceRoot string) (*ReviewFleetSettings, error) {
	if cfg == nil {
		return nil, nil
	}
	settings := &ReviewFleetSettings{Enabled: cfg.ReviewFleet.Enabled}
	if !settings.Enabled {
		return settings, nil
	}
	executable, err := resolveReviewFleetExecutable(cfg.AgentPathFor(types.AgentCodex), sourceRoot)
	if err != nil {
		return nil, err
	}
	settings.CodexExecutable = executable
	settings.CodexExecutableDigest, err = reviewFleetExecutableDigest(executable)
	if err != nil {
		return nil, err
	}
	roles := []string{
		config.ReviewFleetRoleTestAdversary,
		config.ReviewFleetRoleCorrectness,
		config.ReviewFleetRoleArchitecture,
		config.ReviewFleetRoleSecurity,
	}
	settings.Reviewers = make([]ReviewProfile, 0, len(roles))
	for _, role := range roles {
		profile, ok := cfg.ReviewFleet.Reviewers[role]
		if !ok {
			return nil, fmt.Errorf("review fleet config is missing reviewer %q", role)
		}
		settings.Reviewers = append(settings.Reviewers, projectReviewFleetProfile(role, profile))
	}
	settings.Consolidator = projectReviewFleetProfile(config.ReviewFleetProfileConsolidator, cfg.ReviewFleet.Consolidator)
	settings.Certifier = projectReviewFleetProfile(config.ReviewFleetProfileCertifier, cfg.ReviewFleet.Certifier)
	settings.CodexProfileArgs = func(profile ReviewProfile) ([]string, error) {
		return cfg.ReviewFleetCodexArgs(profile.Role, profile.SecurityEscalated)
	}
	return settings, nil
}

func resolveReviewFleetCodexExecutable(configured string) (string, error) {
	return resolveReviewFleetExecutable(configured, "")
}

// resolveReviewFleetExecutable resolves an executable before a repository
// checkout can influence a fleet subprocess. sourceRoot is optional for the
// configuration-only callers; fleet execution always supplies it.
func resolveReviewFleetExecutable(configured, sourceRoot string) (string, error) {
	configured = strings.TrimSpace(configured)
	if configured == "" {
		return "", fmt.Errorf("review fleet executable is empty")
	}
	var executable string
	var err error
	if filepath.IsAbs(configured) {
		executable = configured
	} else {
		pathValue, pathErr := reviewFleetCanonicalPATH(sourceRoot, os.Getenv("PATH"))
		if pathErr != nil {
			return "", pathErr
		}
		for _, dir := range filepath.SplitList(pathValue) {
			candidate := filepath.Join(dir, configured)
			if runtime.GOOS == "windows" && filepath.Ext(candidate) == "" {
				candidate += ".exe"
			}
			info, statErr := os.Stat(candidate)
			if statErr == nil && info.Mode().IsRegular() && (runtime.GOOS == "windows" || info.Mode().Perm()&0o111 != 0) {
				executable = candidate
				break
			}
		}
		if executable == "" {
			return "", fmt.Errorf("resolve review fleet executable from canonical PATH")
		}
	}
	if !filepath.IsAbs(executable) {
		executable, err = filepath.Abs(executable)
		if err != nil {
			return "", fmt.Errorf("resolve absolute review fleet Codex executable: %w", err)
		}
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return "", fmt.Errorf("resolve canonical review fleet Codex executable: %w", err)
	}
	if !filepath.IsAbs(executable) {
		return "", fmt.Errorf("resolved review fleet Codex executable is not absolute")
	}
	if sourceRoot != "" {
		canonicalRoot, rootErr := filepath.EvalSymlinks(sourceRoot)
		if rootErr != nil {
			return "", fmt.Errorf("canonicalize review fleet source root: %w", rootErr)
		}
		if reviewFleetPathWithin(canonicalRoot, executable) {
			return "", fmt.Errorf("resolved review fleet executable is inside the source worktree")
		}
	}
	return executable, nil
}

func reviewFleetCanonicalPATH(sourceRoot, pathValue string) (string, error) {
	canonicalRoot := ""
	if sourceRoot != "" {
		var err error
		canonicalRoot, err = filepath.EvalSymlinks(sourceRoot)
		if err != nil {
			return "", fmt.Errorf("canonicalize review fleet source root: %w", err)
		}
	}
	dirs := make([]string, 0, len(filepath.SplitList(pathValue)))
	for _, entry := range filepath.SplitList(pathValue) {
		if !filepath.IsAbs(entry) {
			continue
		}
		canonical, err := filepath.EvalSymlinks(entry)
		if err != nil || !filepath.IsAbs(canonical) {
			continue
		}
		if canonicalRoot != "" && reviewFleetPathWithin(canonicalRoot, canonical) {
			continue
		}
		dirs = append(dirs, canonical)
	}
	return strings.Join(dirs, string(os.PathListSeparator)), nil
}

func reviewFleetPathWithin(root, candidate string) bool {
	rel, err := filepath.Rel(root, candidate)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

type reviewFleetContract struct {
	Version               int                          `json:"version"`
	CodexExecutable       string                       `json:"codex_executable"`
	CodexExecutableDigest string                       `json:"codex_executable_digest"`
	Reviewers             []reviewFleetContractProfile `json:"reviewers"`
	Consolidator          reviewFleetContractProfile   `json:"consolidator"`
	Certifier             reviewFleetContractProfile   `json:"certifier"`
	TrustedGuidance       []config.PathInstruction     `json:"trusted_guidance"`
}

type reviewFleetContractProfile struct {
	Role               string   `json:"role"`
	Model              string   `json:"model"`
	Reasoning          string   `json:"reasoning"`
	HighRiskPaths      []string `json:"high_risk_paths,omitempty"`
	EscalatedReasoning string   `json:"escalated_reasoning,omitempty"`
	Args               []string `json:"args"`
	EscalatedArgs      []string `json:"escalated_args,omitempty"`
}

// reviewFleetFingerprint hashes the complete effective fleet contract that a
// resumed run is allowed to use. The digest covers every profile, high-risk
// path, generated safe argument, and resolved Codex executable. Recovery
// requires exact equality instead of accepting a merely enabled fleet.
func reviewFleetFingerprint(settings *ReviewFleetSettings) (string, error) {
	return reviewFleetFingerprintWithGuidance(settings, nil)
}

func reviewFleetFingerprintWithGuidance(settings *ReviewFleetSettings, guidance []config.PathInstruction) (string, error) {
	if settings == nil || !settings.Enabled {
		return "", fmt.Errorf("cannot fingerprint a disabled review fleet")
	}
	if !filepath.IsAbs(settings.CodexExecutable) {
		return "", fmt.Errorf("review fleet Codex executable is not resolved")
	}
	digest, err := reviewFleetExecutableDigest(settings.CodexExecutable)
	if err != nil {
		return "", err
	}
	if settings.CodexExecutableDigest == "" || settings.CodexExecutableDigest != digest {
		return "", fmt.Errorf("review fleet Codex executable changed since configuration")
	}
	contract := reviewFleetContract{
		Version:               reviewFleetContractVersion,
		CodexExecutable:       settings.CodexExecutable,
		CodexExecutableDigest: digest,
		Reviewers:             make([]reviewFleetContractProfile, 0, len(settings.Reviewers)),
		TrustedGuidance:       normalizeFleetGuidance(guidance),
	}
	for _, profile := range settings.Reviewers {
		fingerprinted, err := reviewFleetFingerprintProfile(settings, profile)
		if err != nil {
			return "", err
		}
		contract.Reviewers = append(contract.Reviewers, fingerprinted)
	}
	consolidator, err := reviewFleetFingerprintProfile(settings, settings.Consolidator)
	if err != nil {
		return "", err
	}
	contract.Consolidator = consolidator
	certifier, err := reviewFleetFingerprintProfile(settings, settings.Certifier)
	if err != nil {
		return "", err
	}
	contract.Certifier = certifier
	encoded, err := json.Marshal(contract)
	if err != nil {
		return "", fmt.Errorf("encode review fleet contract: %w", err)
	}
	fingerprint := sha256.Sum256(encoded)
	return hex.EncodeToString(fingerprint[:]), nil
}

func reviewFleetExecutableDigest(executable string) (string, error) {
	file, err := os.Open(executable)
	if err != nil {
		return "", fmt.Errorf("open review fleet Codex executable: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", fmt.Errorf("stat review fleet Codex executable: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("review fleet Codex executable is not a regular file")
	}
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return "", fmt.Errorf("hash review fleet Codex executable: %w", err)
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func normalizeFleetGuidance(guidance []config.PathInstruction) []config.PathInstruction {
	result := make([]config.PathInstruction, 0, len(guidance))
	for _, rule := range guidance {
		rule.Path = strings.TrimSpace(rule.Path)
		rule.Instructions = strings.TrimSpace(rule.Instructions)
		result = append(result, rule)
	}
	return result
}

func reviewFleetFingerprintProfile(settings *ReviewFleetSettings, profile ReviewProfile) (reviewFleetContractProfile, error) {
	if settings.CodexProfileArgs == nil {
		return reviewFleetContractProfile{}, fmt.Errorf("review fleet Codex profile args are not configured")
	}
	args, err := settings.CodexProfileArgs(profile)
	if err != nil {
		return reviewFleetContractProfile{}, fmt.Errorf("build review fleet fingerprint args for %q: %w", profile.Role, err)
	}
	args, err = validateReviewFleetIsolation(args)
	if err != nil {
		return reviewFleetContractProfile{}, fmt.Errorf("validate review fleet fingerprint args for %q: %w", profile.Role, err)
	}
	result := reviewFleetContractProfile{
		Role:               profile.Role,
		Model:              profile.Model,
		Reasoning:          profile.Reasoning,
		HighRiskPaths:      append([]string(nil), profile.HighRiskPaths...),
		EscalatedReasoning: profile.EscalatedReasoning,
		Args:               args,
	}
	if len(profile.HighRiskPaths) > 0 || strings.TrimSpace(profile.EscalatedReasoning) != "" {
		escalated := profile
		escalated.SecurityEscalated = true
		result.EscalatedArgs, err = settings.CodexProfileArgs(escalated)
		if err != nil {
			return reviewFleetContractProfile{}, fmt.Errorf("build escalated review fleet fingerprint args for %q: %w", profile.Role, err)
		}
		result.EscalatedArgs, err = validateReviewFleetIsolation(result.EscalatedArgs)
		if err != nil {
			return reviewFleetContractProfile{}, fmt.Errorf("validate escalated review fleet fingerprint args for %q: %w", profile.Role, err)
		}
	}
	return result, nil
}

func projectReviewFleetProfile(role string, profile config.ReviewFleetProfile) ReviewProfile {
	return ReviewProfile{
		Role:               role,
		Model:              profile.Model,
		Reasoning:          profile.ReasoningEffort,
		HighRiskPaths:      append([]string(nil), profile.HighRiskPaths...),
		EscalatedReasoning: profile.EscalatedReasoningEffort,
	}
}

func validateReviewFleetArgs(args []string) ([]string, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("safe Codex profile args are empty")
	}
	total := 0
	for _, arg := range args {
		if strings.ContainsAny(arg, "\x00\r\n") {
			return nil, fmt.Errorf("safe Codex profile args contain control characters")
		}
		total += len(arg)
	}
	if total > reviewFleetMaxArgBytes {
		return nil, fmt.Errorf("safe Codex profile args exceed %d bytes", reviewFleetMaxArgBytes)
	}
	return append([]string(nil), args...), nil
}

func validateReviewFleetIsolation(args []string) ([]string, error) {
	validated, err := validateReviewFleetArgs(args)
	if err != nil {
		return nil, err
	}
	var readOnly, ephemeral, ignoredRules, ignoredUserConfig, suppressedProjectDoc, restrictedShellEnv bool
	for i := 0; i < len(validated); i++ {
		arg := validated[i]
		switch {
		case arg == "--dangerously-bypass-approvals-and-sandbox", arg == "--full-auto", arg == "--approve-for-me":
			return nil, fmt.Errorf("review fleet Codex args contain unsafe execution flag %q", arg)
		case arg == "--sandbox" || arg == "-s":
			if i+1 >= len(validated) || validated[i+1] != reviewFleetReadOnlySandbox {
				return nil, fmt.Errorf("review fleet Codex sandbox must be read-only")
			}
			readOnly = true
			i++
		case strings.HasPrefix(arg, "--sandbox="):
			if strings.TrimPrefix(arg, "--sandbox=") != reviewFleetReadOnlySandbox {
				return nil, fmt.Errorf("review fleet Codex sandbox must be read-only")
			}
			readOnly = true
		case arg == "--ephemeral":
			ephemeral = true
		case arg == "--ignore-rules":
			ignoredRules = true
		case arg == "--ignore-user-config":
			ignoredUserConfig = true
		case arg == "-c" || arg == "--config":
			if i+1 >= len(validated) {
				return nil, fmt.Errorf("review fleet Codex config flag is incomplete")
			}
			if strings.TrimSpace(validated[i+1]) == "project_doc_max_bytes=0" {
				suppressedProjectDoc = true
			}
			if strings.TrimSpace(validated[i+1]) == `shell_environment_policy.inherit="core"` {
				restrictedShellEnv = true
			}
			i++
		case strings.HasPrefix(arg, "-c=") || strings.HasPrefix(arg, "--config="):
			value := arg[strings.IndexByte(arg, '=')+1:]
			if strings.TrimSpace(value) == "project_doc_max_bytes=0" {
				suppressedProjectDoc = true
			}
			if strings.TrimSpace(value) == `shell_environment_policy.inherit="core"` {
				restrictedShellEnv = true
			}
		}
	}
	if !readOnly || !ephemeral || !ignoredRules || !ignoredUserConfig || !suppressedProjectDoc || !restrictedShellEnv {
		return nil, fmt.Errorf("review fleet Codex args are missing mandatory read-only isolation controls")
	}
	return validated, nil
}

type reviewProfileRunner struct {
	cfg             *config.Config
	settings        *ReviewFleetSettings
	db              *db.DB
	runID           string
	stepName        types.StepName
	round           func() int
	workDir         string
	evidenceRoot    string
	onLifecycle     func(agent.LifecycleEvent)
	sourceCodexHome string
	baseSHA         string
	defaultBranch   string

	mu          sync.Mutex
	sandboxRoot string
	checkoutDir string
	homeDir     string
	codexHome   string
	sandboxHead string
	closed      bool
}

func (e *Executor) newReviewProfileRunner(run *db.Run, repo *db.Repo, stepName types.StepName, round func() int, onLifecycle func(agent.LifecycleEvent)) *reviewProfileRunner {
	if e == nil || e.reviewFleet == nil || !e.reviewFleet.Enabled || e.config == nil || run == nil || repo == nil {
		return nil
	}
	runner := &reviewProfileRunner{
		cfg:             e.config,
		settings:        e.reviewFleet,
		db:              e.db,
		runID:           run.ID,
		stepName:        stepName,
		round:           round,
		workDir:         e.workDir,
		evidenceRoot:    e.runEvidenceDir(run.ID),
		onLifecycle:     onLifecycle,
		sourceCodexHome: reviewFleetSourceCodexHome(),
		baseSHA:         run.BaseSHA,
		defaultBranch:   repo.DefaultBranch,
	}
	return runner
}

func (r *reviewProfileRunner) Run(ctx context.Context, profile ReviewProfile, opts agent.RunOpts) (*agent.Result, error) {
	if r == nil || r.cfg == nil || r.settings == nil {
		return nil, fmt.Errorf("review fleet runner is not configured")
	}
	invocation := *r
	invocation.mu = sync.Mutex{}
	invocation.sandboxRoot = ""
	invocation.checkoutDir = ""
	invocation.homeDir = ""
	invocation.codexHome = ""
	invocation.sandboxHead = ""
	invocation.closed = false
	defer invocation.Close()
	return invocation.run(ctx, profile, opts)
}

func (r *reviewProfileRunner) run(ctx context.Context, profile ReviewProfile, opts agent.RunOpts) (*agent.Result, error) {
	if strings.TrimSpace(opts.CWD) != "" && opts.CWD != r.workDir {
		return nil, fmt.Errorf("review fleet runner refuses a worktree outside the shared read-only checkout")
	}
	checkoutDir, isolatedEnv, err := r.ensureSandbox(ctx, opts.TargetSHA)
	if err != nil {
		return nil, err
	}
	// Fleet invocations are always cold, even if a caller accidentally passes
	// session metadata copied from the ordinary review loop.
	opts.Session = nil
	opts.SessionFallback = false
	opts.SessionFallbackReason = ""
	argsFn := r.settings.CodexProfileArgs
	if argsFn == nil {
		return nil, fmt.Errorf("review fleet Codex profile args are not configured")
	}
	args, err := argsFn(profile)
	if err != nil {
		return nil, fmt.Errorf("build review fleet Codex profile %q: %w", profile.Role, err)
	}
	args, err = validateReviewFleetIsolation(args)
	if err != nil {
		return nil, err
	}
	if !filepath.IsAbs(r.settings.CodexExecutable) {
		return nil, fmt.Errorf("review fleet Codex executable is not resolved")
	}
	digest, err := reviewFleetExecutableDigest(r.settings.CodexExecutable)
	if err != nil {
		return nil, err
	}
	if r.settings.CodexExecutableDigest == "" || digest != r.settings.CodexExecutableDigest {
		return nil, fmt.Errorf("review fleet Codex executable changed since configuration")
	}
	base, err := agent.NewWithOptions(types.AgentCodex, r.settings.CodexExecutable, args, agent.Options{
		ACPRegistryOverrides:   r.cfg.ACPRegistryOverrides,
		DisableProjectSettings: true,
	})
	if err != nil {
		return nil, fmt.Errorf("create review fleet Codex profile %q: %w", profile.Role, err)
	}
	defer base.Close()
	if err := agent.EnsureGateNeutralized(base); err != nil {
		return nil, fmt.Errorf("neutralize review fleet Codex profile %q: %w", profile.Role, err)
	}
	wrapped := agent.WithSteering(base, r.evidenceRoot)
	wrapped = &gateStepBoundaryAgent{inner: wrapped, phase: r.stepName}
	wrapped = &lifecycleAgent{inner: wrapped, onLifecycle: func(event agent.LifecycleEvent) {
		event.Agent = profile.Role + "/" + event.Agent
		if event.Message != "" {
			event.Message = safeReviewFleetRuntimeText(profile.Role+": "+event.Message, reviewFleetMaxRuntimeLogBytes)
		}
		if r.onLifecycle != nil {
			r.onLifecycle(event)
		}
	}}
	wrapped = &perfRecordingAgent{inner: wrapped, db: r.db, runID: r.runID, stepName: r.stepName, round: r.round}
	opts.CWD = checkoutDir
	opts.Env = append(reviewFleetNonGitEnv(opts.Env), isolatedEnv...)
	result, err := wrapped.Run(ctx, opts)
	if err != nil {
		return result, fmt.Errorf("review fleet profile %q failed: %s", profile.Role, safeReviewFleetRuntimeText(err.Error(), reviewFleetMaxRuntimeLogBytes))
	}
	return result, nil
}

func safeReviewFleetRuntimeText(value string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	value = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, value)
	value = strings.Join(strings.Fields(value), " ")
	value = intent.StripAdversarial(value)
	value = intent.RedactSecrets(value)
	value = safeurl.RedactText(value)
	lower := strings.ToLower(value)
	for _, directive := range []string{
		"ignore previous instructions",
		"ignore all previous instructions",
		"ignore the instructions above",
		"you are now the system",
		"developer message:",
	} {
		for index := strings.Index(lower, directive); index >= 0; index = strings.Index(lower, directive) {
			value = value[:index] + "[runtime directive removed]" + value[index+len(directive):]
			lower = strings.ToLower(value)
		}
	}
	if len(value) <= maxBytes {
		return value
	}
	const marker = " …[truncated]"
	if maxBytes <= len(marker) {
		return marker[:maxBytes]
	}
	value = value[:maxBytes-len(marker)]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value + marker
}

// ensureSandbox returns a clean, immutable source export for the exact source
// HEAD being reviewed. The export deliberately excludes repository skills
// and .codex state; HOME, CODEX_HOME, CODEX_SQLITE_HOME, and XDG state are
// isolated, with only a bounded auth.json copy. A fix round that advances HEAD
// gets a fresh shadow automatically.
func (r *reviewProfileRunner) ensureSandbox(ctx context.Context, expectedHeads ...string) (string, []string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return "", nil, fmt.Errorf("review fleet runner is closed")
	}
	head, err := r.reviewFleetGitRun(ctx, r.workDir, "rev-parse", "HEAD")
	if err != nil {
		return "", nil, fmt.Errorf("resolve review fleet source head: %w", err)
	}
	if len(expectedHeads) > 0 && strings.TrimSpace(expectedHeads[0]) != "" && head != expectedHeads[0] {
		return "", nil, fmt.Errorf("review fleet source head changed from review target %s to %s", expectedHeads[0], head)
	}
	status, err := r.reviewFleetGitRun(ctx, r.workDir, "status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return "", nil, fmt.Errorf("check review fleet source worktree: %w", err)
	}
	if strings.TrimSpace(status) != "" {
		return "", nil, fmt.Errorf("review fleet requires a clean committed source worktree")
	}
	if r.checkoutDir != "" && r.sandboxHead == head {
		return r.checkoutDir, r.isolatedEnv(), nil
	}
	if err := r.removeSandboxLocked(); err != nil {
		return "", nil, err
	}
	root, err := os.MkdirTemp("", "no-mistakes-review-fleet-")
	if err != nil {
		return "", nil, fmt.Errorf("create review fleet isolation root: %w", err)
	}
	r.sandboxRoot = root
	r.checkoutDir = filepath.Join(root, "checkout")
	r.homeDir = filepath.Join(root, "home")
	r.codexHome = filepath.Join(root, "codex")
	for _, dir := range []string{
		r.homeDir,
		r.codexHome,
		filepath.Join(root, "codex-sqlite"),
		filepath.Join(root, "xdg-config"),
		filepath.Join(root, "xdg-data"),
		filepath.Join(root, "xdg-state"),
		filepath.Join(root, "xdg-cache"),
	} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			_ = r.removeSandboxLocked()
			return "", nil, fmt.Errorf("create review fleet isolation directory: %w", err)
		}
	}
	if err := copyReviewFleetAuth(r.sourceCodexHome, r.codexHome); err != nil {
		_ = r.removeSandboxLocked()
		return "", nil, err
	}
	if err := r.exportSandbox(ctx, head); err != nil {
		_ = r.removeSandboxLocked()
		return "", nil, err
	}
	if err := r.verifySandbox(ctx, head); err != nil {
		_ = r.removeSandboxLocked()
		return "", nil, err
	}
	r.sandboxHead = head
	return r.checkoutDir, r.isolatedEnv(), nil
}

func (r *reviewProfileRunner) exportSandbox(ctx context.Context, head string) error {
	if err := os.MkdirAll(r.checkoutDir, 0o700); err != nil {
		return fmt.Errorf("create review fleet source export: %w", err)
	}
	var total int64
	entries := 0
	metadataBytes := 0
	err := git.ForEachNULRecordWithBaseEnv(ctx, r.workDir, reviewFleetBaseEnv(r.workDir), reviewFleetGitEnv(), []string{"ls-tree", "-r", "-z", "--full-tree", head}, func(entry []byte) error {
		entries++
		metadataBytes += len(entry)
		if entries > reviewFleetMaxExportEntries || metadataBytes > reviewFleetMaxTreeMetadataBytes {
			return fmt.Errorf("review fleet source export exceeds bounded entry policy")
		}
		metadata, path, ok := bytes.Cut(entry, []byte{'\t'})
		if !ok {
			return fmt.Errorf("read malformed review fleet source-tree entry")
		}
		fields := bytes.Fields(metadata)
		if len(fields) != 3 || string(fields[1]) != "blob" || string(fields[0]) == "120000" {
			return fmt.Errorf("review fleet source export contains unsupported entry %q", path)
		}
		relative, pathErr := reviewFleetExportPath(string(path))
		if pathErr != nil {
			return pathErr
		}
		if reviewFleetExcludedExportPath(relative) {
			return nil
		}
		sizeText, err := git.RunWithBaseEnv(ctx, r.workDir, reviewFleetBaseEnv(r.workDir), reviewFleetGitEnv(), "cat-file", "-s", string(fields[2]))
		if err != nil {
			return fmt.Errorf("size review fleet source blob %q: %w", relative, err)
		}
		var size int64
		if _, err := fmt.Sscan(strings.TrimSpace(sizeText), &size); err != nil || size < 0 {
			return fmt.Errorf("read review fleet source blob size %q", relative)
		}
		if size > reviewFleetMaxExportFileBytes || total+size > reviewFleetMaxExportTotalBytes {
			return fmt.Errorf("review fleet source export exceeds bounded size policy at %q", relative)
		}
		target := filepath.Join(r.checkoutDir, relative)
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return fmt.Errorf("create review fleet export parent: %w", err)
		}
		file, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return fmt.Errorf("create review fleet export file: %w", err)
		}
		copyErr := git.CopyBlobWithBaseEnv(ctx, r.workDir, reviewFleetBaseEnv(r.workDir), reviewFleetGitEnv(), string(fields[2]), file)
		closeErr := file.Close()
		if copyErr != nil {
			return fmt.Errorf("read review fleet source blob %q: %w", relative, copyErr)
		}
		if closeErr != nil {
			return fmt.Errorf("close review fleet export file: %w", closeErr)
		}
		total += size
		return nil
	})
	if err != nil {
		return fmt.Errorf("list review fleet source tree: %w", err)
	}
	if err := r.writeReviewArtifacts(ctx, head); err != nil {
		return err
	}
	return sealReviewFleetExport(r.checkoutDir)
}

func (r *reviewProfileRunner) writeReviewArtifacts(ctx context.Context, head string) error {
	base := r.resolveArtifactBase(ctx, head)
	args := []string{"diff", "--no-ext-diff", "--binary", base + ".." + head, "--", ".", ":(exclude).agents/skills/**", ":(exclude).codex/**"}
	diff, err := git.RunRawWithBaseEnv(ctx, r.workDir, reviewFleetBaseEnv(r.workDir), reviewFleetGitEnv(), args...)
	if err != nil {
		return fmt.Errorf("create review fleet diff artifact: %w", err)
	}
	if len(diff) > reviewFleetMaxReviewDataBytes {
		return fmt.Errorf("review fleet diff artifact exceeds %d bytes", reviewFleetMaxReviewDataBytes)
	}
	history, err := r.reviewFleetGitRun(ctx, r.workDir, "log", "--format=%H%x09%P%x09%s", "-n", "32", base+".."+head)
	if err != nil {
		return fmt.Errorf("create review fleet history artifact: %w", err)
	}
	if len(history) > reviewFleetMaxReviewDataBytes {
		return fmt.Errorf("review fleet history artifact exceeds %d bytes", reviewFleetMaxReviewDataBytes)
	}
	artifactDir := filepath.Join(r.checkoutDir, ".review-fleet")
	if err := os.MkdirAll(artifactDir, 0o700); err != nil {
		return fmt.Errorf("create review fleet artifact directory: %w", err)
	}
	if err := os.WriteFile(filepath.Join(artifactDir, "base-to-target.diff"), diff, 0o600); err != nil {
		return fmt.Errorf("write review fleet diff artifact: %w", err)
	}
	if err := os.WriteFile(filepath.Join(artifactDir, "history.txt"), []byte(history+"\n"), 0o600); err != nil {
		return fmt.Errorf("write review fleet history artifact: %w", err)
	}
	return nil
}

func (r *reviewProfileRunner) resolveArtifactBase(ctx context.Context, head string) string {
	for _, ref := range []string{"origin/" + strings.TrimSpace(r.defaultBranch), strings.TrimSpace(r.defaultBranch)} {
		if strings.TrimSpace(ref) == "" || ref == "origin/" {
			continue
		}
		base, err := r.reviewFleetGitRun(ctx, r.workDir, "merge-base", head, ref)
		if err == nil && strings.TrimSpace(base) != "" {
			return strings.TrimSpace(base)
		}
	}
	base := strings.TrimSpace(r.baseSHA)
	if base != "" && !git.IsZeroSHA(base) {
		return base
	}
	return git.EmptyTreeSHA
}

func sealReviewFleetExport(root string) error {
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		mode := os.FileMode(0o444)
		if info.IsDir() {
			mode = 0o555
		}
		if err := os.Chmod(path, mode); err != nil {
			return fmt.Errorf("seal review fleet source export: %w", err)
		}
		return nil
	})
}

func reviewFleetExportPath(name string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(name))
	if clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("review fleet source export contains forbidden path %q", name)
	}
	return clean, nil
}

func reviewFleetExcludedExportPath(path string) bool {
	for _, excluded := range []string{filepath.Join(".agents", "skills"), ".codex", ".git"} {
		if path == excluded || strings.HasPrefix(path, excluded+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func reviewFleetGitEnv() []string {
	return []string{
		"NO_MISTAKES_FLEET_GIT_ENV=1",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=" + os.DevNull,
		"GIT_ATTR_NOSYSTEM=1",
		"GIT_TEMPLATE_DIR=",
		"GIT_CONFIG_PARAMETERS=",
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=core.hooksPath",
		"GIT_CONFIG_VALUE_0=" + os.DevNull,
	}
}

func (r *reviewProfileRunner) reviewFleetGitRun(ctx context.Context, dir string, args ...string) (string, error) {
	return git.RunWithBaseEnv(ctx, dir, reviewFleetBaseEnv(r.workDir), reviewFleetGitEnv(), args...)
}

func reviewFleetBaseEnv(workDir string) []string {
	env := reviewFleetNonGitEnv(os.Environ())
	filtered := make([]string, 0, len(env))
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || !strings.EqualFold(key, "PATH") {
			filtered = append(filtered, entry)
			continue
		}
		paths, err := reviewFleetCanonicalPATH(workDir, value)
		if err != nil {
			paths = ""
		}
		filtered = append(filtered, "PATH="+paths)
	}
	return filtered
}

func (r *reviewProfileRunner) verifySandbox(ctx context.Context, expectedHead string) error {
	if _, err := os.Lstat(filepath.Join(r.checkoutDir, ".git")); !os.IsNotExist(err) {
		return fmt.Errorf("review fleet source export retained Git metadata")
	}
	for _, relative := range []string{filepath.Join(".agents", "skills"), ".codex"} {
		if _, err := os.Lstat(filepath.Join(r.checkoutDir, relative)); !os.IsNotExist(err) {
			return fmt.Errorf("review fleet source export exposes excluded prompt-control path %s", relative)
		}
	}
	sourceHead, err := r.reviewFleetGitRun(ctx, r.workDir, "rev-parse", "HEAD")
	if err != nil {
		return fmt.Errorf("verify review fleet source head after preparing shadow: %w", err)
	}
	if sourceHead != expectedHead {
		return fmt.Errorf("review fleet source head changed while preparing shadow: got %q, want %q", sourceHead, expectedHead)
	}
	sourceStatus, err := r.reviewFleetGitRun(ctx, r.workDir, "status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return fmt.Errorf("verify review fleet source cleanliness after preparing shadow: %w", err)
	}
	if strings.TrimSpace(sourceStatus) != "" {
		return fmt.Errorf("review fleet source worktree changed while preparing shadow")
	}
	return nil
}

func (r *reviewProfileRunner) isolatedEnv() []string {
	root := r.sandboxRoot
	env := []string{
		"HOME=" + r.homeDir,
		"CODEX_HOME=" + r.codexHome,
		"CODEX_SQLITE_HOME=" + filepath.Join(root, "codex-sqlite"),
		"XDG_CONFIG_HOME=" + filepath.Join(root, "xdg-config"),
		"XDG_DATA_HOME=" + filepath.Join(root, "xdg-data"),
		"XDG_STATE_HOME=" + filepath.Join(root, "xdg-state"),
		"XDG_CACHE_HOME=" + filepath.Join(root, "xdg-cache"),
		"PWD=" + r.checkoutDir,
	}
	for _, entry := range reviewFleetBaseEnv(r.workDir) {
		key, _, ok := strings.Cut(entry, "=")
		if ok && strings.EqualFold(key, "PATH") {
			env = append(env, entry)
			break
		}
	}
	return append(env, reviewFleetGitEnv()...)
}

func reviewFleetNonGitEnv(env []string) []string {
	filtered := make([]string, 0, len(env))
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(strings.ToUpper(key), "GIT_") {
			continue
		}
		filtered = append(filtered, entry)
	}
	return filtered
}

func (r *reviewProfileRunner) Close() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	_ = r.removeSandboxLocked()
}

func (r *reviewProfileRunner) removeSandboxLocked() error {
	root := r.sandboxRoot
	r.sandboxRoot = ""
	r.checkoutDir = ""
	r.homeDir = ""
	r.codexHome = ""
	r.sandboxHead = ""
	if root == "" {
		return nil
	}
	if err := os.RemoveAll(root); err != nil {
		return fmt.Errorf("remove review fleet isolation root: %w", err)
	}
	return nil
}

func reviewFleetSourceCodexHome() string {
	if configured := strings.TrimSpace(os.Getenv("CODEX_HOME")); configured != "" {
		return configured
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".codex")
}

func copyReviewFleetAuth(sourceCodexHome, targetCodexHome string) error {
	if strings.TrimSpace(sourceCodexHome) == "" {
		return nil
	}
	source := filepath.Join(sourceCodexHome, "auth.json")
	info, err := os.Lstat(source)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect Codex auth for review fleet: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("Codex auth for review fleet must be a regular file")
	}
	if info.Size() > reviewFleetMaxAuthBytes {
		return fmt.Errorf("Codex auth for review fleet exceeds %d bytes", reviewFleetMaxAuthBytes)
	}
	contents, err := os.ReadFile(source)
	if err != nil {
		return fmt.Errorf("read Codex auth for review fleet: %w", err)
	}
	if err := os.WriteFile(filepath.Join(targetCodexHome, "auth.json"), contents, 0o600); err != nil {
		return fmt.Errorf("copy Codex auth for review fleet: %w", err)
	}
	return nil
}
