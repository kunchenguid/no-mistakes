package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Scenario is a list of canned responses matched against the prompt text.
// The first entry whose Match substring appears in the prompt wins. The
// final unconditional response is the default (matches everything).
type Scenario struct {
	Actions []Action `yaml:"actions"`
}

// Action describes a single canned response. Match is a substring tested
// against the prompt; an empty Match always matches and is treated as a
// catch-all when listed last. Edits are applied to the working directory
// before the response is emitted, so subsequent pipeline steps see the
// changes (this is how a "fix" round actually mutates files).
type Action struct {
	Match string `yaml:"match"`

	// Structured is the JSON body returned in the structured-output slot
	// (claude/grok result.structured_output, opencode.info.structured, or the
	// agent_message.text payload for codex). Encoded back to JSON when
	// emitted, so YAML authors can write it inline without escaping.
	Structured map[string]any `yaml:"structured,omitempty"`

	// StructuredRaw is emitted as the structured-output slot verbatim.
	// It is useful for testing parser fallback paths with non-object JSON.
	StructuredRaw string `yaml:"structured_raw,omitempty"`

	// Text is the human-readable response shown alongside structured
	// output. Defaults to a generic acknowledgement.
	Text string `yaml:"text,omitempty"`

	// Edits are file modifications applied in CWD before responding.
	Edits []Edit `yaml:"edits,omitempty"`

	// Stage lists paths to git-add after edits are applied.
	Stage []string `yaml:"stage,omitempty"`

	// Git runs git invocations in CWD after edits and staging, each entry the
	// argument list for one `git` call. File edits alone cannot express HOW an
	// agent ended an in-progress operation, which is exactly what the rebase
	// step's merge-shape guard judges: concluding a merge with
	// `git commit --no-edit`, abandoning it with `git merge --abort`, or
	// replacing it with a rebase are three different histories from the same
	// resolved files. A scenario needs to be able to produce each one.
	Git [][]string `yaml:"git,omitempty"`

	// DelayMS pauses before responding, for e2e tests that need an observable active run.
	DelayMS int `yaml:"delay_ms,omitempty"`
}

// Edit performs a Replace of Old with New in Path. If Old is empty the
// whole file is overwritten with New. If the file does not exist it is
// created.
type Edit struct {
	Path string `yaml:"path"`
	Old  string `yaml:"old,omitempty"`
	New  string `yaml:"new,omitempty"`
}

func loadScenario(path string) (*Scenario, error) {
	if path == "" {
		return defaultScenario(), nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read scenario %q: %w", path, err)
	}
	var s Scenario
	if err := yaml.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("parse scenario %q: %w", path, err)
	}
	return &s, nil
}

// defaultScenario returns an "everything is clean" response that satisfies
// every JSON schema no-mistakes hands to an agent: empty findings array,
// low risk, and for the test step a populated tested array plus the
// live-validation contract (one passing scenario and a go verdict) the step
// now requires of every evidence turn.
func defaultScenario() *Scenario {
	return &Scenario{
		Actions: []Action{{
			Text: "no issues found",
			Structured: map[string]any{
				"findings":        []any{},
				"summary":         "no issues found",
				"risk_level":      "low",
				"risk_rationale":  "no risks detected in the diff",
				"risk_scope":      "source-or-external",
				"tested":          []string{"fakeagent: simulated test run"},
				"testing_summary": "simulated tests passed",
				"artifacts":       []any{},
				"scenarios": []any{map[string]any{
					"name":     "fakeagent: simulated end-to-end scenario",
					"result":   "pass",
					"live":     true,
					"evidence": "fakeagent: simulated test run",
					// Present-but-empty rather than omitted: the codex adapter
					// rewrites every schema property as required-and-nullable,
					// so a canned response that omits an optional field fails
					// validation on that backend alone.
					"reason": "",
				}},
				"verdict": "go",
				"title":   "feat: fakeagent change",
				"body":    "## Summary\nfakeagent canned PR body",
			},
		}},
	}
}

// Match returns the first action whose Match substring is contained in the
// prompt. An empty Match matches everything, so a single trailing entry
// can serve as the catch-all. The process working directory is the
// worktree no-mistakes pointed the agent at; MatchInDir is the variant for
// adapters whose worktree arrives with the request instead.
func (s *Scenario) Match(prompt string) Action {
	wd, err := os.Getwd()
	if err != nil {
		wd = "."
	}
	return s.MatchInDir(wd, prompt)
}

// MatchInDir is Match for a worktree named explicitly. A review turn whose
// canned response omits reviewed_paths gets the worktree's own reviewable
// changed-file set filled in (see reviewCoverageForPrompt).
func (s *Scenario) MatchInDir(wd, prompt string) Action {
	for _, a := range s.Actions {
		if a.Match == "" || strings.Contains(prompt, a.Match) {
			return withReviewCoverage(wd, prompt, a)
		}
	}
	return Action{Text: "no matching scenario"}
}

