//go:build e2e

package e2e

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	toon "github.com/toon-format/toon-go"

	"github.com/kunchenguid/no-mistakes/internal/scm/plugin/fakeplugin"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const (
	notValidatedUntestedName  = "refund reaches the payment provider"
	notValidatedUntestedWhy   = "needs a live payment-provider sandbox credential this lab does not have"
	notValidatedApproveReason = "provider sandbox is down; refund path reviewed by hand"
)

// notValidatedScenarioYAML scripts the canned evidence agent: with gaps it
// drives two of three scenarios and returns inconclusive; without gaps every
// scenario is driven live and the verdict is go.
func notValidatedScenarioYAML(t *testing.T, gaps bool) string {
	t.Helper()
	verdict, third := "go", `        - name: refund reaches the payment provider
          result: pass
          live: true
          evidence: "fakeagent: refund reached the provider sandbox"
          reason: ""`
	if gaps {
		verdict, third = "inconclusive", `        - name: `+notValidatedUntestedName+`
          result: untested
          live: false
          evidence: ""
          reason: "`+notValidatedUntestedWhy+`"`
	}
	path := filepath.Join(t.TempDir(), "scenario.yaml")
	if err := os.WriteFile(path, []byte(`actions:
  - match: "You are validating a code change by driving the product itself."
    text: "scenarios driven"
    structured:
      findings: []
      summary: "scenarios driven"
      tested: ["fakeagent: drove checkout, cart and refund"]
      testing_summary: "drove the refund journey scenarios"
      artifacts: []
      verdict: `+verdict+`
      scenarios:
        - name: checkout succeeds
          result: pass
          live: true
          evidence: "fakeagent: checkout returned 200"
          reason: ""
        - name: cart persists
          result: pass
          live: true
          evidence: "fakeagent: cart survived a reload"
          reason: ""
`+third+`
  - text: "no issues found"
    structured:
      findings: []
      summary: "no issues found"
      risk_level: low
      risk_rationale: "synthetic clean response"
      risk_scope: source-or-external
`), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestNotValidatedLiveJourney drives real CLI/daemon/gate runs against a
// stateful fake GitHub. The gap variants leave three kinds of work unvalidated
// live: a Test scenario the canned evidence agent reports as untested, an
// inconclusive verdict the operator approves with a reason (a Test exception),
// and steps skipped with --skip. They check that the agent-facing run view
// lists all of them with their reasons, and that the published PR body carries
// the Not validated live callout outside every fold: under pr.appendix:
// minimal (which omits the Testing section), collapsed (which folds the
// evidence), and machine-local pr.update_text: keep (inside the owned
// appendix). The canned agent stands in for the model; nothing here claims it
// drove a product.
func TestNotValidatedLiveJourney(t *testing.T) {
	for _, tc := range []struct {
		name, appendix string
		keep           bool
	}{
		{name: "minimal", appendix: "minimal"},
		{name: "collapsed", appendix: "collapsed"},
		{name: "keep", appendix: "collapsed", keep: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const branch = "feature/not-validated-live"
			opts := SetupOpts{Agent: "claude", Scenario: notValidatedScenarioYAML(t, true)}
			if tc.keep {
				// pr.update_text is machine-local only, keyed by the origin
				// setupStatefulGitHub installs.
				opts.GlobalConfigExtra = "repository_overrides:\n  https://github.com/example/closes.git:\n    pr:\n      update_text: keep\n"
			}
			h := NewHarness(t, opts)
			statePath := setupStatefulGitHub(t, h)
			// pr.appendix is trusted default-branch config.
			h.CommitChange("main", ".no-mistakes.yaml", "allow_repo_commands: true\nno_ci: true\npr:\n  appendix: "+tc.appendix+"\n", "configure appendix")
			if out, err := h.runGit(t.Context(), h.WorkDir, "push", "origin", "main"); err != nil {
				t.Fatalf("push main: %v\n%s", err, out)
			}
			if out, err := h.Run("init"); err != nil {
				t.Fatalf("init: %v\n%s", err, out)
			}
			h.CommitChange(branch, "refund.txt", "refund feature\n", "add refunds")

			if out, err := h.Run("axi", "run", "--intent", "Let a customer refund an order", "--skip", "document,ci"); err != nil {
				t.Fatalf("axi run: %v\n%s", err, out)
			}
			waitForStepStatus(t, h, branch, types.StepTest, types.StepStatusAwaitingApproval, 60*time.Second)
			respond, err := h.Run("axi", "respond", "--step", "test", "--action", "approve", "--reason", notValidatedApproveReason)
			if err != nil {
				t.Fatalf("respond: %v\n%s", err, respond)
			}
			t.Logf("axi respond output:\n%s", respond)
			assertValidationBlock(t, "respond", respond, notValidatedUntestedName, notValidatedUntestedWhy, notValidatedApproveReason, true)

			status, err := h.Run("axi", "status")
			if err != nil {
				t.Fatalf("status: %v\n%s", err, status)
			}
			t.Logf("axi status output:\n%s", status)
			assertValidationBlock(t, "status", status, notValidatedUntestedName, notValidatedUntestedWhy, notValidatedApproveReason, false)

			body := livePRBody(t, statePath, branch)
			t.Logf("published PR body:\n%s", body)
			callout := strings.Index(body, "## Not validated live")
			if callout < 0 {
				t.Fatalf("published body has no Not validated live callout:\n%s", body)
			}
			for _, want := range []string{
				"1 of 3 scenarios were not driven live against the product.",
				"**Untested:** " + notValidatedUntestedName + " - " + notValidatedUntestedWhy,
				"**Test exception** (`test`): live validation verdict: inconclusive Test exception approved: " + notValidatedApproveReason,
				"**Skipped by operator** (`document`): no reason recorded",
				"**Skipped by operator** (`ci`): no reason recorded",
			} {
				if !strings.Contains(body, want) {
					t.Errorf("published body missing %q", want)
				}
			}
			if fold := strings.Index(body, "<details"); fold >= 0 && fold < callout {
				t.Errorf("callout is inside a fold")
			}
			switch {
			case tc.keep:
				start, end := strings.Index(body, "<!-- no-mistakes-pr-appendix:v1"), strings.Index(body, "<!-- /no-mistakes-pr-appendix:v1 -->")
				if start < 0 || end < 0 || callout < start || callout > end {
					t.Errorf("callout is not inside the owned appendix")
				}
			case tc.appendix == "collapsed":
				fold := strings.Index(body, "<summary>Validation</summary>")
				if fold < 0 || fold < callout {
					t.Errorf("collapsed body has no Validation fold after the callout")
				}
			default:
				if strings.Contains(body, "## Testing") {
					t.Errorf("minimal appendix published the Testing section")
				}
			}
			if !tc.keep {
				if changed := strings.Index(body, "## What Changed"); changed >= 0 && changed < callout {
					t.Errorf("callout does not lead the narrative")
				}
			}
		})
	}
}

// TestFullyLiveRunReportsNoValidationGaps drives a run whose Test step
// drives every scenario live, with no exception and no skip, to CI readiness
// against the reference provider-plugin forge (whose checks pass). Neither the
// drive result, axi status, nor the published PR body may report a gap.
func TestFullyLiveRunReportsNoValidationGaps(t *testing.T) {
	const branch = "feature/fully-live"
	h, statePath := pluginForgeRun(t, branch, []fakeplugin.Check{{Name: "build", Bucket: "pass", State: "SUCCESS"}}, "", false)

	out, err := h.Run("axi", "run", "--intent", "Let a customer refund an order", "--wait", "50s")
	t.Logf("axi run output:\n%s", out)
	if err != nil {
		t.Fatalf("axi run: %v\n%s", err, out)
	}
	assertNoValidationGaps(t, "run", out, true)
	status, err := h.Run("axi", "status")
	if err != nil {
		t.Fatalf("status: %v\n%s", err, status)
	}
	t.Logf("axi status output:\n%s", status)
	assertNoValidationGaps(t, "status", status, false)

	body := pluginPRBody(t, statePath)
	t.Logf("published PR body:\n%s", body)
	if !strings.Contains(body, "no-mistakes-pipeline-attestation") {
		t.Fatalf("published body carries no attestation:\n%s", body)
	}
	if strings.Contains(body, "## Not validated live") || strings.Contains(body, "not driven live against the product.") {
		t.Fatalf("fully live run published a Not validated live callout:\n%s", body)
	}
	if run := h.ActiveRun(branch); run != nil {
		h.CancelRun(run.ID)
	}
}

// pluginForgeRun prepares a fully live run on branch against the reference
// provider-plugin forge, which reports checks for every PR, and returns the
// harness and the forge state path. A non-empty appendix is committed as the
// trusted pr.appendix mode, and keep sets machine-local pr.update_text: keep
// so the body carries an owned appendix.
func pluginForgeRun(t *testing.T, branch string, checks []fakeplugin.Check, appendix string, keep bool) (*Harness, string) {
	t.Helper()
	const (
		pluginHost = "ssm.example.com"
		remoteURL  = "https://" + pluginHost + "/team/repo.git"
	)
	h := NewHarness(t, SetupOpts{
		Agent:    "claude",
		Scenario: notValidatedScenarioYAML(t, false),
		GlobalConfigExtra: "provider_plugins:\n" +
			"  ssm:\n" +
			"    command: " + fakeplugin.ExecutableName + "\n" +
			"    hosts: [" + pluginHost + "]\n" +
			"    timeout: 30s\n" + keepUpdateText(keep, remoteURL),
	})
	configureGitURLRewrite(t, h, remoteURL, h.UpstreamDir)
	if out, err := h.runGit(t.Context(), h.WorkDir, "remote", "set-url", "origin", remoteURL); err != nil {
		t.Fatalf("set plugin-host origin: %v\n%s", err, out)
	}
	stateDir := filepath.Dir(h.AgentLog)
	statePath := filepath.Join(stateDir, "provider-plugin-state.json")
	if err := fakeplugin.Save(statePath, fakeplugin.State{Checks: checks, FailedLogs: "build: exit status 1"}); err != nil {
		t.Fatal(err)
	}
	t.Setenv(fakeplugin.EnvState, statePath)
	t.Setenv(fakeplugin.EnvLog, filepath.Join(stateDir, "provider-plugin-requests.ndjson"))
	if appendix != "" {
		h.CommitChange("main", ".no-mistakes.yaml", "pr:\n  appendix: "+appendix+"\n", "configure appendix")
		if out, err := h.runGit(t.Context(), h.WorkDir, "push", "origin", "main"); err != nil {
			t.Fatalf("push main: %v\n%s", err, out)
		}
	}
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	h.CommitChange(branch, "refund.txt", "refund feature\n", "add refunds")
	return h, statePath
}

// ownedAppendix returns the owned appendix of body and everything outside its
// markers, after checking the digest its start marker carries (sha256 of the
// appendix text, the published ownership contract).
func ownedAppendix(t *testing.T, body string) (appendix, outside string) {
	t.Helper()
	const startMarker, endMarker = "<!-- no-mistakes-pr-appendix:v1 sha256=", "\n<!-- /no-mistakes-pr-appendix:v1 -->"
	start, end := strings.Index(body, startMarker), strings.Index(body, endMarker)
	if start < 0 || end < start {
		t.Fatalf("body has no owned appendix:\n%s", body)
	}
	digest, appendix, ok := strings.Cut(body[start+len(startMarker):end], " -->\n")
	if !ok {
		t.Fatalf("malformed appendix marker:\n%s", body)
	}
	if sum := fmt.Sprintf("%x", sha256.Sum256([]byte(appendix))); sum != digest {
		t.Fatalf("appendix digest %s does not match its content (%s):\n%s", digest, sum, body)
	}
	return appendix, body[:start] + body[end+len(endMarker):]
}

func keepUpdateText(keep bool, remoteURL string) string {
	if !keep {
		return ""
	}
	return "repository_overrides:\n  " + remoteURL + ":\n    pr:\n      update_text: keep\n"
}

func pluginPRBody(t *testing.T, statePath string) string {
	t.Helper()
	state, err := fakeplugin.Load(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.PRs) != 1 {
		t.Fatalf("plugin PRs = %+v", state.PRs)
	}
	return state.PRs[0].Body
}

// TestCIGateSkipRefreshesTheNotValidatedLiveCallout drives a fully live run
// whose CI check fails, so the CI step parks (the harness sets auto_fix.ci:
// 0), and the operator skips it there, in the default, minimal and collapsed
// appendix modes. The PR body was published before CI with no callout; after
// the skip the published body must be exactly that body with the callout
// inserted after Intent: in minimal and collapsed bodies no "## " heading
// follows the lead, so anything bounded by headings would eat the risk line,
// the fold or the attestation.
func TestCIGateSkipRefreshesTheNotValidatedLiveCallout(t *testing.T) {
	const (
		intent  = "## Intent\n\nLet a customer refund an order"
		callout = "## Not validated live\n\n⚠️ **1 exception or skip let this run continue without that validation.**\n\n- **Skipped by operator** (`ci`): no reason recorded"
	)
	for _, tc := range []struct {
		name, appendix string
		keep           bool
	}{
		{name: "default"},
		{name: "minimal", appendix: "minimal"},
		{name: "collapsed", appendix: "collapsed"},
		{name: "keep minimal", appendix: "minimal", keep: true},
		{name: "keep collapsed", appendix: "collapsed", keep: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const branch = "feature/ci-gate-skip"
			h, statePath := pluginForgeRun(t, branch, []fakeplugin.Check{{Name: "build", Bucket: "fail", State: "FAILURE"}}, tc.appendix, tc.keep)

			out, err := h.Run("axi", "run", "--intent", "Let a customer refund an order", "--wait", "50s")
			t.Logf("axi run output:\n%s", out)
			if err != nil {
				t.Fatalf("axi run: %v\n%s", err, out)
			}
			if !strings.Contains(out, "gate:") || !strings.Contains(out, "step: ci") {
				t.Fatalf("run did not park at the CI gate:\n%s", out)
			}
			before := pluginPRBody(t, statePath)
			t.Logf("PR body before the CI decision:\n%s", before)
			// The lead is at the top of the body, or of the owned appendix.
			lead, outsideBefore := before, ""
			if tc.keep {
				lead, outsideBefore = ownedAppendix(t, before)
			}
			if strings.Contains(before, "## Not validated live") || !strings.HasPrefix(lead, intent+"\n\n") {
				t.Fatalf("unexpected body before CI:\n%s", before)
			}

			respond, err := h.Run("axi", "respond", "--step", "ci", "--action", "skip", "--wait", "50s")
			t.Logf("axi respond output:\n%s", respond)
			if err != nil {
				t.Fatalf("respond: %v\n%s", err, respond)
			}
			body := pluginPRBody(t, statePath)
			t.Logf("PR body after the CI skip:\n%s", body)
			want := intent + "\n\n" + callout + "\n\n" + strings.TrimPrefix(lead, intent+"\n\n")
			got := body
			if tc.keep {
				var outside string
				got, outside = ownedAppendix(t, body)
				if outside != outsideBefore {
					t.Fatalf("refresh changed the body outside the owned appendix:\nbefore:\n%s\nafter:\n%s", before, body)
				}
			}
			if got != want {
				t.Fatalf("refresh did not insert exactly the callout:\nwant:\n%s\ngot:\n%s", want, got)
			}
			if !strings.Contains(body, "no-mistakes-pipeline-attestation") {
				t.Fatalf("published body carries no attestation:\n%s", body)
			}
		})
	}
}

