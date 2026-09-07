package eval

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/e2edaemon"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/safeurl"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// ReplayOptions controls one isolated candidate comparison. The optional
// callbacks observe progress for interactive rendering: OnPlan fires once the
// case set is reserved and the session is recorded, OnResult after each
// replay's evaluation is persisted. Both run synchronously on the replay
// goroutine and may be nil.
type ReplayOptions struct {
	Set       string
	Candidate Candidate
	Repeats   int
	OnPlan    func(session Session, cases []Case)
	OnResult  func(evaluation Evaluation, completed, total int)
}

// Session records the immutable local plan used for one replay batch.
type Session struct {
	ID        string    `json:"id"`
	StartedAt time.Time `json:"started_at"`
	Set       string    `json:"set"`
	Candidate string    `json:"candidate"`
	Repeats   int       `json:"repeats"`
	CaseIDs   []string  `json:"case_ids"`
	Cohort    string    `json:"cohort"`
}

const (
	replayReservationLease   = 5 * time.Minute
	replayReservationRefresh = time.Minute
)

// Replay runs exactly the captured review pass. It does not start a daemon or
// use the production NM_HOME: every case is restored into a fresh temp gate and
// worktree. Candidates inherit the caller's HOME so harness sign-in matches
// an ordinary pipeline agent spawn. Push, PR, CI, and all fix loops are
// intentionally absent from the MVP subject under test.
func Replay(ctx context.Context, store *Store, opts ReplayOptions) (Session, []Evaluation, error) {
	if store == nil {
		return Session{}, nil, fmt.Errorf("eval replay requires a store")
	}
	if opts.Repeats <= 0 {
		return Session{}, nil, fmt.Errorf("repeats must be at least 1")
	}
	if err := opts.Candidate.Validate(); err != nil {
		return Session{}, nil, err
	}
	cases, session, err := store.prepareReplay(ctx, opts)
	if err != nil {
		return Session{}, nil, err
	}
	replayCtx, cancelReplay := context.WithCancel(ctx)
	stopReservation := store.keepReplayReservation(replayCtx, cancelReplay, session.ID)
	defer func() {
		cancelReplay()
		stopReservation()
		store.releaseReplayReservation(session.ID)
	}()

	if opts.OnPlan != nil {
		opts.OnPlan(session, cases)
	}
	total := len(cases) * opts.Repeats
	evaluations := make([]Evaluation, 0, total)
	var failed int
	for repeat := 1; repeat <= opts.Repeats; repeat++ {
		for _, c := range cases {
			evaluation := replayOne(replayCtx, store, c, session, opts.Candidate, repeat)
			if evaluation.Status != "completed" {
				failed++
			}
			if err := store.persistEvaluation(c, evaluation); err != nil {
				return session, evaluations, err
			}
			evaluations = append(evaluations, evaluation)
			if opts.OnResult != nil {
				opts.OnResult(evaluation, len(evaluations), total)
			}
		}
	}
	if failed > 0 {
		return session, evaluations, fmt.Errorf("%d replay invocation(s) failed; inspect eval report for the local failure count", failed)
	}
	return session, evaluations, nil
}

func (s *Store) prepareReplay(ctx context.Context, opts ReplayOptions) ([]Case, Session, error) {
	unlock, err := lockCorpus(ctx, s.root)
	if err != nil {
		return nil, Session{}, err
	}
	defer unlock()

	cases, err := s.ListCases(opts.Set)
	if err != nil {
		return nil, Session{}, err
	}
	if len(cases) == 0 {
		return nil, Session{}, fmt.Errorf("case set %q is empty", opts.Set)
	}
	session := Session{ID: newSessionID(), StartedAt: time.Now().UTC(), Set: opts.Set, Candidate: opts.Candidate.String(), Repeats: opts.Repeats}
	for _, c := range cases {
		session.CaseIDs = append(session.CaseIDs, c.ID)
	}
	session.Cohort = cohortID(session.CaseIDs, session.Repeats)
	sessionsDir := filepath.Join(s.root, "sessions")
	if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
		return nil, Session{}, fmt.Errorf("create eval sessions directory: %w", err)
	}
	sessionPath := filepath.Join(sessionsDir, session.ID+".json")
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, Session{}, fmt.Errorf("begin eval replay reservation: %w", err)
	}
	reservedUntil := time.Now().Add(replayReservationLease).Unix()
	for _, c := range cases {
		if _, err := tx.ExecContext(ctx, `INSERT INTO replay_case_reservations (session_id, case_id, reserved_until) VALUES (?, ?, ?)`, session.ID, c.ID, reservedUntil); err != nil {
			_ = tx.Rollback()
			return nil, Session{}, fmt.Errorf("reserve eval case %q: %w", c.ID, err)
		}
	}
	if err := writeJSON(sessionPath, session); err != nil {
		_ = tx.Rollback()
		return nil, Session{}, fmt.Errorf("write eval session: %w", err)
	}
	if err := tx.Commit(); err != nil {
		_ = os.Remove(sessionPath)
		return nil, Session{}, fmt.Errorf("commit eval replay reservation: %w", err)
	}
	return cases, session, nil
}