// reviewPromptMarker opens every review turn's prompt (initial review and
// each post-fix rereview); the fix turn and every other step use different
// openers and are left alone.
const reviewPromptMarker = "Review the code changes and return structured findings"

// withReviewCoverage is the fixture-side stand-in for the coverage record a
// real reviewer reports. The production Review step fails closed when a
// clean round omits reviewed_paths (it parks for approval instead of
// certifying an unread head), so a canned review response that does not
// spell out reviewed_paths would park every e2e journey at the review gate.
// A scenario that names reviewed_paths itself - partial, empty, or
// fabricated - is passed through untouched so coverage tests can still
// exercise the gate; only an absent field is filled, and only on a review
// turn. The fill is the same set the step computes for that round: the
// files changed between the prompt's base commit and the worktree, minus
// the prompt's ignore patterns. This escape lives in the fake agent alone;
// there is no production default that stands in for a missing record.
func withReviewCoverage(wd, prompt string, a Action) Action {
	if !strings.Contains(prompt, reviewPromptMarker) || a.StructuredRaw != "" {
		return a
	}
	if a.Structured != nil {
		if _, present := a.Structured["reviewed_paths"]; present {
			return a
		}
	}
	paths, err := reviewCoverageForPrompt(wd, prompt)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fakeagent: review coverage: %v\n", err)
		return a
	}
	structured := make(map[string]any, len(a.Structured)+1)
	for k, v := range a.Structured {
		structured[k] = v
	}
	structured["reviewed_paths"] = paths
	a.Structured = structured
	return a
}

// reviewCoverageForPrompt derives the reviewable changed-file set for a
// review prompt: `git diff --name-only <base commit>` against the worktree
// (which equals base..HEAD when the tree is clean, in both the initial review
// and the post-fix rereview), filtered by the prompt's ignore patterns with
// the same matching rules the pipeline applies (basename glob for a bare
// pattern, prefix for `dir/**`, full-path glob otherwise). Paths are
// returned as an empty, non-nil slice when nothing is reviewable so the
// field is still reported.
func reviewCoverageForPrompt(wd, prompt string) ([]string, error) {
	base := promptContextValue(prompt, "base commit")
	if base == "" {
		return nil, errors.New("review prompt has no base commit line")
	}
	var ignore []string
	if raw := promptContextValue(prompt, "ignore patterns"); raw != "" && raw != "none" {
		for _, pattern := range strings.Split(raw, ",") {
			if pattern = strings.TrimSpace(pattern); pattern != "" {
				ignore = append(ignore, pattern)
			}
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "diff", "--name-only", "-z", "--no-renames", base)
	cmd.Dir = wd
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git diff --name-only %s in %s: %w", base, wd, err)
	}
	paths := []string{}
	for _, file := range strings.Split(string(out), "\x00") {
		if file == "" || ignoredByPatterns(file, ignore) {
			continue
		}
		paths = append(paths, file)
	}
	return paths, nil
}

// promptContextValue reads a `- <key>: <value>` line from the prompt's
// Context block.
func promptContextValue(prompt, key string) string {
	prefix := "- " + key + ": "
	for _, line := range strings.Split(prompt, "\n") {
		if strings.HasPrefix(line, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(line, prefix))
		}
	}
	return ""
}

// ignoredByPatterns mirrors the pipeline's matchIgnorePattern
// (internal/pipeline/steps/common_diff.go) so the fake reviewer's coverage is
// the exact set the step holds it to.
func ignoredByPatterns(file string, patterns []string) bool {
	for _, pattern := range patterns {
		if strings.HasSuffix(pattern, "/**") {
			prefix := strings.TrimSuffix(pattern, "/**")
			if file == prefix || strings.HasPrefix(file, prefix+"/") {
				return true
			}
			continue
		}
		if !strings.Contains(pattern, "/") {
			if matched, _ := path.Match(pattern, path.Base(file)); matched {
				return true
			}
			continue
		}
		if matched, _ := path.Match(pattern, file); matched {
			return true
		}
	}
	return false
}

// applyEdits mutates files under CWD (which is the worktree no-mistakes
// pointed the agent at). Errors are logged to stderr but not fatal so a
// scenario with a stale path doesn't kill the whole run.

func applyEdits(edits []Edit) error {
	wd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("get working directory: %w", err)
	}
	return applyEditsInDir(wd, edits)
}

func applyAction(action Action) error {
	wd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("get working directory: %w", err)
	}
	return applyActionInDir(wd, action)
}

func applyActionInDir(wd string, action Action) error {
	if action.DelayMS > 0 {
		time.Sleep(time.Duration(action.DelayMS) * time.Millisecond)
	}
	if err := applyEditsInDir(wd, action.Edits); err != nil {
		return err
	}
	if err := stageFilesInDir(wd, action.Stage); err != nil {
		return err
	}
	return runGitInDir(wd, action.Git)
}