// assertNoValidationGaps checks that a fully live run reports its live count
// and nothing it left unvalidated.
func assertNoValidationGaps(t *testing.T, label, out string, wantHelp bool) {
	t.Helper()
	start := strings.Index(out, "run:\n")
	if start < 0 {
		t.Fatalf("%s: no run document:\n%s", label, out)
	}
	var doc struct {
		Run struct {
			Validation struct {
				Verdict    string     `toon:"verdict"`
				Live       int        `toon:"live"`
				Total      int        `toon:"total"`
				Untested   []struct{} `toon:"untested"`
				Exceptions []struct{} `toon:"exceptions"`
			} `toon:"validation"`
		} `toon:"run"`
		Help []string `toon:"help"`
	}
	if err := toon.UnmarshalString(out[start:], &doc); err != nil {
		t.Fatalf("%s: parse output: %v\n%s", label, err, out)
	}
	v := doc.Run.Validation
	if v.Verdict != types.TestVerdictGo || v.Live != 3 || v.Total != 3 || len(v.Untested) != 0 || len(v.Exceptions) != 0 {
		t.Fatalf("%s: validation = %+v, want go 3/3 with no gaps", label, v)
	}
	if wantHelp {
		joined := strings.Join(doc.Help, "\n")
		if strings.Contains(joined, "not validated live") || strings.Contains(joined, "skipped by the operator") {
			t.Fatalf("%s: fully live run got a gap help line:\n%s", label, joined)
		}
	}
}

