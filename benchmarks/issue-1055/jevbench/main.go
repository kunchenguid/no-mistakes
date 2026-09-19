// Command jevbench measures the opt-in Jev review pre-brief
// (jev.review_assist, issue #1055) on real, cold, session-free review
// launches: it drives the production ReviewStep against a real agent CLI and
// the live TypeSafe API, once per launch, alternating assist off and on over
// the same change.
//
// The launch is the production review turn with everything around it removed:
// no daemon, no executor, no fixer. Each launch gets a fresh git worktree and
// a fresh agent invocation, which is exactly the cold, session-free review the
// pipeline runs; RunSessions is nil, so no session state can leak between
// launches. What the pipeline would add around the turn (gate bookkeeping,
// prompt round history) does not affect what this benchmark measures: the
// billed tokens and wall time of one complete review launch.
//
// Usage:
//
//	go run ./benchmarks/issue-1055/jevbench -repo <repo> -base <sha> -head <sha> \
//	  -out <dir> -change <name> [-reps 3] [-agent pi] [-model m] [-effort xhigh]
//
// The harness resumes: launches already recorded in the evidence file are not
// re-run, so a caller can invoke it repeatedly until both modes reach -reps.
// TYPESAFE_API_KEY must be in the environment for the on-mode launches; its
// value is never printed or persisted.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// launchRecord is one cold review launch's evidence row.
type launchRecord struct {
	Change    string `json:"change"`
	Mode      string `json:"mode"` // "off" or "on"
	BaseSHA   string `json:"base_sha"`
	HeadSHA   string `json:"head_sha"`
	StartedAt string `json:"started_at"`
	WallMS    int64  `json:"wall_ms"`
	// Attempts counts every agent attempt the launch made: review reruns
	// after a schema rejection and adapter-internal retries alike. The token
	// counters sum all of them, and UsageReported is false when any attempt
	// reported no usage, so a launch that needed reruns never reads as cheaper.
	Attempts        int  `json:"attempts"`
	UsageReported   bool `json:"usage_reported"`
	InputTokens     int  `json:"input_tokens"`
	OutputTokens    int  `json:"output_tokens"`
	CacheReadTokens int  `json:"cache_read_tokens"`
	// Token semantics are adapter-specific: Pi reports uncached input and
	// cache reads as separate counters, so input_tokens IS the fresh input
	// and no derived field is computed here; results.md does the arithmetic
	// with the adapter's documented semantics.
	ReportedModel string `json:"reported_model"`
	Findings      int    `json:"findings"`
	ReviewedPaths int    `json:"reviewed_paths"`
	RiskLevel     string `json:"risk_level"`
	NeedsApproval bool   `json:"needs_approval"`
	// JevNote is the step-log line the pre-brief emitted (counts, model,
	// tokens), or the fallback line when the assist degraded. Empty in
	// off-mode. It is the production log line, so it never carries content.
	JevNote string `json:"jev_note"`
	Error   string `json:"error,omitempty"`
}

// meteringAgent sums the usage of every attempt a launch makes, the way eval
// replay's observedAgent does: the review step can rerun its reviewer, and the
// adapter retries below this seam and hands back only its last attempt.
type meteringAgent struct {
	inner        agent.Agent
	attempts     int
	usage        agent.TokenUsage
	usageMissing bool
	model        string
}

func (m *meteringAgent) Name() string { return m.inner.Name() }
func (m *meteringAgent) Close() error { return m.inner.Close() }
func (m *meteringAgent) Run(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
	reported := 0
	previous := opts.OnAttempt
	opts.OnAttempt = func(attempt agent.Attempt) {
		if previous != nil {
			previous(attempt)
		}
		reported++
		m.observe(attempt.Result)
	}
	result, err := m.inner.Run(ctx, opts)
	if reported == 0 {
		reported = 1
		m.observe(result)
	}
	m.attempts += reported
	return result, err
}

func (m *meteringAgent) observe(result *agent.Result) {
	if result == nil || !result.UsageReported {
		m.usageMissing = true
		return
	}
	m.usage.InputTokens += result.Usage.InputTokens
	m.usage.OutputTokens += result.Usage.OutputTokens
	m.usage.CacheReadTokens += result.Usage.CacheReadTokens
	if result.Model != "" {
		m.model = result.Model
	}
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "jevbench:", err)
		os.Exit(1)
	}
}

