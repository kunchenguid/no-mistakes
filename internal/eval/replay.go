package eval

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/safeurl"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// ReplayOptions controls one isolated candidate comparison.
type ReplayOptions struct {
	Set       string
	Candidate Candidate
	Repeats   int
}

// Session records the immutable local plan used for one replay batch.
type Session struct {
	ID        string    `json:"id"`
	StartedAt time.Time `json:"started_at"`
	Set       string    `json:"set"`
	Candidate string    `json:"candidate"`
	Repeats   int       `json:"repeats"`
	CaseIDs   []string  `json:"case_ids"`
}

// Replay runs exactly the captured review pass. It does not start a daemon or
// use the production NM_HOME: every case is restored into a fresh temp gate and
// worktree. Push, PR, CI, and all fix loops are intentionally absent from the
// MVP subject under test.
func Replay(ctx context.Context, store *Store, opts ReplayOptions) (Session, []Evaluation, error) {
	if store == nil {
		return Session{}, nil, fmt.Errorf("eval replay requires a store")
	}
	if opts.Repeats <= 0 {
		return Session{}, nil, fmt.Errorf("repeats must be at least 1")
	}
	cases, err := store.ListCases(opts.Set)
	if err != nil {
		return Session{}, nil, err
	}
	if len(cases) == 0 {
		return Session{}, nil, fmt.Errorf("case set %q is empty", opts.Set)
	}
	session := Session{ID: newSessionID(), StartedAt: time.Now().UTC(), Set: opts.Set, Candidate: opts.Candidate.String(), Repeats: opts.Repeats}
	for _, c := range cases {
		session.CaseIDs = append(session.CaseIDs, c.ID)
	}
	sessionsDir := filepath.Join(store.root, "sessions")
	if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
		return Session{}, nil, fmt.Errorf("create eval sessions directory: %w", err)
	}
	if err := writeJSON(filepath.Join(sessionsDir, session.ID+".json"), session); err != nil {
		return Session{}, nil, fmt.Errorf("write eval session: %w", err)
	}

	evaluations := make([]Evaluation, 0, len(cases)*opts.Repeats)
	var failed int
	for repeat := 1; repeat <= opts.Repeats; repeat++ {
		for _, c := range cases {
			evaluation := replayOne(ctx, c, session, opts.Candidate, repeat)
			if evaluation.Status != "completed" {
				failed++
			}
			if err := store.persistEvaluation(c, evaluation); err != nil {
				return session, evaluations, err
			}
			evaluations = append(evaluations, evaluation)
		}
	}
	if failed > 0 {
		return session, evaluations, fmt.Errorf("%d replay invocation(s) failed; inspect eval report for the local failure count", failed)
	}
	return session, evaluations, nil
}