func assertValidationBlock(t *testing.T, label, out, untestedName, untestedWhy, approveReason string, wantHelp bool) {
	t.Helper()
	start := strings.Index(out, "run:\n")
	if start < 0 {
		t.Fatalf("%s: no run document:\n%s", label, out)
	}
	var doc struct {
		Outcome string `toon:"outcome"`
		Run     struct {
			Validation struct {
				Verdict  string `toon:"verdict"`
				Live     int    `toon:"live"`
				Total    int    `toon:"total"`
				Untested []struct {
					Scenario string `toon:"scenario"`
					Reason   string `toon:"reason"`
				} `toon:"untested"`
				Exceptions []struct {
					Step   string `toon:"step"`
					Kind   string `toon:"kind"`
					Reason string `toon:"reason"`
				} `toon:"exceptions"`
			} `toon:"validation"`
		} `toon:"run"`
		Help []string `toon:"help"`
	}
	if err := toon.UnmarshalString(out[start:], &doc); err != nil {
		t.Fatalf("%s: parse output: %v\n%s", label, err, out)
	}
	if doc.Outcome != "passed-with-override" {
		t.Fatalf("%s: outcome = %q, want passed-with-override", label, doc.Outcome)
	}
	v := doc.Run.Validation
	if v.Verdict != types.TestVerdictInconclusive || v.Live != 2 || v.Total != 3 {
		t.Fatalf("%s: validation = %+v", label, v)
	}
	if len(v.Untested) != 1 || v.Untested[0].Scenario != untestedName || v.Untested[0].Reason != untestedWhy {
		t.Fatalf("%s: untested = %+v", label, v.Untested)
	}
	kinds := map[string]string{}
	for _, e := range v.Exceptions {
		kinds[e.Step+"/"+e.Kind] = e.Reason
	}
	if !strings.Contains(kinds["test/test-exception"], approveReason) {
		t.Fatalf("%s: Test exception reason missing: %+v", label, v.Exceptions)
	}
	for _, step := range []string{"document", "ci"} {
		if kinds[step+"/operator-skip"] != "no reason recorded" {
			t.Fatalf("%s: %s operator skip missing: %+v", label, step, v.Exceptions)
		}
	}
	if !wantHelp {
		return
	}
	joined := strings.Join(doc.Help, "\n")
	for _, want := range []string{"1 of 3 scenarios were not validated live", "The document step was skipped by the operator", "Report the approved Test exception"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("%s: help missing %q:\n%s", label, want, joined)
		}
	}
}