func run() error {
	repo := flag.String("repo", ".", "repository the change lives in")
	base := flag.String("base", "", "base commit SHA (required)")
	head := flag.String("head", "", "head commit SHA (required)")
	change := flag.String("change", "change", "corpus change name, for the evidence rows")
	outDir := flag.String("out", "", "evidence directory (required)")
	reps := flag.Int("reps", 3, "completed launches per mode")
	agentName := flag.String("agent", string(types.AgentPi), "pipeline agent harness")
	model := flag.String("model", "", "model override (default: harness default)")
	effort := flag.String("effort", "", "effort override (default: harness default)")
	flag.Parse()

	if *base == "" || *head == "" || *outDir == "" {
		return errors.New("-base, -head, and -out are required")
	}
	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		return err
	}
	evidencePath := filepath.Join(*outDir, "launches-"+*change+".jsonl")
	records, err := readRecords(evidencePath)
	if err != nil {
		return err
	}

	off, on := 0, 0
	for _, r := range records {
		if r.Error == "" {
			if r.Mode == "off" {
				off++
			} else {
				on++
			}
		}
	}
	if off >= *reps && on >= *reps {
		fmt.Printf("jevbench: %s already has %d off + %d on completed launches; nothing to do\n", *change, off, on)
		return nil
	}
	// Alternate modes so provider-side caching and machine load spread evenly
	// across the comparison arms.
	mode := "off"
	if off > on {
		mode = "on"
	}
	if off >= *reps {
		mode = "on"
	}
	if on >= *reps {
		mode = "off"
	}

	record, err := runLaunch(*repo, *base, *head, *change, mode, *agentName, *model, *effort)
	if err != nil {
		return err
	}
	return appendRecord(evidencePath, record)
}

func runLaunch(repo, base, head, change, mode, agentName, model, effort string) (launchRecord, error) {
	record := launchRecord{
		Change:    change,
		Mode:      mode,
		BaseSHA:   base,
		HeadSHA:   head,
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}

	// A fresh worktree per launch: the reviewer reads real files, and nothing
	// carries over between launches. Worktrees share the object store, so this
	// is cheap.
	fixture, err := os.MkdirTemp("", "jevbench-*")
	if err != nil {
		return record, err
	}
	defer os.RemoveAll(fixture)
	fixtureDir := filepath.Join(fixture, "wt")
	if out, err := exec.Command("git", "-C", repo, "worktree", "add", "--detach", fixtureDir, head).CombinedOutput(); err != nil {
		return record, fmt.Errorf("create fixture worktree: %v: %s", err, out)
	}
	defer func() {
		_ = exec.Command("git", "-C", repo, "worktree", "remove", "--force", fixtureDir).Run()
	}()

	profile := agentcfg.Profile{Model: model, Effort: agentcfg.Effort(effort)}
	inner, err := agent.NewWithOptions(types.AgentName(agentName), agentName, nil, agent.Options{Profile: profile})
	if err != nil {
		return record, fmt.Errorf("build agent: %w", err)
	}
	defer inner.Close()
	metered := &meteringAgent{inner: inner}

	cfg := &config.Config{
		Agent:              types.AgentName(agentName),
		ReviewAgentTimeout: config.DefaultReviewAgentTimeout,
		Jev:                config.Jev{ReviewAssist: mode == "on"},
	}
	var logLines []string
	sctx := &pipeline.StepContext{
		Ctx: context.Background(),
		Run: &db.Run{ID: "jevbench-" + change, Branch: "refs/heads/jevbench", HeadSHA: head, BaseSHA: base},
		// DefaultBranch stays empty on purpose: resolveBranchBaseSHA would
		// otherwise merge-base against it, and a corpus head that is an
		// ancestor of the real default branch would diff against itself.
		// Empty falls back to the explicit -base, which is the comparison
		// this benchmark controls.
		Repo:    &db.Repo{ID: "jevbench", WorkingPath: fixtureDir},
		WorkDir: fixtureDir,
		Agent:   metered,
		Config:  cfg,
		Log:     func(s string) { logLines = append(logLines, s) },
	}

	start := time.Now()
	outcome, err := (&steps.ReviewStep{}).Execute(sctx)
	record.WallMS = time.Since(start).Milliseconds()
	record.Attempts = metered.attempts
	record.UsageReported = metered.attempts > 0 && !metered.usageMissing
	record.InputTokens = metered.usage.InputTokens
	record.OutputTokens = metered.usage.OutputTokens
	record.CacheReadTokens = metered.usage.CacheReadTokens
	record.ReportedModel = metered.model
	if err != nil {
		// A failed launch is evidence too: record it with the error and let
		// the caller decide whether the sample stands.
		record.Error = err.Error()
		return record, nil
	}
	if outcome != nil {
		var findings steps.Findings
		if err := json.Unmarshal([]byte(outcome.Findings), &findings); err == nil {
			record.Findings = len(findings.Items)
			record.RiskLevel = findings.RiskLevel
		}
		record.ReviewedPaths = len(outcome.ReviewedPaths)
		record.NeedsApproval = outcome.NeedsApproval
	}
	for _, line := range logLines {
		if strings.Contains(line, "jev") {
			record.JevNote = line
		}
	}
	return record, nil
}

func readRecords(path string) ([]launchRecord, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var records []launchRecord
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var r launchRecord
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		records = append(records, r)
	}
	return records, nil
}

func appendRecord(path string, record launchRecord) error {
	encoded, err := json.Marshal(record)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(append(encoded, '\n')); err != nil {
		return err
	}
	fmt.Printf("jevbench: %s %s launch complete: wall=%ds attempts=%d input=%d output=%d cache_read=%d findings=%d jev=%q err=%q\n",
		record.Change, record.Mode, record.WallMS/1000, record.Attempts, record.InputTokens, record.OutputTokens, record.CacheReadTokens, record.Findings, record.JevNote, record.Error)
	return nil
}
