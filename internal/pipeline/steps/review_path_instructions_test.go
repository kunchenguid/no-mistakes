package steps

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
)

// wantBlock builds the exact rendered block for one rule so assertions compare
// full expected output rather than checking that a substring is present.
func wantBlock(path, files, instructions string) string {
	return config.ReviewPathInstructionsPathLabel + path + "\n" +
		config.ReviewPathInstructionsFilesLabel + files + "\n" +
		config.ReviewPathInstructionsRulesLabel + "\n" +
		instructions
}

func wantSection(blocks ...string) string {
	if len(blocks) == 0 {
		return ""
	}
	return "\n\n" + config.ReviewPathInstructionsHeading + "\n" + strings.Join(blocks, "\n\n")
}

func sectionFor(changed []string, rules []config.PathInstruction) string {
	return reviewPathInstructionsSection(matchPathInstructions(changed, rules))
}

func TestMatchPathInstructions(t *testing.T) {
	t.Parallel()

	scm := config.PathInstruction{Path: "internal/scm/**", Instructions: "Credential-carrying URLs must go through internal/safeurl."}
	docs := config.PathInstruction{Path: "docs/**", Instructions: "Prose changes only. Do not request test coverage."}
	generated := config.PathInstruction{Path: "*.generated.go", Instructions: "Generated code is out of scope."}
	exact := config.PathInstruction{Path: "internal/config/config.go", Instructions: "Every new field needs a trust decision."}

	tests := []struct {
		name          string
		changed       []string
		rules         []config.PathInstruction
		wantBlocks    []pathInstructionBlock
		wantUnmatched []string
		wantDuplicate []string
		wantUnusable  []string
	}{
		{
			name:    "subtree pattern matches nested files and records them",
			changed: []string{"internal/scm/github/github.go", "internal/scm/gitlab/gitlab.go", "README.md"},
			rules:   []config.PathInstruction{scm},
			wantBlocks: []pathInstructionBlock{{
				Path:         "internal/scm/**",
				Instructions: scm.Instructions,
				Files:        []string{"internal/scm/github/github.go", "internal/scm/gitlab/gitlab.go"},
			}},
		},
		{
			name:       "subtree pattern matches the directory itself",
			changed:    []string{"internal/scm"},
			rules:      []config.PathInstruction{scm},
			wantBlocks: []pathInstructionBlock{{Path: "internal/scm/**", Instructions: scm.Instructions, Files: []string{"internal/scm"}}},
		},
		{
			name:          "subtree pattern does not match a sibling prefix",
			changed:       []string{"internal/scmutil/util.go"},
			rules:         []config.PathInstruction{scm},
			wantUnmatched: []string{"internal/scm/**"},
		},
		{
			name:       "basename pattern matches at any depth",
			changed:    []string{"internal/db/schema.generated.go"},
			rules:      []config.PathInstruction{generated},
			wantBlocks: []pathInstructionBlock{{Path: "*.generated.go", Instructions: generated.Instructions, Files: []string{"internal/db/schema.generated.go"}}},
		},
		{
			name:       "full path pattern matches exactly",
			changed:    []string{"internal/config/config.go"},
			rules:      []config.PathInstruction{exact},
			wantBlocks: []pathInstructionBlock{{Path: "internal/config/config.go", Instructions: exact.Instructions, Files: []string{"internal/config/config.go"}}},
		},
		{
			name:    "blocks keep config order, not changed-file order",
			changed: []string{"docs/reference/repo-config.md", "internal/scm/github/github.go"},
			rules:   []config.PathInstruction{scm, docs},
			wantBlocks: []pathInstructionBlock{
				{Path: "internal/scm/**", Instructions: scm.Instructions, Files: []string{"internal/scm/github/github.go"}},
				{Path: "docs/**", Instructions: docs.Instructions, Files: []string{"docs/reference/repo-config.md"}},
			},
		},
		{
			name:    "the same text under two globs stays two scoped blocks",
			changed: []string{"internal/scm/github/github.go", "docs/index.md"},
			rules: []config.PathInstruction{
				{Path: "internal/scm/**", Instructions: "Name the owning invariant."},
				{Path: "docs/**", Instructions: "Name the owning invariant."},
			},
			wantBlocks: []pathInstructionBlock{
				{Path: "internal/scm/**", Instructions: "Name the owning invariant.", Files: []string{"internal/scm/github/github.go"}},
				{Path: "docs/**", Instructions: "Name the owning invariant.", Files: []string{"docs/index.md"}},
			},
		},
		{
			name:    "an exact duplicate entry is injected once",
			changed: []string{"internal/scm/github/github.go"},
			rules:   []config.PathInstruction{scm, scm},
			wantBlocks: []pathInstructionBlock{
				{Path: "internal/scm/**", Instructions: scm.Instructions, Files: []string{"internal/scm/github/github.go"}},
			},
			wantDuplicate: []string{"internal/scm/**"},
		},
		{
			name:          "no glob matches",
			changed:       []string{"README.md"},
			rules:         []config.PathInstruction{scm, docs},
			wantUnmatched: []string{"internal/scm/**", "docs/**"},
		},
		{
			name:    "no rules configured",
			changed: []string{"internal/scm/github/github.go"},
			rules:   nil,
		},
		{
			name:          "no changed files",
			changed:       nil,
			rules:         []config.PathInstruction{scm},
			wantUnmatched: []string{"internal/scm/**"},
		},
		{
			name:    "entries missing a path or instructions are recorded, not silently dropped",
			changed: []string{"internal/scm/github/github.go"},
			rules: []config.PathInstruction{
				{Path: "", Instructions: "orphaned rule"},
				{Path: "internal/scm/**", Instructions: "   "},
				scm,
			},
			wantBlocks:   []pathInstructionBlock{{Path: "internal/scm/**", Instructions: scm.Instructions, Files: []string{"internal/scm/github/github.go"}}},
			wantUnusable: []string{"(no path)", "internal/scm/**"},
		},
		{
			name:    "instruction whitespace is collapsed for the prompt",
			changed: []string{"internal/scm/github/github.go"},
			rules: []config.PathInstruction{
				{Path: "internal/scm/**", Instructions: "  redact   every   URL  \n\n  and log   nothing  "},
			},
			wantBlocks: []pathInstructionBlock{{
				Path:         "internal/scm/**",
				Instructions: "redact every URL\n\nand log nothing",
				Files:        []string{"internal/scm/github/github.go"},
			}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := matchPathInstructions(tt.changed, tt.rules)

			if len(got.Blocks) != len(tt.wantBlocks) {
				t.Fatalf("blocks = %+v, want %+v", got.Blocks, tt.wantBlocks)
			}
			for i, want := range tt.wantBlocks {
				if got.Blocks[i].Path != want.Path {
					t.Errorf("blocks[%d].Path = %q, want %q", i, got.Blocks[i].Path, want.Path)
				}
				if got.Blocks[i].Instructions != want.Instructions {
					t.Errorf("blocks[%d].Instructions = %q, want %q", i, got.Blocks[i].Instructions, want.Instructions)
				}
				if strings.Join(got.Blocks[i].Files, ",") != strings.Join(want.Files, ",") {
					t.Errorf("blocks[%d].Files = %q, want %q", i, got.Blocks[i].Files, want.Files)
				}
			}
			assertIDs(t, "unmatched", got.UnmatchedIDs, tt.wantUnmatched)
			assertIDs(t, "duplicate", got.DuplicateIDs, tt.wantDuplicate)
			assertIDs(t, "unusable", got.UnusableIDs, tt.wantUnusable)
		})
	}
}

