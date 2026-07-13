package steps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestCollectBasecampReferences(t *testing.T) {
	t.Parallel()

	const (
		canonical100 = "https://app.basecamp.com/3594299/buckets/123456/card_tables/cards/10077092945"
		legacy100    = "https://3.basecamp.com/3594299/buckets/123456/card_tables/cards/10077092945"
		legacy200    = "https://3.basecamp.com/3594299/buckets/123456/card_tables/cards/20077092945"
	)

	tests := []struct {
		name      string
		intent    string
		commitLog string
		wantIDs   []string
		wantURLs  []string
	}{
		{
			name:     "canonical app URL from intent",
			intent:   "Ship the card: " + canonical100,
			wantIDs:  []string{"10077092945"},
			wantURLs: []string{canonical100},
		},
		{
			name:      "legacy URL from commit metadata",
			commitLog: "abc123 fix(pr): preserve Basecamp link\n\n" + legacy100,
			wantIDs:   []string{"10077092945"},
			wantURLs:  []string{legacy100},
		},
		{
			name:     "BC hash bare ID",
			intent:   "Track BC#10077092945 in the PR.",
			wantIDs:  []string{"10077092945"},
			wantURLs: []string{""},
		},
		{
			name:      "Basecamp card phrase bare ID",
			commitLog: "abc123 Basecamp card 10077092945",
			wantIDs:   []string{"10077092945"},
			wantURLs:  []string{""},
		},
		{
			name: "malformed and non-card URLs are ignored",
			intent: "http://app.basecamp.com/3594299/buckets/123456/card_tables/cards/10077092945 " +
				"https://example.com/3594299/buckets/123456/card_tables/cards/20077092945 " +
				"https://app.basecamp.com/3594299/buckets/123456/card_tables/cards/not-a-card " +
				"https://app.basecamp.com/3594299/buckets/123456/todos/30077092945 " +
				"https://app.basecamp.com/3594299/buckets/123456/card_tables/cards/40077092945/extra",
			wantIDs:  []string{},
			wantURLs: []string{},
		},
		{
			name:   "multiple cards keep first-seen order across intent and commits",
			intent: "BC#30077092945 then " + canonical100 + " then Basecamp card 20077092945",
			commitLog: "def456 " +
				"https://app.basecamp.com/3594299/buckets/123456/card_tables/cards/40077092945",
			wantIDs:  []string{"30077092945", "10077092945", "20077092945", "40077092945"},
			wantURLs: []string{"", canonical100, "", "https://app.basecamp.com/3594299/buckets/123456/card_tables/cards/40077092945"},
		},
		{
			name:      "intent URL wins over legacy commit URL and duplicates",
			intent:    canonical100,
			commitLog: "def456 " + legacy100 + " BC#10077092945",
			wantIDs:   []string{"10077092945"},
			wantURLs:  []string{canonical100},
		},
		{
			name:      "commit URL upgrades an intent bare ID without moving it",
			intent:    "BC#20077092945 then Basecamp card 30077092945",
			commitLog: "def456 " + legacy200,
			wantIDs:   []string{"20077092945", "30077092945"},
			wantURLs:  []string{legacy200, ""},
		},
		{
			name:      "no reference",
			intent:    "Improve PR body generation.",
			commitLog: "abc123 feat(pr): improve generated sections",
			wantIDs:   []string{},
			wantURLs:  []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := collectBasecampReferences(tt.intent, tt.commitLog)
			gotIDs := make([]string, 0, len(got))
			gotURLs := make([]string, 0, len(got))
			for _, ref := range got {
				gotIDs = append(gotIDs, ref.CardID)
				gotURLs = append(gotURLs, ref.URL)
			}
			if !reflect.DeepEqual(gotIDs, tt.wantIDs) {
				t.Fatalf("card IDs = %#v, want %#v", gotIDs, tt.wantIDs)
			}
			if !reflect.DeepEqual(gotURLs, tt.wantURLs) {
				t.Fatalf("URLs = %#v, want %#v", gotURLs, tt.wantURLs)
			}
		})
	}
}