// runGitInDir runs each scenario git invocation in wd. A failure is returned
// rather than logged: a scenario that says "conclude the merge" and silently
// did not would make the pipeline's own guard look like the thing under test.
func runGitInDir(wd string, invocations [][]string) error {
	for _, args := range invocations {
		if len(args) == 0 {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = wd
		out, err := cmd.CombinedOutput()
		cancel()
		if err != nil {
			return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, out)
		}
	}
	return nil
}

func applyEditsInDir(wd string, edits []Edit) error {
	wd, err := filepath.Abs(wd)
	if err != nil {
		return fmt.Errorf("resolve working directory: %w", err)
	}

	var errs []error
	for _, e := range edits {
		if e.Path == "" {
			continue
		}
		path, err := scenarioEditPath(wd, e.Path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "fakeagent: %v\n", err)
			errs = append(errs, err)
			continue
		}
		if e.Old == "" {
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				err = fmt.Errorf("mkdir %s: %w", e.Path, err)
				fmt.Fprintf(os.Stderr, "fakeagent: %v\n", err)
				errs = append(errs, err)
				continue
			}
			if err := os.WriteFile(path, []byte(e.New), 0o644); err != nil {
				err = fmt.Errorf("write %s: %w", e.Path, err)
				fmt.Fprintf(os.Stderr, "fakeagent: %v\n", err)
				errs = append(errs, err)
			}
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			err = fmt.Errorf("read %s: %w", e.Path, err)
			fmt.Fprintf(os.Stderr, "fakeagent: %v\n", err)
			errs = append(errs, err)
			continue
		}
		if !strings.Contains(string(data), e.Old) {
			err = fmt.Errorf("replace %s: old text not found", e.Path)
			fmt.Fprintf(os.Stderr, "fakeagent: %v\n", err)
			errs = append(errs, err)
			continue
		}
		updated := strings.Replace(string(data), e.Old, e.New, 1)
		if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
			err = fmt.Errorf("write %s: %w", e.Path, err)
			fmt.Fprintf(os.Stderr, "fakeagent: %v\n", err)
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func stageFilesInDir(wd string, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	relPaths := make([]string, 0, len(paths))
	for _, path := range paths {
		if path == "" {
			continue
		}
		full, err := scenarioEditPath(wd, path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(wd, full)
		if err != nil {
			return fmt.Errorf("stage %q: %w", path, err)
		}
		relPaths = append(relPaths, rel)
	}
	if len(relPaths) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	args := append([]string{"add", "--"}, relPaths...)
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = wd
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git add staged files: %w: %s", err, out)
	}
	return nil
}

func scenarioEditPath(wd, path string) (string, error) {
	if filepath.IsAbs(path) {
		return "", fmt.Errorf("path %q must stay under working directory", path)
	}
	clean := filepath.Clean(path)
	full := filepath.Join(wd, clean)
	rel, err := filepath.Rel(wd, full)
	if err != nil {
		return "", fmt.Errorf("resolve %q: %w", path, err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q must stay under working directory", path)
	}
	base, err := scenarioExistingBasePath(full)
	if err != nil {
		return "", fmt.Errorf("resolve %q: %w", path, err)
	}
	if err := scenarioPathWithinWorkingDirectory(wd, base); err != nil {
		return "", fmt.Errorf("path %q must stay under working directory", path)
	}
	return full, nil
}

func scenarioExistingBasePath(path string) (string, error) {
	current := path
	for {
		if _, err := os.Lstat(current); err == nil {
			return filepath.EvalSymlinks(current)
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		next := filepath.Dir(current)
		if next == current {
			return "", fmt.Errorf("no existing path for %q", path)
		}
		current = next
	}
}

func scenarioPathWithinWorkingDirectory(wd, path string) error {
	resolvedWD, err := filepath.EvalSymlinks(wd)
	if err != nil {
		resolvedWD = wd
	}
	rel, err := filepath.Rel(resolvedWD, path)
	if err != nil {
		return err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("path escapes working directory")
	}
	return nil
}

// structuredJSON marshals an action's Structured map. Empty structured
// becomes an empty object so the parser sees something parseable.
func (a Action) structuredJSON() []byte {
	if a.StructuredRaw != "" {
		return []byte(a.StructuredRaw)
	}
	if a.Structured == nil {
		return []byte("{}")
	}
	data, err := json.Marshal(a.Structured)
	if err != nil {
		return []byte("{}")
	}
	return data
}

func (a Action) hasStructuredOutput() bool {
	return a.Structured != nil || a.StructuredRaw != ""
}

func (a Action) textOrDefault() string {
	if a.Text != "" {
		return a.Text
	}
	return "ok"
}