func assertIDs(t *testing.T, label string, got, want []string) {
	t.Helper()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("%s = %q, want %q", label, got, want)
	}
}

// Selection of trusted rules must not depend on ignore_patterns, which comes
// from the pushed branch. Without this, a contributor suppresses a maintainer's
// rule from the review of their own branch by adding that rule's glob to their
// ignore list.
func TestMatchPathInstructions_PushedIgnorePatternsCannotSuppressTrustedRule(t *testing.T) {
	t.Parallel()

	rule := config.PathInstruction{Path: "internal/scm/**", Instructions: "Credential-carrying URLs must go through internal/safeurl."}
	changedFiles := "internal/scm/github/github.go\x00internal/cli/root.go\x00"
	contributorIgnores := []string{"internal/scm/**"}

	// What the review step feeds the matcher: the complete changed set.
	got := matchPathInstructions(changedPathList(changedFiles), []config.PathInstruction{rule})
	if len(got.Blocks) != 1 || got.Blocks[0].Path != rule.Path {
		t.Fatalf("blocks = %+v, want the trusted rule applied despite the pushed ignore list", got.Blocks)
	}
	if strings.Join(got.Blocks[0].Files, ",") != "internal/scm/github/github.go" {
		t.Fatalf("matched files = %q, want the ignored path still recorded as the rule's scope", got.Blocks[0].Files)
	}

	// Proof the vector is real if the filtered set is used instead.
	suppressed := matchPathInstructions(reviewablePaths(changedPathList(changedFiles), contributorIgnores), []config.PathInstruction{rule})
	if len(suppressed.Blocks) != 0 {
		t.Fatalf("the ignore-filtered set unexpectedly kept the rule; this test no longer proves the vector: %+v", suppressed.Blocks)
	}
}