func TestRenderBasecampSection(t *testing.T) {
	t.Parallel()

	refs := []basecampReference{
		{CardID: "10077092945", URL: "https://app.basecamp.com/3594299/buckets/123456/card_tables/cards/10077092945"},
		{CardID: "20077092945"},
	}
	want := "## Basecamp\n\n" +
		"- [Basecamp card 10077092945](https://app.basecamp.com/3594299/buckets/123456/card_tables/cards/10077092945)\n" +
		"- Basecamp card 20077092945 — canonical URL not provided"

	if got := renderBasecampSection(refs); got != want {
		t.Fatalf("section =\n%s\nwant =\n%s", got, want)
	}
	if got := renderBasecampSection(nil); got != "" {
		t.Fatalf("empty section = %q, want empty", got)
	}
}

func TestBasecampWarningFindingsJSON(t *testing.T) {
	t.Parallel()

	if got := basecampWarningFindingsJSON([]basecampReference{{
		CardID: "10077092945",
		URL:    "https://app.basecamp.com/3594299/buckets/123456/card_tables/cards/10077092945",
	}}); got != "" {
		t.Fatalf("linked reference findings = %s, want empty", got)
	}

	raw := basecampWarningFindingsJSON([]basecampReference{
		{CardID: "10077092945", URL: "https://app.basecamp.com/3594299/buckets/123456/card_tables/cards/10077092945"},
		{CardID: "20077092945"},
	})
	findings, err := types.ParseFindingsJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings.Items) != 1 {
		t.Fatalf("findings = %#v, want one bare-ID warning", findings.Items)
	}
	got := findings.Items[0]
	if got.Severity != "warning" || got.Action != types.ActionNoOp {
		t.Fatalf("finding severity/action = %s/%s, want warning/no-op", got.Severity, got.Action)
	}
	if !strings.Contains(got.Description, "20077092945") || !strings.Contains(got.Description, "no canonical URL") {
		t.Fatalf("finding description = %q", got.Description)
	}
}

func TestPRStepBuildPRContentBasecampSection(t *testing.T) {
	t.Parallel()

	const canonicalURL = "https://app.basecamp.com/3594299/buckets/123456/card_tables/cards/10077092945"
	tests := []struct {
		name             string
		intent           string
		agentFailure     bool
		wantEntry        string
		wantWarningCount int
	}{
		{
			name:      "agent success links canonical URL",
			intent:    "Implement " + canonicalURL,
			wantEntry: "[Basecamp card 10077092945](" + canonicalURL + ")",
		},
		{
			name:             "fallback renders bare ID and warning",
			intent:           "Implement BC#20077092945",
			agentFailure:     true,
			wantEntry:        "Basecamp card 20077092945 — canonical URL not provided",
			wantWarningCount: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir, baseSHA, headSHA := setupGitRepo(t)
			ag := &mockAgent{
				name: "test",
				runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
					if tt.agentFailure {
						return nil, errors.New("simulated agent failure")
					}
					payload := json.RawMessage(`{"title":"fix(pr): add durable Basecamp references","body":"## What Changed\n\n- preserve card context\n\n## Basecamp\n\n- stale agent section"}`)
					return &agent.Result{Output: payload}, nil
				},
			}
			sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
			sctx.UserIntent = tt.intent

			content, refs, err := (&PRStep{}).buildPRContent(sctx, nil, "feature", baseSHA, 0)
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{"## Intent", "## Basecamp", tt.wantEntry, "## What Changed"} {
				if !strings.Contains(content.Body, want) {
					t.Fatalf("body missing %q:\n%s", want, content.Body)
				}
			}
			if strings.Count(content.Body, "## Basecamp") != 1 || strings.Contains(content.Body, "stale agent section") {
				t.Fatalf("Basecamp section was not regenerated deterministically:\n%s", content.Body)
			}
			if intentIndex, basecampIndex, changedIndex := strings.Index(content.Body, "## Intent"), strings.Index(content.Body, "## Basecamp"), strings.Index(content.Body, "## What Changed"); !(intentIndex < basecampIndex && basecampIndex < changedIndex) {
				t.Fatalf("section order = Intent:%d Basecamp:%d WhatChanged:%d\n%s", intentIndex, basecampIndex, changedIndex, content.Body)
			}

			warningJSON := basecampWarningFindingsJSON(refs)
			if tt.wantWarningCount == 0 {
				if warningJSON != "" {
					t.Fatalf("warning findings = %s, want empty", warningJSON)
				}
				return
			}
			findings, err := types.ParseFindingsJSON(warningJSON)
			if err != nil {
				t.Fatal(err)
			}
			if len(findings.Items) != tt.wantWarningCount {
				t.Fatalf("warning count = %d, want %d", len(findings.Items), tt.wantWarningCount)
			}
		})
	}
}