func replayOne(ctx context.Context, c Case, session Session, candidate Candidate, repeat int) Evaluation {
	started := time.Now()
	evaluation := Evaluation{
		ID:        newSessionID(),
		SessionID: session.ID,
		CaseID:    c.ID,
		Candidate: candidate.String(),
		Repeat:    repeat,
		StartedAt: started.Unix(),
		Status:    "failed",
	}
	if c.Labels.Verdict.Known {
		expected := c.Labels.Verdict.ShouldPark
		evaluation.ExpectedPark = &expected
	}

	root, err := os.MkdirTemp("", "nm-eval-replay-")
	if err != nil {
		evaluation.Error = safeurl.RedactText(fmt.Sprintf("create isolated replay root: %v", err))
		evaluation.CompletedAt = time.Now().Unix()
		return evaluation
	}
	defer os.RemoveAll(root)

	workDir, err := restoreCase(ctx, c, root)
	if err != nil {
		evaluation.Error = safeurl.RedactText(err.Error())
		evaluation.CompletedAt = time.Now().Unix()
		return evaluation
	}
	cfg, err := replayConfig(c)
	if err != nil {
		evaluation.Error = safeurl.RedactText(err.Error())
		evaluation.CompletedAt = time.Now().Unix()
		return evaluation
	}
	cfg.Agent = candidate.Agent
	cfg.Agents = []types.AgentName{candidate.Agent}

	baseAgent, err := agent.NewWithOptions(candidate.Agent, cfg.AgentPathFor(candidate.Agent), candidateModelArgs(candidate), agent.Options{
		ACPRegistryOverrides:   cfg.ACPRegistryOverrides,
		DisableProjectSettings: cfg.DisableProjectSettings,
	})
	if err != nil {
		evaluation.Error = safeurl.RedactText(fmt.Sprintf("create candidate agent: %v", err))
		evaluation.CompletedAt = time.Now().Unix()
		return evaluation
	}
	defer baseAgent.Close()
	observed := &observedAgent{inner: agent.WithSteering(baseAgent)}

	replayRun := &db.Run{ID: "eval-" + evaluation.ID, Branch: c.Branch, HeadSHA: c.ReviewedHeadSHA, BaseSHA: c.BaseSHA}
	if c.Intent != "" {
		intent := c.Intent
		replayRun.Intent = &intent
	}
	if c.IntentSource != "" {
		source := c.IntentSource
		replayRun.IntentSource = &source
	}
	defaultBranch := c.DefaultBranch
	if defaultBranch == "" {
		defaultBranch = "main"
	}
	replayRepo := &db.Repo{ID: "eval", WorkingPath: workDir, DefaultBranch: defaultBranch}
	step := &steps.ReviewStep{}
	outcome, err := step.Execute(&pipeline.StepContext{
		Ctx:          ctx,
		Run:          replayRun,
		Repo:         replayRepo,
		WorkDir:      workDir,
		Agent:        observed,
		Config:       cfg,
		Log:          func(string) {},
		LogChunk:     func(string) {},
		LogFile:      func(string) {},
		UserIntent:   c.Intent,
		IntentSource: c.IntentSource,
	})
	// Candidate wall time is the actual review invocation, matching the local
	// agent-invocation metric rather than charging bundle restoration setup.
	evaluation.DurationMS = observed.durationMS
	if evaluation.DurationMS == 0 && observed.result == nil {
		evaluation.DurationMS = time.Since(started).Milliseconds()
	}
	evaluation.CompletedAt = time.Now().Unix()
	if observed.result != nil {
		evaluation.Model = observed.result.Model
		if evaluation.Model == "" {
			evaluation.Model = candidate.Model
		}
		if observed.result.UsageReported {
			evaluation.TokensReported = true
			evaluation.InputTokens = int64(observed.result.Usage.InputTokens)
			evaluation.OutputTokens = int64(observed.result.Usage.OutputTokens)
			evaluation.CacheReadTokens = int64(observed.result.Usage.CacheReadTokens)
			evaluation.FreshInputTokens = int64(agent.FreshInputTokens(observed.result.Usage.InputTokens, observed.result.Usage.CacheReadTokens))
		}
	}
	if err != nil {
		evaluation.Error = safeurl.RedactText(fmt.Sprintf("replay review: %v", err))
		return evaluation
	}
	if outcome == nil {
		evaluation.Error = "replay review returned no outcome"
		return evaluation
	}
	evaluation.Status = "completed"
	evaluation.CandidateParked = outcome.NeedsApproval || hasAskUserFindings(outcome.Findings)
	evaluation.FindingsJSON = outcome.Findings
	evaluation.FindingCount = findingCount(outcome.Findings)
	return evaluation
}

func restoreCase(ctx context.Context, c Case, root string) (string, error) {
	// The temp Paths root is intentionally created even though this MVP invokes
	// ReviewStep directly. It documents and enforces the same isolated-root
	// boundary used by the e2e daemon harness, while never setting process-wide
	// NM_HOME or touching the shared daemon.
	p := paths.WithRoot(filepath.Join(root, "nmhome"))
	if err := p.EnsureDirs(); err != nil {
		return "", fmt.Errorf("create isolated eval state: %w", err)
	}
	gateDir := filepath.Join(root, "gate.git")
	if err := git.InitBare(ctx, gateDir); err != nil {
		return "", fmt.Errorf("create isolated eval gate: %w", err)
	}
	prefix := "refs/no-mistakes/eval/" + c.ID + "/*"
	bundle := filepath.Join(c.Dir, "branch.bundle")
	if _, err := git.Run(ctx, gateDir, "fetch", bundle, "+"+prefix+":"+prefix); err != nil {
		return "", fmt.Errorf("restore case bundle: %w", err)
	}
	defaultBranch := c.DefaultBranch
	if defaultBranch == "" {
		defaultBranch = "main"
	}
	if _, err := git.Run(ctx, gateDir, "update-ref", "refs/remotes/origin/"+defaultBranch, c.TrustedConfigSHA); err != nil {
		return "", fmt.Errorf("restore trusted default branch: %w", err)
	}
	workDir := filepath.Join(root, "worktree")
	if err := git.WorktreeAdd(ctx, gateDir, workDir, c.ReviewedHeadSHA); err != nil {
		return "", fmt.Errorf("restore review worktree: %w", err)
	}
	return workDir, nil
}