func TestReviewablePaths(t *testing.T) {
	t.Parallel()

	got := reviewablePaths(
		changedPathList("internal/scm/github/github.go\x00vendor/dep/dep.go\x00schema.generated.go\x00docs/index.md\x00"),
		[]string{"vendor/**", "*.generated.go"})
	assertIDs(t, "reviewablePaths", got, []string{"internal/scm/github/github.go", "docs/index.md"})

	if got := reviewablePaths(changedPathList(""), nil); len(got) != 0 {
		t.Fatalf("reviewablePaths(nil) = %q, want none", got)
	}
	if got := reviewablePaths(changedPathList("vendor/dep/dep.go\x00"), []string{"vendor/**"}); len(got) != 0 {
		t.Fatalf("reviewablePaths() = %q, want none when everything is ignored", got)
	}
}

func TestChangedPathList(t *testing.T) {
	t.Parallel()
	assertIDs(t, "changedPathList",
		changedPathList("internal/scm/github/github.go\x00vendor/dep/dep.go\x00"),
		[]string{"internal/scm/github/github.go", "vendor/dep/dep.go"})
	if got := changedPathList(""); len(got) != 0 {
		t.Fatalf("changedPathList(\"\") = %q, want none", got)
	}
}

func TestChangedPathList_RenameAndUnusualNames(t *testing.T) {
	dir, _, _ := setupGitRepo(t)
	oldPath := dir + "/internal/scm/legacy.go"
	os.MkdirAll(dir+"/internal/scm", 0o755)
	os.WriteFile(oldPath, []byte("package scm\n"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "base")
	base := gitCmd(t, dir, "rev-parse", "HEAD")
	os.MkdirAll(dir+"/docs", 0o755)
	os.Rename(oldPath, dir+"/docs/legacy.go")
	oddPath := "internal/scm/über\n.go"
	os.WriteFile(dir+"/"+oddPath, []byte("package scm\n"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "rename and unicode")
	changed := changedPathList(gitCmd(t, dir, "diff", "--name-only", "-z", "--no-renames", base+"..HEAD"))
	matches := matchPathInstructions(changed, []config.PathInstruction{{Path: "internal/scm/**", Instructions: "keep"}})
	assertIDs(t, "matched paths", matches.Blocks[0].Files, []string{"internal/scm/legacy.go", oddPath})
	if got := matchedFilesSummary(matches.Blocks[0].Files); got != `internal/scm/legacy.go, "internal/scm/über\n.go"` {
		t.Fatalf("matched files summary = %q", got)
	}
}

// TestReviewPathInstructionsSection_EmptyWithoutMatches pins the no-op
// guarantee: with nothing configured, or nothing matching, the section is the
// empty string exactly, so the prompt gains no heading, no blank line, and no
// trailing whitespace.
func TestReviewPathInstructionsSection_EmptyWithoutMatches(t *testing.T) {
	t.Parallel()

	rules := []config.PathInstruction{{Path: "docs/**", Instructions: "Prose changes only."}}
	for _, tt := range []struct {
		name    string
		changed []string
		rules   []config.PathInstruction
	}{
		{name: "nothing configured", changed: []string{"docs/index.md"}, rules: nil},
		{name: "nothing matching", changed: []string{"internal/config/config.go"}, rules: rules},
		{name: "no changed files", changed: nil, rules: rules},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := sectionFor(tt.changed, tt.rules)
			if got != "" {
				t.Fatalf("section = %q, want the empty string exactly (len %d)", got, len(got))
			}
		})
	}

	// Control: the same rule set renders a full section once a path matches, so
	// the empty results above are caused by nothing matching rather than by a
	// renderer that produces nothing at all.
	control := sectionFor([]string{"docs/index.md"}, rules)
	if want := wantSection(wantBlock("docs/**", "docs/index.md", "Prose changes only.")); control != want {
		t.Fatalf("control section =\n%q\nwant\n%q", control, want)
	}
}