func (s *Store) keepReplayReservation(ctx context.Context, cancelReplay context.CancelFunc, sessionID string) func() {
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(replayReservationRefresh)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-stop:
				return
			case <-ticker.C:
				reservedUntil := time.Now().Add(replayReservationLease).Unix()
				if _, err := s.db.ExecContext(ctx, `UPDATE replay_case_reservations SET reserved_until = ? WHERE session_id = ?`, reservedUntil, sessionID); err != nil {
					cancelReplay()
					return
				}
			}
		}
	}()
	return func() {
		close(stop)
		<-done
	}
}

func (s *Store) releaseReplayReservation(sessionID string) {
	_, _ = s.db.Exec(`DELETE FROM replay_case_reservations WHERE session_id = ?`, sessionID)
}

func replayOne(ctx context.Context, store *Store, c Case, session Session, candidate Candidate, repeat int) (evaluation Evaluation) {
	started := time.Now()
	evaluation = Evaluation{
		ID:        newSessionID(),
		SessionID: session.ID,
		CaseID:    c.ID,
		Candidate: candidate.String(),
		Cohort:    session.Cohort,
		Repeat:    repeat,
		StartedAt: started.Unix(),
		Status:    "failed",
	}
	evaluation.HasFindingGold = c.Labels.HasGold()
	evaluation.GoldCount = c.Labels.TrueIssueCount()
	evaluation.FalsePositiveGold = c.Labels.FalsePositiveCount()
	defer func() {
		if evaluation.Status != "completed" {
			evaluation.FalseNegative = evaluation.GoldCount
			if evaluation.CompletedAt == 0 {
				evaluation.CompletedAt = time.Now().Unix()
			}
		}
	}()

	// The replay sandbox deliberately stays OUTSIDE the source NM_HOME: it
	// materializes its own nested NM_HOME and worktree, and the eval store,
	// object pools, and Store.Prune all live under <NM_HOME>/eval, so a live
	// sandbox nested there would sit inside the very state it is replaying.
	// That isolation outranks moving it off the system temp directory; see the
	// held-scope note in AGENTS.md ("no-mistakes Owns Its Own Scratch").
	root, err := os.MkdirTemp("", "nm-eval-replay-")
	if err != nil {
		evaluation.Error = safeurl.RedactText(fmt.Sprintf("create isolated replay root: %v", err))
		evaluation.CompletedAt = time.Now().Unix()
		return evaluation
	}
	defer os.RemoveAll(root)

	isolatedPaths := paths.WithRoot(filepath.Join(root, "nmhome"))
	if err := isolatedPaths.EnsureDirs(); err != nil {
		evaluation.Error = safeurl.RedactText(fmt.Sprintf("create isolated eval state: %v", err))
		evaluation.CompletedAt = time.Now().Unix()
		return evaluation
	}
	ownership, err := e2edaemon.Acquire(isolatedPaths.Root(), "", 2*time.Minute)
	if err != nil {
		evaluation.Error = safeurl.RedactText(fmt.Sprintf("acquire isolated eval ownership: %v", err))
		evaluation.CompletedAt = time.Now().Unix()
		return evaluation
	}
	defer ownership.Release()

	workDir, err := restoreCase(ctx, store, c, root)
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

	// The candidate's tuning goes through the same harness-neutral Profile the
	// pipeline uses, so eval and a real run reach each harness's model and
	// effort mechanism by exactly one code path. Raw args stay empty: capture
	// strips agent_args_override and agent_config from the pinned config so a
	// replay cannot inherit the capturing machine's own pins.
	baseAgent, err := agent.NewWithOptions(candidate.Agent, cfg.AgentPathFor(candidate.Agent), nil, agent.Options{
		ACPRegistryOverrides:   cfg.ACPRegistryOverrides,
		DisableProjectSettings: cfg.DisableProjectSettings,
		Profile:                candidate.Profile(),
	})
	if err != nil {
		evaluation.Error = safeurl.RedactText(fmt.Sprintf("create candidate agent: %v", err))
		evaluation.CompletedAt = time.Now().Unix()
		return evaluation
	}
	defer baseAgent.Close()
	observed := &observedAgent{inner: agent.WithSteering(baseAgent, isolatedPaths.EvidenceDir()), ownership: ownership}

	replayDB, stepResultID, fixing, previousFindings, err := replayRoundContext(isolatedPaths, c, workDir)
	if err != nil {
		evaluation.Error = safeurl.RedactText(err.Error())
		evaluation.CompletedAt = time.Now().Unix()
		return evaluation
	}
	defer replayDB.Close()

	startingHeadSHA := c.StartingHeadSHA
	if startingHeadSHA == "" {
		startingHeadSHA = c.ReviewedHeadSHA
	}
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
		Ctx:                   ctx,
		Run:                   replayRun,
		Repo:                  replayRepo,
		WorkDir:               workDir,
		Agent:                 observed,
		Config:                cfg,
		DB:                    replayDB,
		StepResultID:          stepResultID,
		Fixing:                fixing,
		SkipFixExecution:      fixing,
		ReviewStartingHeadSHA: startingHeadSHA,
		PreviousFindings:      previousFindings,
		// Keep NM_HOME on the nested sandbox so replay cannot see or mutate
		// production pipeline/eval state. Do not rewrite HOME: candidates use
		// the same harness sign-in and user settings as an ordinary pipeline
		// agent spawn. That is not a security sandbox; a candidate may still
		// read and write ordinary HOME-relative agent files.
		Env:          []string{"NM_HOME=" + isolatedPaths.Root()},
		Log:          func(string) {},
		LogChunk:     func(string) {},
		LogFile:      func(string) {},
		UserIntent:   c.Intent,
		IntentSource: c.IntentSource,
	})
	// Candidate wall time is the actual review invocation, matching the local
	// agent-invocation metric rather than charging case restoration setup.
	evaluation.DurationMS = observed.durationMS
	if evaluation.DurationMS == 0 && observed.result == nil {
		evaluation.DurationMS = time.Since(started).Milliseconds()
	}
	evaluation.CompletedAt = time.Now().Unix()
	if observed.result != nil {
		evaluation.Model = observed.result.Model
		if evaluation.Model == "" {
			evaluation.Model = candidate.Model
		} else if !agentcfg.ServedMatchesRequested(candidate.Model, evaluation.Model, observed.result.ModelProvider) {
			evaluation.Error = safeurl.RedactText(fmt.Sprintf("candidate served model %q, requested %q", evaluation.Model, candidate.Model))
			return evaluation
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
	evaluation.FindingsJSON = outcome.Findings
	evaluation.FindingCount = findingCount(outcome.Findings)
	score := ScoreCandidate(c.Labels, outcome.Findings)
	evaluation.TruePositive = score.TruePositive
	evaluation.TruePositiveExact = score.TruePositiveExact
	evaluation.TruePositiveFuzzy = score.TruePositiveFuzzy
	evaluation.FalseNegative = score.FalseNegative
	evaluation.FalsePositive = score.FalsePositive
	evaluation.FalsePositiveGold = score.FalsePositiveGold
	evaluation.Pending = score.Pending
	return evaluation
}

func replayRoundContext(p *paths.Paths, c Case, workDir string) (*db.DB, string, bool, string, error) {
	var selected sourceRound
	if err := readJSON(filepath.Join(c.Dir, "original", "round.json"), &selected); err != nil {
		return nil, "", false, "", fmt.Errorf("read captured review round: %w", err)
	}
	var rounds []sourceRound
	if err := readJSON(filepath.Join(c.Dir, "original", "rounds.json"), &rounds); err != nil {
		return nil, "", false, "", fmt.Errorf("read captured round history: %w", err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		return nil, "", false, "", fmt.Errorf("open isolated replay database: %w", err)
	}
	fail := func(err error) (*db.DB, string, bool, string, error) {
		database.Close()
		return nil, "", false, "", err
	}
	repo, err := database.InsertRepoWithID("eval-repo", workDir, "local://eval", c.DefaultBranch)
	if err != nil {
		return fail(fmt.Errorf("create isolated replay repository: %w", err))
	}
	run, err := database.InsertRun(repo.ID, c.Branch, c.ReviewedHeadSHA, c.BaseSHA)
	if err != nil {
		return fail(fmt.Errorf("create isolated replay run: %w", err))
	}
	step, err := database.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		return fail(fmt.Errorf("create isolated replay step: %w", err))
	}
	var previousFindings string
	for _, round := range rounds {
		if round.StepName != "" && round.StepName != string(types.StepReview) {
			continue
		}
		if round.Round >= selected.Round {
			continue
		}
		inserted, err := database.InsertReviewStepRound(step.ID, round.Round, round.Trigger, round.FindingsJSON, round.FixSummary, stringValue(round.ReviewedHeadSHA), round.DurationMS)
		if err != nil {
			return fail(fmt.Errorf("restore captured review history: %w", err))
		}
		if round.UserFindingsJSON != nil {
			if err := database.SetStepRoundUserFindings(inserted.ID, round.UserFindingsJSON); err != nil {
				return fail(fmt.Errorf("restore captured user findings: %w", err))
			}
		}
		if round.SelectedFindingIDs != nil {
			source := stringValue(round.SelectionSource)
			if err := database.SetStepRoundSelection(inserted.ID, round.SelectedFindingIDs, source); err != nil {
				return fail(fmt.Errorf("restore captured gate decision: %w", err))
			}
		}
		if round.Round == selected.Round-1 {
			previousFindings = stringValue(round.UserFindingsJSON)
			if previousFindings == "" {
				previousFindings = stringValue(round.FindingsJSON)
			}
		}
	}
	fixing := selected.Trigger == "auto_fix" || selected.Trigger == "user_fix"
	if fixing && previousFindings == "" {
		return fail(fmt.Errorf("captured fix round %d has no previous findings", selected.Round))
	}
	return database, step.ID, fixing, previousFindings, nil
}

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func restoreCase(ctx context.Context, store *Store, c Case, root string) (string, error) {
	gateDir := filepath.Join(root, "gate.git")
	if err := git.InitBare(ctx, gateDir); err != nil {
		return "", fmt.Errorf("create isolated eval gate: %w", err)
	}
	if err := restoreCaseObjects(ctx, store.poolDir(c.RepoFingerprint), gateDir, c.ID); err != nil {
		return "", err
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

type observedAgent struct {
	inner        agent.Agent
	ownership    *e2edaemon.Ownership
	result       *agent.Result
	durationMS   int64
	ownershipErr error
	mu           sync.Mutex
}

func (a *observedAgent) Name() string { return a.inner.Name() }
func (a *observedAgent) Close() error { return a.inner.Close() }
func (a *observedAgent) Run(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
	previousLifecycle := opts.OnLifecycle
	opts.OnLifecycle = func(event agent.LifecycleEvent) {
		if event.Phase == "start" && event.PID > 0 {
			if syncErr := a.ownership.SyncProcess(event.PID); syncErr != nil {
				a.mu.Lock()
				if a.ownershipErr == nil {
					a.ownershipErr = syncErr
				}
				a.mu.Unlock()
			}
		}
		if previousLifecycle != nil {
			previousLifecycle(event)
		}
	}
	started := time.Now()
	result, err := a.inner.Run(ctx, opts)
	a.durationMS += time.Since(started).Milliseconds()
	a.result = result
	a.mu.Lock()
	ownershipErr := a.ownershipErr
	a.mu.Unlock()
	if ownershipErr != nil {
		return result, ownershipErr
	}
	return result, err
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
	reported := 0
	if evaluation.TokensReported {
		reported = 1
	}
	_, err := s.db.Exec(`INSERT INTO evaluations
(id, session_id, case_id, candidate, repeat_number, started_at, completed_at, status, gold_count, true_positive, false_negative, false_positive, pending, tokens_reported, input_tokens, output_tokens, fresh_input_tokens, duration_ms, path)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		evaluation.ID, evaluation.SessionID, evaluation.CaseID, evaluation.Candidate, evaluation.Repeat,
		evaluation.StartedAt, evaluation.CompletedAt, evaluation.Status, evaluation.GoldCount,
		evaluation.TruePositive, evaluation.FalseNegative, evaluation.FalsePositive, evaluation.Pending, reported,
		evaluation.InputTokens, evaluation.OutputTokens, evaluation.FreshInputTokens, evaluation.DurationMS, path)
	if err != nil {
		return fmt.Errorf("record eval result: %w", err)
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

func cohortID(caseIDs []string, repeats int) string {
	ids := append([]string(nil), caseIDs...)
	sort.Strings(ids)
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d\x00%s", repeats, strings.Join(ids, "\x00"))))
	return hex.EncodeToString(sum[:8])
}

func newSessionID() string {
	var bytes [8]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return fmt.Sprintf("%d-%s", time.Now().UnixNano(), hex.EncodeToString(bytes[:]))
}