func replayConfig(c Case) (*config.Config, error) {
	global, err := config.LoadGlobal(filepath.Join(c.Dir, "config", "global.yaml"))
	if err != nil {
		return nil, fmt.Errorf("load captured global config: %w", err)
	}
	repoBytes, err := os.ReadFile(filepath.Join(c.Dir, "config", "repo-config.yaml"))
	if err != nil {
		return nil, fmt.Errorf("read captured repo config: %w", err)
	}
	repo, err := config.LoadRepoFromBytes(repoBytes)
	if err != nil {
		return nil, fmt.Errorf("load captured repo config: %w", err)
	}
	return config.Merge(global, repo), nil
}

func candidateModelArgs(candidate Candidate) []string {
	if candidate.Agent == types.AgentCodex {
		return []string{"-m", candidate.Model}
	}
	return []string{"--model", candidate.Model}
}

type observedAgent struct {
	inner      agent.Agent
	result     *agent.Result
	durationMS int64
}

func (a *observedAgent) Name() string { return a.inner.Name() }
func (a *observedAgent) Close() error { return a.inner.Close() }
func (a *observedAgent) Run(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
	started := time.Now()
	result, err := a.inner.Run(ctx, opts)
	a.durationMS += time.Since(started).Milliseconds()
	a.result = result
	return result, err
}

func hasAskUserFindings(raw string) bool {
	findings, err := types.ParseFindingsJSON(raw)
	return err == nil && types.HasAskUserFindings(findings)
}

func findingCount(raw string) int {
	findings, err := types.ParseFindingsJSON(raw)
	if err != nil {
		return 0
	}
	return len(findings.Items)
}

func (s *Store) persistEvaluation(c Case, evaluation Evaluation) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("eval registry is closed")
	}
	candidateDir := candidatePathPart(evaluation.Candidate)
	resultDir := filepath.Join(c.Dir, "evals", evaluation.SessionID, candidateDir)
	if err := os.MkdirAll(resultDir, 0o755); err != nil {
		return fmt.Errorf("create eval result directory: %w", err)
	}
	path := filepath.Join(resultDir, fmt.Sprintf("repeat-%03d.json", evaluation.Repeat))
	if err := writeJSON(path, evaluation); err != nil {
		return fmt.Errorf("write eval result: %w", err)
	}
	var expected any
	if evaluation.ExpectedPark != nil {
		if *evaluation.ExpectedPark {
			expected = 1
		} else {
			expected = 0
		}
	}
	parked := 0
	if evaluation.CandidateParked {
		parked = 1
	}
	reported := 0
	if evaluation.TokensReported {
		reported = 1
	}
	_, err := s.db.Exec(`INSERT INTO evaluations
(id, session_id, case_id, candidate, repeat_number, started_at, completed_at, status, expected_park, candidate_parked, tokens_reported, input_tokens, output_tokens, fresh_input_tokens, duration_ms, path)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		evaluation.ID, evaluation.SessionID, evaluation.CaseID, evaluation.Candidate, evaluation.Repeat,
		evaluation.StartedAt, evaluation.CompletedAt, evaluation.Status, expected, parked, reported,
		evaluation.InputTokens, evaluation.OutputTokens, evaluation.FreshInputTokens, evaluation.DurationMS, path)
	if err != nil {
		return fmt.Errorf("record eval result: %w", err)
	}
	if evaluation.Status == "completed" && evaluation.ExpectedPark != nil && !*evaluation.ExpectedPark && evaluation.CandidateParked {
		if err := incrementQueuedFindings(c.Dir); err != nil {
			return err
		}
	}
	return nil
}

func incrementQueuedFindings(caseDir string) error {
	var labels Labels
	if err := readJSON(filepath.Join(caseDir, "labels.json"), &labels); err != nil {
		return fmt.Errorf("read local labels queue: %w", err)
	}
	labels.QueuedCandidateFindings++
	if err := writeJSON(filepath.Join(caseDir, "labels.json"), labels); err != nil {
		return fmt.Errorf("update local labels queue: %w", err)
	}
	return nil
}

func candidatePathPart(candidate string) string {
	var b strings.Builder
	for _, r := range candidate {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '-' || r == '_' {
			b.WriteRune(r)
			continue
		}
		b.WriteByte('_')
	}
	if b.Len() == 0 {
		return "candidate"
	}
	return b.String()
}

func newSessionID() string {
	var bytes [8]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return fmt.Sprintf("%d-%s", time.Now().UnixNano(), hex.EncodeToString(bytes[:]))
}