// Every injected block must name the glob it came from and the files it matched,
// so a rule scoped to one directory can never read as a repository-wide
// instruction in a mixed diff.
func TestReviewPathInstructionsSection_LabelsEveryBlockWithItsScope(t *testing.T) {
	t.Parallel()

	rules := []config.PathInstruction{
		{Path: "internal/**", Instructions: "Credential-carrying URLs must go through internal/safeurl."},
		{Path: "docs/**", Instructions: "Prose changes only. Do not request test coverage."},
	}
	changed := []string{"internal/pipeline/steps/push.go", "docs/notes.md"}

	got := sectionFor(changed, rules)
	want := wantSection(
		wantBlock("internal/**", "internal/pipeline/steps/push.go", rules[0].Instructions),
		wantBlock("docs/**", "docs/notes.md", rules[1].Instructions),
	)
	if got != want {
		t.Fatalf("section =\n%q\nwant\n%q", got, want)
	}

	// The mixed-diff hazard: the lax docs rule must never appear unqualified.
	if strings.Contains(got, "\n"+rules[1].Instructions) && !strings.Contains(got, config.ReviewPathInstructionsPathLabel+"docs/**") {
		t.Fatal("the docs rule appears without its scope label")
	}
	for _, block := range []string{rules[0].Instructions, rules[1].Instructions} {
		idx := strings.Index(got, block)
		if idx < 0 {
			t.Fatalf("block %q missing from the section", block)
		}
		if !strings.HasSuffix(got[:idx], config.ReviewPathInstructionsRulesLabel+"\n") {
			t.Fatalf("block %q is not preceded by its %q label", block, config.ReviewPathInstructionsRulesLabel)
		}
	}
}

func TestReviewPathInstructionsSection_SingleMatchRendersExactly(t *testing.T) {
	t.Parallel()

	rules := []config.PathInstruction{
		{Path: "internal/scm/**", Instructions: "Credential-carrying URLs must go through internal/safeurl."},
		{Path: "docs/**", Instructions: "Prose changes only. Do not request test coverage."},
	}
	got := sectionFor([]string{"internal/scm/github/github.go", "internal/cli/root.go"}, rules)
	want := wantSection(wantBlock("internal/scm/**", "internal/scm/github/github.go", rules[0].Instructions))
	if got != want {
		t.Fatalf("section =\n%q\nwant\n%q", got, want)
	}
}

// A broad glob must not push the section past the size the config was validated
// against, so the file list truncates deterministically and says how many files
// it left out.
func TestMatchedFilesSummary_TruncatesWithinAllowance(t *testing.T) {
	t.Parallel()

	if got := matchedFilesSummary([]string{"a.go", "b.go"}); got != "a.go, b.go" {
		t.Fatalf("summary = %q, want the full list", got)
	}

	var files []string
	for i := 0; i < 200; i++ {
		files = append(files, fmt.Sprintf("internal/pipeline/steps/generated_file_%03d.go", i))
	}
	got := matchedFilesSummary(files)
	if len(got) > config.ReviewPathInstructionsMaxFilesBytes {
		t.Fatalf("summary is %d bytes, over the %d allowance: %q", len(got), config.ReviewPathInstructionsMaxFilesBytes, got)
	}
	if !strings.HasPrefix(got, files[0]) {
		t.Errorf("summary = %q, want it to start with the first matched file", got)
	}
	if !strings.Contains(got, " more") {
		t.Errorf("summary = %q, want a remaining-count suffix", got)
	}
	// Deterministic: the same input renders the same summary.
	if again := matchedFilesSummary(files); again != got {
		t.Errorf("summary is not deterministic: %q then %q", got, again)
	}

	// A single file longer than the whole allowance still renders a bounded value.
	long := strings.Repeat("x", config.ReviewPathInstructionsMaxFilesBytes*2)
	if got := matchedFilesSummary([]string{long, "b.go"}); len(got) > config.ReviewPathInstructionsMaxFilesBytes {
		t.Fatalf("summary is %d bytes, over the allowance", len(got))
	}
}

