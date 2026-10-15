package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/testartifact"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func testPayloadWithArtifacts(artifacts string) string {
	return `{"findings":[],"summary":"","tested":["npm run e2e -- checkout"],"testing_summary":"drove checkout end to end",` +
		`"artifacts":` + artifacts + `,` +
		`"scenarios":[{"name":"user reaches the success screen","result":"pass","live":true,"evidence":"checkout.png","reason":""}],"verdict":"go"}`
}

func TestUnmarshalRequiredTestFindings_ArtifactContract(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		artifacts string
		wantErr   string
	}{
		{name: "no artifacts", artifacts: `[]`},
		{name: "declared media, log, and quote", artifacts: `[{"kind":"image","shows":"ui","label":"Checkout","path":"/evidence/checkout.png"},{"kind":"log","label":"Server log","path":"/evidence/server.log"},{"kind":"text-quote","label":"Heading","content":"## Done"}]`},
		{name: "older agent log without a posture", artifacts: `[{"kind":"log","label":"Server log","path":"/evidence/server.log","content":"booted"}]`},
		{name: "unknown kind", artifacts: `[{"kind":"other","label":"Thing","path":"/evidence/x"}]`, wantErr: `artifact 1: kind "other" is not one of log, text-quote, image, video`},
		{name: "missing kind", artifacts: `[{"label":"Thing","path":"/evidence/x"}]`, wantErr: "artifact 1: missing kind"},
		{name: "undeclared image", artifacts: `[{"kind":"screenshot","label":"Shot","path":"/evidence/a.png"}]`, wantErr: "artifact 1: image missing shows"},
		{name: "malformed shows", artifacts: `[{"kind":"video","shows":"everything","label":"Rec","path":"/evidence/a.mp4"}]`, wantErr: `shows "everything" is not one of ui, chart, terminal`},
		{name: "quote with an endpoint", artifacts: `[{"kind":"text-quote","label":"Boot","content":"Server ready at http://localhost:43210"}]`, wantErr: "value-shaped text (endpoint)"},
		{name: "missing label is still checked first", artifacts: `[{"kind":"log","path":"/evidence/x.log"}]`, wantErr: "artifact 0 missing label"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var findings Findings
			err := unmarshalRequiredTestFindings([]byte(testPayloadWithArtifacts(tt.artifacts)), &findings)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unmarshalRequiredTestFindings() = %v, want accepted", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("unmarshalRequiredTestFindings() = %v, want an error containing %q", err, tt.wantErr)
			}
		})
	}
}

// An artifact that breaks the contract goes back to the analyzer as a
// correction like any other contract violation, and what the step records is
// the contract's canonical form: an older agent's log keeps only its path.
func TestTestStep_ArtifactContractViolationTriggersCorrectionRound(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	invalid := testPayloadWithArtifacts(`[{"kind":"other","label":"Thing","path":"/evidence/x"}]`)
	corrected := testPayloadWithArtifacts(`[{"kind":"LOG","label":"Server log","path":"/evidence/server.log","url":"https://example.com/log","content":"bootstrap password: synthetic"},{"kind":"screenshot","shows":"UI","label":"Checkout","path":"/evidence/checkout.png"}]`)
	calls := 0
	ag := &mockAgent{
		name: "test",
		runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
			calls++
			if calls == 1 {
				return &agent.Result{Output: json.RawMessage(invalid)}, nil
			}
			return &agent.Result{Output: json.RawMessage(corrected)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("an artifact contract violation must be returned to the analyzer, not fail the step: %v", err)
	}
	if len(ag.calls) != 2 {
		t.Fatalf("agent calls = %d, want 1 rejected payload plus 1 correction", len(ag.calls))
	}
	if want := `artifact 1: kind "other" is not one of log, text-quote, image, video`; !strings.Contains(ag.calls[1].Prompt, want) {
		t.Fatalf("correction prompt missing %q:\n%s", want, ag.calls[1].Prompt)
	}
	findings, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatal(err)
	}
	want := []types.TestArtifact{
		{Kind: testartifact.KindLog, Label: "Server log", Path: "/evidence/server.log"},
		{Kind: testartifact.KindImage, Shows: testartifact.ShowsUI, Label: "Checkout", Path: "/evidence/checkout.png"},
	}
	if len(findings.Artifacts) != len(want) {
		t.Fatalf("recorded artifacts = %+v, want %+v", findings.Artifacts, want)
	}
	for i := range want {
		if findings.Artifacts[i] != want[i] {
			t.Fatalf("recorded artifact %d = %+v, want %+v", i, findings.Artifacts[i], want[i])
		}
	}
	if strings.Contains(outcome.Findings, "bootstrap password") {
		t.Fatalf("recorded findings kept a log's inline content: %s", outcome.Findings)
	}
}

