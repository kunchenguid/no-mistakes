package pipeline

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
	reviewFleetReadOnlySandbox    = "read-only"
	reviewFleetMaxArgBytes        = 4096
	reviewFleetMaxAuthBytes       = 4 * 1024 * 1024
	reviewFleetMaxRuntimeLogBytes = 2048
)

const reviewFleetContractVersion = 1

// reviewFleetSettingsFromConfig projects the trusted global-only config into
// the execution types used by the pipeline. The fixed order is deliberate:
// reviewer completion is concurrent, but configuration and test evidence stay
// deterministic.
func reviewFleetSettingsFromConfig(cfg *config.Config) (*ReviewFleetSettings, error) {
	if cfg == nil {
		return nil, nil
	}
	settings := &ReviewFleetSettings{Enabled: cfg.ReviewFleet.Enabled}
	if !settings.Enabled {
		return settings, nil
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

type reviewFleetContract struct {
	Version         int                          `json:"version"`
	CodexExecutable string                       `json:"codex_executable"`
	Reviewers       []reviewFleetContractProfile `json:"reviewers"`
	Consolidator    reviewFleetContractProfile   `json:"consolidator"`
	Certifier       reviewFleetContractProfile   `json:"certifier"`
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
func reviewFleetFingerprint(cfg *config.Config, settings *ReviewFleetSettings) (string, error) {
	if cfg == nil || settings == nil || !settings.Enabled {
		return "", fmt.Errorf("cannot fingerprint a disabled review fleet")
	}
	executable, err := exec.LookPath(cfg.AgentPathFor(types.AgentCodex))
	if err != nil {
		return "", fmt.Errorf("resolve review fleet Codex executable: %w", err)
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		return "", fmt.Errorf("resolve absolute review fleet Codex executable: %w", err)
	}
	if resolved, resolveErr := filepath.EvalSymlinks(executable); resolveErr == nil {
		executable = resolved
	}
	contract := reviewFleetContract{
		Version:         reviewFleetContractVersion,
		CodexExecutable: executable,
		Reviewers:       make([]reviewFleetContractProfile, 0, len(settings.Reviewers)),
	}
	for _, profile := range settings.Reviewers {
		fingerprinted, err := reviewFleetFingerprintProfile(settings, profile)
		if err != nil {
			return "", err
		}
		contract.Reviewers = append(contract.Reviewers, fingerprinted)
	}
	contract.Consolidator, err = reviewFleetFingerprintProfile(settings, settings.Consolidator)
	if err != nil {
		return "", err
	}
	contract.Certifier, err = reviewFleetFingerprintProfile(settings, settings.Certifier)
	if err != nil {
		return "", err
	}
	encoded, err := json.Marshal(contract)
	if err != nil {
		return "", fmt.Errorf("encode review fleet contract: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
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

	mu          sync.Mutex
	sandboxRoot string
	checkoutDir string
	homeDir     string
	codexHome   string
	sandboxHead string
	closed      bool
}

func (e *Executor) newReviewProfileRunner(run *db.Run, stepName types.StepName, round func() int, onLifecycle func(agent.LifecycleEvent)) *reviewProfileRunner {
	if e == nil || e.reviewFleet == nil || !e.reviewFleet.Enabled || e.config == nil || run == nil {
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
	}
	return runner
}

func (r *reviewProfileRunner) Run(ctx context.Context, profile ReviewProfile, opts agent.RunOpts) (*agent.Result, error) {
	if r == nil || r.cfg == nil || r.settings == nil {
		return nil, fmt.Errorf("review fleet runner is not configured")
	}
	if strings.TrimSpace(opts.CWD) != "" && opts.CWD != r.workDir {
		return nil, fmt.Errorf("review fleet runner refuses a worktree outside the shared read-only checkout")
	}
	checkoutDir, isolatedEnv, err := r.ensureSandbox(ctx)
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
	base, err := agent.NewWithOptions(types.AgentCodex, r.cfg.AgentPathFor(types.AgentCodex), args, agent.Options{
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
	opts.Env = append(opts.Env, isolatedEnv...)
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

// ensureSandbox returns a clean, detached shadow checkout for the exact source
// HEAD being reviewed. The checkout deliberately excludes repository skills
// and .codex state; HOME and CODEX_HOME are empty except for a bounded copy of
// auth.json. A fix round that advances HEAD gets a fresh shadow automatically.
func (r *reviewProfileRunner) ensureSandbox(ctx context.Context) (string, []string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return "", nil, fmt.Errorf("review fleet runner is closed")
	}
	head, err := git.HeadSHA(ctx, r.workDir)
	if err != nil {
		return "", nil, fmt.Errorf("resolve review fleet source head: %w", err)
	}
	status, err := git.Run(ctx, r.workDir, "status", "--porcelain", "--untracked-files=all")
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
	if _, err := git.Run(ctx, root, "clone", "--no-local", "--no-checkout", "--", r.workDir, r.checkoutDir); err != nil {
		_ = r.removeSandboxLocked()
		return "", nil, fmt.Errorf("clone review fleet shadow checkout: %w", err)
	}
	if _, err := git.Run(ctx, r.checkoutDir, "sparse-checkout", "set", "--no-cone", "/*", "!/.agents/skills/", "!/.codex/"); err != nil {
		_ = r.removeSandboxLocked()
		return "", nil, fmt.Errorf("exclude checkout prompt-control directories: %w", err)
	}
	if _, err := git.Run(ctx, r.checkoutDir, "checkout", "--detach", head); err != nil {
		_ = r.removeSandboxLocked()
		return "", nil, fmt.Errorf("checkout review fleet source head: %w", err)
	}
	if _, err := git.Run(ctx, r.checkoutDir, "remote", "remove", "origin"); err != nil {
		_ = r.removeSandboxLocked()
		return "", nil, fmt.Errorf("detach review fleet shadow from source: %w", err)
	}
	if err := r.verifySandbox(ctx, head); err != nil {
		_ = r.removeSandboxLocked()
		return "", nil, err
	}
	r.sandboxHead = head
	return r.checkoutDir, r.isolatedEnv(), nil
}

func (r *reviewProfileRunner) verifySandbox(ctx context.Context, expectedHead string) error {
	head, err := git.HeadSHA(ctx, r.checkoutDir)
	if err != nil {
		return fmt.Errorf("verify review fleet shadow head: %w", err)
	}
	if head != expectedHead {
		return fmt.Errorf("verify review fleet shadow head: got %q, want %q", head, expectedHead)
	}
	status, err := git.Run(ctx, r.checkoutDir, "status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return fmt.Errorf("verify review fleet shadow cleanliness: %w", err)
	}
	if strings.TrimSpace(status) != "" {
		return fmt.Errorf("verify review fleet shadow cleanliness: status %q", status)
	}
	for _, relative := range []string{filepath.Join(".agents", "skills"), ".codex"} {
		if _, err := os.Lstat(filepath.Join(r.checkoutDir, relative)); !os.IsNotExist(err) {
			return fmt.Errorf("review fleet shadow exposes excluded prompt-control path %s", relative)
		}
	}
	origin, err := git.Run(ctx, r.checkoutDir, "remote")
	if err != nil {
		return fmt.Errorf("inspect review fleet shadow remotes: %w", err)
	}
	if strings.TrimSpace(origin) != "" {
		return fmt.Errorf("review fleet shadow retained source remote %q", origin)
	}
	if _, err := os.Lstat(filepath.Join(r.checkoutDir, ".git", "objects", "info", "alternates")); !os.IsNotExist(err) {
		return fmt.Errorf("review fleet shadow retained an object-store alternate")
	}
	sourceHead, err := git.HeadSHA(ctx, r.workDir)
	if err != nil {
		return fmt.Errorf("verify review fleet source head after preparing shadow: %w", err)
	}
	if sourceHead != expectedHead {
		return fmt.Errorf("review fleet source head changed while preparing shadow: got %q, want %q", sourceHead, expectedHead)
	}
	sourceStatus, err := git.Run(ctx, r.workDir, "status", "--porcelain", "--untracked-files=all")
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
	return []string{
		"HOME=" + r.homeDir,
		"CODEX_HOME=" + r.codexHome,
		"XDG_CONFIG_HOME=" + filepath.Join(root, "xdg-config"),
		"XDG_DATA_HOME=" + filepath.Join(root, "xdg-data"),
		"XDG_STATE_HOME=" + filepath.Join(root, "xdg-state"),
		"XDG_CACHE_HOME=" + filepath.Join(root, "xdg-cache"),
		"PWD=" + r.checkoutDir,
	}
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