// The config-time byte cap has to measure the real assembled section, otherwise
// it does not bound what is injected. This is the drift check between the
// accounting in internal/config and this renderer.
func TestReviewPathInstructionsSectionStaysWithinAccountedBytes(t *testing.T) {
	t.Parallel()

	var entries []config.PathInstruction
	var changed []string
	for i := 0; i < config.MaxReviewPathInstructions; i++ {
		entries = append(entries, config.PathInstruction{
			Path:         fmt.Sprintf("internal/package_number_%02d/**", i),
			Instructions: fmt.Sprintf("Rule %02d: %s", i, strings.Repeat("guidance ", 12)),
		})
		for f := 0; f < 40; f++ {
			changed = append(changed, fmt.Sprintf("internal/package_number_%02d/some/deeply/nested/file_%02d.go", i, f))
		}
	}

	matches := matchPathInstructions(changed, entries)
	if len(matches.Blocks) != len(entries) {
		t.Fatalf("blocks = %d, want every entry to match", len(matches.Blocks))
	}
	section := reviewPathInstructionsSection(matches)
	accounted := config.ReviewPathInstructionsBytes(entries)
	if len(section) > accounted {
		t.Fatalf("section is %d bytes but the config accounting allowed %d; the cap no longer bounds the prompt", len(section), accounted)
	}

	// A single entry with a single short file is the tight case: the accounting
	// may only exceed the real section by the unused matched-file allowance.
	one := []config.PathInstruction{{Path: "a/**", Instructions: "check it"}}
	oneSection := reviewPathInstructionsSection(matchPathInstructions([]string{"a/b.go"}, one))
	slack := config.ReviewPathInstructionsBytes(one) - len(oneSection)
	if slack < 0 || slack > config.ReviewPathInstructionsMaxFilesBytes {
		t.Fatalf("accounting slack = %d, want between 0 and the %d file allowance", slack, config.ReviewPathInstructionsMaxFilesBytes)
	}
}

// Instruction text config accepts must survive prompt rendering, and text config
// rejects must be exactly the text that would render empty. This pins the
// agreement between validation in internal/config and the sanitizer this package
// applies, which is what makes an empty-rendering rule fail closed at parse time
// rather than vanish from the prompt.
func TestPathInstructionRenderingAgreesWithConfigValidation(t *testing.T) {
	t.Parallel()

	nonEmpty := []string{
		"Credential-carrying URLs must go through internal/safeurl.",
		"  spaced   out  ",
		"line one\nline two",
		"marker <<<<<<< inside real prose",
	}
	for _, raw := range nonEmpty {
		if config.RenderedInstructions(raw) == "" {
			t.Fatalf("config would reject %q, so it must not render as usable text", raw)
		}
		if sanitizePromptMultilineText(raw) == "" {
			t.Errorf("config accepts %q but it renders empty in the prompt", raw)
		}
	}

	empty := []string{"=======", "<<<<<<<", ">>>>>>>", " <<<<<<<  ======= ", "   "}
	for _, raw := range empty {
		if config.RenderedInstructions(raw) != "" {
			t.Fatalf("test input %q is not an empty-rendering value", raw)
		}
		if sanitizePromptMultilineText(raw) != "" {
			t.Errorf("config rejects %q but the prompt renderer keeps %q", raw, sanitizePromptMultilineText(raw))
		}
	}

	// The second invariant config.RenderedInstructions documents: the prompt
	// renderer never lengthens text. config.ReviewPathInstructionsBytes charges
	// the raw trimmed instructions, so a renderer that could grow its input
	// (escaping, wrapping) would turn the validated byte cap into an
	// underestimate and let an over-budget section reach the review prompt.
	grow := append(append([]string{}, nonEmpty...), empty...)
	grow = append(grow,
		"carriage\r\nreturn\rline",
		"<<<<<<< HEAD\nours\n=======\ntheirs\n>>>>>>> branch",
		"tabs\tand    wide   gaps\n\n\n   trailing   ",
		"unicode ✓ stays ✓ intact",
	)
	for _, raw := range grow {
		if got, limit := len(sanitizePromptMultilineText(raw)), len(strings.TrimSpace(raw)); got > limit {
			t.Errorf("sanitizePromptMultilineText(%q) is %d bytes, longer than the %d bytes config.ReviewPathInstructionsBytes accounts for", raw, got, limit)
		}
	}
}