func TestTestFindingsSchema_DeclaresTheArtifactContract(t *testing.T) {
	t.Parallel()
	var schema struct {
		Properties struct {
			Artifacts struct {
				Items struct {
					Properties map[string]struct {
						Enum        []string `json:"enum"`
						Description string   `json:"description"`
					} `json:"properties"`
					Required []string `json:"required"`
				} `json:"items"`
			} `json:"artifacts"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(testFindingsSchema, &schema); err != nil {
		t.Fatal(err)
	}
	items := schema.Properties.Artifacts.Items
	if got := strings.Join(items.Properties["kind"].Enum, ","); got != "log,text-quote,image,video" {
		t.Fatalf("artifact kind enum = %q", got)
	}
	if got := strings.Join(items.Properties["shows"].Enum, ","); got != "ui,chart,terminal" {
		t.Fatalf("artifact shows enum = %q", got)
	}
	if got := strings.Join(items.Required, ","); got != "kind,label" {
		t.Fatalf("artifact required = %q, want kind,label", got)
	}
	for _, want := range []string{"not shown in the pull request", "line and byte summary"} {
		if !strings.Contains(items.Properties["kind"].Description, want) {
			t.Fatalf("kind description %q missing %q", items.Properties["kind"].Description, want)
		}
	}
	for _, want := range []string{"text-quote", "closing references", "code fences", "attestation", "appendix"} {
		if !strings.Contains(items.Properties["content"].Description, want) {
			t.Fatalf("content description %q missing %q", items.Properties["content"].Description, want)
		}
	}
}

func TestTestStep_PromptDescribesTypedArtifacts(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{
		name: "test",
		runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(passingScenarioFindingsJSON)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	if _, err := (&TestStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	prompt := ag.calls[0].Prompt
	for _, want := range []string{
		`kind "log"`,
		"not shown in the pull request",
		"store_in_repo may still publish the whole evidence directory",
		`kind "text-quote"`,
		"closing reference",
		"code fence",
		"attestation",
		"appendix",
		`"shows"`,
		"terminal",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("evidence prompt missing %q", want)
		}
	}
	if strings.Contains(prompt, "artifact content for short logs or command output that should be shown directly in the PR") {
		t.Fatal("evidence prompt still solicits inline logs")
	}
}

// The whole PR body, not just the Testing section, carries none of a log's
// bytes: the summary is the only thing the PR step learns about the file.
func TestPRStep_LogBytesNeverReachThePRBody(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, prDraftAgent(), dir, baseSHA, headSHA, config.Commands{})
	enableDefaultEvidence(sctx)
	logPath := writeEvidenceFile(t, sctx.EvidenceDir, "server.log", []byte(syntheticServerLog))
	findings := fmt.Sprintf(`{"findings":[],"summary":"","testing_summary":"Booted the server and drove checkout.","artifacts":[{"kind":"log","label":"Server boot","path":%q,"content":%q}]}`, logPath, syntheticServerLog)
	insertCompletedStep(t, sctx, types.StepTest, findings, "")

	for _, provider := range []scm.Provider{scm.ProviderGitHub, scm.ProviderBitbucket, scm.ProviderGitLab} {
		content, err := (&PRStep{mediaUploader: &stubMediaUploader{t: t}}).buildPRContent(sctx, "feature", "main", baseSHA, provider, 0)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(content.Body, "- Evidence: Log 1 (not shown in this PR): 7 lines, ") {
			t.Fatalf("%s: expected the log summary, got:\n%s", provider, content.Body)
		}
		for _, leak := range append(syntheticServerLogValues, "server.log", "Server boot") {
			if strings.Contains(content.Title, leak) || strings.Contains(content.Body, leak) {
				t.Fatalf("%s: PR content carries %q:\n%s", provider, leak, content.Body)
			}
		}
	}
}

// A golden of the Testing section for one artifact of every posture, so a
// change to what any posture publishes is a visible diff here.
func TestBuildTestingSummaryForPR_TypedArtifactsGolden(t *testing.T) {
	t.Parallel()
	evidenceRoot := filepath.Join(t.TempDir(), "evidence", "run-123")
	logPath := writeTempEvidenceFile(t, evidenceRoot, "server.log", []byte(syntheticServerLog))
	shot := writeTempEvidenceFile(t, evidenceRoot, "checkout.png", []byte("png"))
	boot := writeTempEvidenceFile(t, evidenceRoot, "boot.png", []byte("png"))
	raw := fmt.Sprintf(`{"findings":[],"summary":"","testing_summary":"Drove checkout live.","artifacts":[`+
		`{"kind":"log","label":"Server boot","path":%q},`+
		`{"kind":"text-quote","label":"Receipt heading","content":"## Receipt\nTotal: 3 items"},`+
		`{"kind":"image","shows":"ui","label":"Checkout success","path":%q},`+
		`{"kind":"image","shows":"terminal","label":"Boot terminal","path":%q}]}`, logPath, shot, boot)
	attachments := map[string]string{shot: testAttachmentURL}
	steps := []*db.StepResult{{ID: "s1", StepName: types.StepTest, Status: types.StepStatusCompleted, FindingsJSON: &raw}}
	rounds := map[string][]*db.StepRound{"s1": {{Round: 1, Trigger: "initial", FindingsJSON: &raw, DurationMS: 300}}}

	md := buildPRTestingSummary(steps, rounds, "git@github.com:example/widgets.git", "abc123", t.TempDir(), evidenceRoot, nil, scm.ProviderGitHub, attachments)

	want := "## Testing\n\n" +
		"Drove checkout live.\n\n" +
		fmt.Sprintf("- Evidence: Log 1 (not shown in this PR): 7 lines, %d bytes, complete; phases: ready 2, error 0, warning 0, shutdown 1\n\n", len(syntheticServerLog)) +
		"<details>\n<summary>Evidence: Receipt heading</summary>\n\n```text\n## Receipt\nTotal: 3 items\n```\n</details>\n\n" +
		"![Checkout success](" + testAttachmentURL + ")\n\n" +
		"- Evidence: Image 2 (not shown in this PR: terminal capture)"
	if md != want {
		t.Fatalf("Testing section drifted.\n--- got ---\n%s\n--- want ---\n%s", md, want)
	}
}

// A quote is published as recorded: the prose cleanup applied to other agent
// text must not change what the quoted evidence says.
func TestBuildTestingSummaryForPR_QuoteIsPublishedVerbatim(t *testing.T) {
	t.Parallel()
	quote := "{\"message\":\"Hello   world\"}\n\tindented"
	raw := fmt.Sprintf(`{"findings":[],"summary":"","testing_summary":"Evidence was collected.","artifacts":[{"kind":"text-quote","label":"Response","content":%q}]}`, quote)

	md := testingSummaryFor(t, raw, t.TempDir(), "", scm.ProviderGitHub)

	if !strings.Contains(md, "```text\n"+quote+"\n```") {
		t.Fatalf("expected the quote verbatim, got:\n%s", md)
	}
}

// The media uploader only needs media, so collecting its candidates reads no
// log file; the Testing section's own collection is the one log scan.
func TestCollectPRTestingArtifacts_ReadsNoLogs(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, prDraftAgent(), dir, baseSHA, headSHA, config.Commands{})
	logPath := writeEvidenceFile(t, sctx.EvidenceDir, "server.log", []byte(syntheticServerLog))
	shot := writeEvidenceFile(t, sctx.EvidenceDir, "checkout.png", []byte("png"))
	findings := fmt.Sprintf(`{"findings":[],"summary":"","testing_summary":"Evidence was collected.","artifacts":[{"kind":"log","label":"Server boot","path":%q},{"kind":"image","shows":"ui","label":"Checkout","path":%q}]}`, logPath, shot)
	insertCompletedStep(t, sctx, types.StepTest, findings, "")
	steps, err := sctx.DB.GetStepsByRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	rounds := map[string][]*db.StepRound{}

	pubs := collectPRTestingArtifacts(sctx, steps, rounds)

	if len(pubs) != 1 || pubs[0].Media == nil {
		t.Fatalf("upload candidates = %+v, want only the media", pubs)
	}
}