func TestPRStepExistingPRUpdateReportsBareBasecampID(t *testing.T) {
	t.Parallel()

	dir, baseSHA, headSHA := setupGitRepo(t)
	env, logFile := fakeGH(t, "https://github.com/test/repo/pull/42")
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.UserIntent = "Keep BC#10077092945 attached to this update."

	outcome, err := (&PRStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Fatal("bare Basecamp ID warning must not park the PR step")
	}
	findings, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings.Items) != 1 || findings.Items[0].Action != types.ActionNoOp {
		t.Fatalf("findings = %#v, want one no-op warning", findings.Items)
	}

	body := readFakeGHBodyArg(t, logFile)
	if strings.Count(body, "## Basecamp") != 1 || !strings.Contains(body, "Basecamp card 10077092945 — canonical URL not provided") {
		t.Fatalf("updated PR body missing deterministic unlinked Basecamp section:\n%s", body)
	}
	if data, err := os.ReadFile(logFile); err != nil || !strings.Contains(string(data), "pr edit") {
		t.Fatalf("expected existing PR update, log=%s err=%v", data, err)
	}
}

func TestPRStepBuildPRContentReadsBasecampURLFromCommitBody(t *testing.T) {
	t.Parallel()

	const legacyURL = "https://3.basecamp.com/3594299/buckets/123456/card_tables/cards/10077092945"
	dir, baseSHA, _ := setupGitRepo(t)
	gitCmd(t, dir, "commit", "--amend", "-m", "feat(pr): preserve external context", "-m", "Basecamp: "+legacyURL)
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})

	content, refs, err := (&PRStep{}).buildPRContent(sctx, nil, "feature", baseSHA, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 || refs[0].CardID != "10077092945" || refs[0].URL != legacyURL {
		t.Fatalf("commit-body refs = %#v", refs)
	}
	if !strings.Contains(content.Body, "[Basecamp card 10077092945]("+legacyURL+")") {
		t.Fatalf("PR body missing commit-body Basecamp URL:\n%s", content.Body)
	}
}

func TestBuildPRBodyPreservesBasecampUnderBodyLimit(t *testing.T) {
	t.Parallel()

	const canonicalURL = "https://app.basecamp.com/3594299/buckets/123456/card_tables/cards/10077092945"
	basecampMD := renderBasecampSection([]basecampReference{{CardID: "10077092945", URL: canonicalURL}})
	testingMD := "## Testing\n\n```text\n" + strings.Repeat("large test log\n", 7000) + "```"
	rounds := make([]string, 0, 140)
	for i := 1; i <= 140; i++ {
		rounds = append(rounds, fmt.Sprintf("%s review round %03d", strings.Repeat("x", 700), i))
	}
	got := buildPRBody(
		"## What Changed\n\n- preserve durable Basecamp context",
		basecampMD,
		"✅ Low: PR metadata only",
		testingMD,
		pipelineMarkdownForTest(rounds...),
		&pipeline.StepContext{UserIntent: "Keep the Basecamp card visible under body limits."},
	)

	assertGitHubBodyLimitForTest(t, got)
	if strings.Count(got, "## Basecamp") != 1 || !strings.Contains(got, canonicalURL) {
		t.Fatalf("Basecamp section was lost under body limit:\n%s", got)
	}
	if !strings.Contains(got, "earlier update rounds omitted") {
		t.Fatalf("expected older Pipeline content to be omitted before Basecamp:\n%s", got)
	}
	testingIndex := strings.Index(got, "## Testing")
	if testingIndex >= 0 && strings.Index(got, "## Basecamp") > testingIndex {
		t.Fatalf("Basecamp section must remain ahead of Testing:\n%s", got)
	}
}