func TestLogPathInstructions(t *testing.T) {
	t.Parallel()

	var logged []string
	matches := pathInstructionMatches{
		Blocks: []pathInstructionBlock{
			{Path: "internal/scm/**", Instructions: "x", Files: []string{"internal/scm/a.go", "internal/scm/b.go"}},
		},
		UnmatchedIDs: []string{"docs/**"},
		DuplicateIDs: []string{"internal/scm/**"},
		UnusableIDs:  []string{"(no path)"},
	}
	logPathInstructions(func(s string) { logged = append(logged, s) }, matches)

	for _, want := range []string{
		"applied 1 trusted review instruction block(s) for changed paths: internal/scm/** (2 file(s))",
		"1 trusted review instruction rule(s) matched no changed path: docs/**",
		"skipped 1 duplicate trusted review instruction rule(s): internal/scm/**",
		"skipped 1 trusted review instruction rule(s) with no usable path or instructions: (no path)",
	} {
		found := false
		for _, line := range logged {
			if line == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing log line %q; got %q", want, logged)
		}
	}

	// Nothing configured logs nothing, so a quiet step log means no rules.
	var quiet []string
	logPathInstructions(func(s string) { quiet = append(quiet, s) }, pathInstructionMatches{})
	if len(quiet) != 0 {
		t.Errorf("expected no log lines with no rules, got %q", quiet)
	}
}

// TestEvidence_ReviewPathInstructionsMatchedPrompt records the assembled review
// prompt section for a mixed diff: one glob matches, a second matches a
// different file, and a third matches nothing. Every block names its own scope.
func TestEvidence_ReviewPathInstructionsMatchedPrompt(t *testing.T) {
	rules := []config.PathInstruction{
		{Path: "internal/scm/**", Instructions: "Any URL or error string that can carry credentials must go through internal/safeurl."},
		{Path: "docs/**", Instructions: "Prose changes only. Do not request test coverage."},
		{Path: "cmd/**", Instructions: "Flag changes need a docs update in the same change."},
	}
	changed := changedPathList("internal/scm/github/github.go\x00internal/scm/github/github_test.go\x00docs/notes.md\x00")

	matches := matchPathInstructions(changed, rules)
	section := reviewPathInstructionsSection(matches)

	t.Logf("configured globs: %q, %q, %q", rules[0].Path, rules[1].Path, rules[2].Path)
	t.Logf("changed paths: %q", changed)
	t.Logf("rules that matched nothing: %q", matches.UnmatchedIDs)
	t.Logf("appended review prompt section:%s", section)
	t.Logf("section with no configured rules: %q", reviewPathInstructionsSection(matchPathInstructions(changed, nil)))

	want := wantSection(
		wantBlock("internal/scm/**", "internal/scm/github/github.go, internal/scm/github/github_test.go", rules[0].Instructions),
		wantBlock("docs/**", "docs/notes.md", rules[1].Instructions),
	)
	if section != want {
		t.Fatalf("section =\n%q\nwant\n%q", section, want)
	}
	assertIDs(t, "unmatched", matches.UnmatchedIDs, []string{"cmd/**"})
	if reviewPathInstructionsSection(matchPathInstructions(changed, nil)) != "" {
		t.Fatal("an unconfigured repository must get no section at all")
	}
}
