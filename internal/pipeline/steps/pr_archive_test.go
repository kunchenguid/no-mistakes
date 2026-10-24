package steps

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Synthetic evidence models repeated fix explanations, long scenario tables
// and artifact lists without copying any private run logs or credentials.
func oversizedReport(t *testing.T, head string) string {
	t.Helper()
	var b strings.Builder
	b.WriteString(compliantPipelineBody(t, head))
	for round := 1; round <= 4; round++ {
		fmt.Fprintf(&b, "\n### Test fix round %d\n", round)
		for i := 0; i < 120; i++ {
			fmt.Fprintf(&b, "| Scenario %d/%d: Zażółć gęślą jaźń 😀 | pass | live | Full retained evidence for this exact synthetic candidate |\n", round, i)
			fmt.Fprintf(&b, "- Artifact %d/%d: [complete log](https://example.test/evidence/round-%d/scenario-%d.log)\n", round, i, round, i)
		}
	}
	return b.String()
}

func archivedReport(t *testing.T, sctx *pipeline.StepContext, body string) ([]byte, string) {
	t.Helper()
	match := regexp.MustCompile(`https://github.com/example/widgets/blob/([a-f0-9]{40,64})/([^ )]+)`).FindStringSubmatch(body)
	if len(match) != 3 {
		t.Fatalf("missing commit-pinned archive URL: %s", body)
	}
	data, err := git.RunRaw(sctx.Ctx, sctx.WorkDir, "show", match[1]+":"+match[2])
	if err != nil {
		t.Fatal(err)
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(data))
	if !strings.Contains(body, "`"+digest+"`") {
		t.Fatal("archive digest differs from live PR reference")
	}
	if _, err := parsePROwnedBody(string(data)); err != nil {
		t.Fatalf("archive ownership digest invalid: %v", err)
	}
	return data, match[1] + ":" + match[2]
}

func TestPRArchivePreservesCompleteReportAndAuthorBytes(t *testing.T) {
	t.Parallel()
	sctx, remote := newEvidencePublishContext(t, "feature/report")
	appendix := oversizedReport(t, sctx.Run.HeadSHA)
	parts := prOwnedBody{before: "# Human narrative\n\n- [x] Accepted\nZażółć 😀\n\n", after: "\n\nCloses example/widgets#7\n", managed: true}
	beforeHead := gitCmd(t, sctx.WorkDir, "rev-parse", "HEAD")
	main := gitCmd(t, remote, "rev-parse", "main")
	if _, err := composeOwnedPRContent(parts, "", appendix, 0); !errors.Is(err, errOwnedPRBudget) {
		t.Fatalf("fixture must reproduce publication size refusal: %v", err)
	}
	content, err := composeOwnedPRContentWithArchive(sctx, parts, "", appendix, 0)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := parsePROwnedBody(content.Body)
	if err != nil || parsed.before != parts.before || parsed.after != parts.after {
		t.Fatalf("author text or digest changed: %+v, %v", parsed, err)
	}
	if len(content.Body) > maxPullRequestBodyBytes || extractPipelineAttestationMarker(content.Body) != extractPipelineAttestationMarker(appendix) {
		t.Fatal("body exceeds budget or rewrote recorded attestation")
	}
	data, object := archivedReport(t, sctx, content.Body)
	if !bytes.Equal(data, []byte(wrapPRAppendix(cleanPRAppendix(appendix)))) {
		t.Fatal("report content was lost or changed")
	}
	// A later render appends a new archive without changing an old link.
	second, err := composeOwnedPRContentWithArchive(sctx, parts, "", appendix+"\nAnother recorded fact.", 0)
	if err != nil {
		t.Fatal(err)
	}
	newData, _ := archivedReport(t, sctx, second.Body)
	oldData, err := git.RunRaw(sctx.Ctx, sctx.WorkDir, "show", object)
	if err != nil || !bytes.Equal(oldData, data) || bytes.Equal(newData, data) {
		t.Fatal("archive links are not immutable")
	}
	if gitCmd(t, sctx.WorkDir, "rev-parse", "HEAD") != beforeHead || gitCmd(t, remote, "rev-parse", "main") != main || gitCmd(t, sctx.WorkDir, "status", "--porcelain") != "" {
		t.Fatal("archiving changed code HEAD, default branch or index/worktree")
	}
}

func TestPRArchiveByteBudgetBoundaries(t *testing.T) {
	t.Parallel()
	for _, unit := range []string{"x", "ą", "😀"} {
		t.Run(unit, func(t *testing.T) {
			sctx, remote := newEvidencePublishContext(t, "feature/boundary")
			base := compliantPipelineBody(t, sctx.Run.HeadSHA)
			parts := prOwnedBody{before: "Closes example/widgets#7"}
			initial, err := composeOwnedPRContent(parts, "", base, 0)
			if err != nil {
				t.Fatal(err)
			}
			remaining := maxPullRequestBodyBytes - len(initial.Body)
			report := base + strings.Repeat(unit, remaining/len(unit)) + strings.Repeat("x", remaining%len(unit))
			atLimit, err := composeOwnedPRContentWithArchive(sctx, parts, "", report, 0)
			if err != nil || len(atLimit.Body) != maxPullRequestBodyBytes || strings.Contains(atLimit.Body, "Complete generated report") {
				t.Fatalf("exact byte limit did not remain inline: bytes=%d, %v", len(atLimit.Body), err)
			}
			if refs := gitCmd(t, remote, "for-each-ref", "refs/heads/no-mistakes/evidence"); refs != "" {
				t.Fatal("inline report unexpectedly pushed an archive")
			}
			over, err := composeOwnedPRContentWithArchive(sctx, parts, "", report+unit, 0)
			if err != nil || !strings.Contains(over.Body, "Complete generated report") {
				t.Fatalf("over-limit report was not archived: %v", err)
			}
			archivedReport(t, sctx, over.Body)
		})
	}
}

func TestPRArchiveRefusalsPublishNothing(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"no-opt-in", "unsupported-host", "invalid-report", "wrong-head", "oversized-author", "oversized-suffix", "provider-limit", "code-branch", "push-refused"} {
		t.Run(mode, func(t *testing.T) {
			sctx, remote := newEvidencePublishContext(t, "feature/refused")
			report := oversizedReport(t, sctx.Run.HeadSHA)
			parts := prOwnedBody{before: "Human narrative", managed: true}
			limit := 0
			switch mode {
			case "no-opt-in":
				sctx.Config.Test.Evidence.StoreInRepo = false
			case "unsupported-host":
				sctx.Repo.UpstreamURL = "https://gitlab.com/example/widgets.git"
			case "invalid-report":
				report += "\n" + extractPipelineAttestationMarker(report)
			case "wrong-head":
				report = oversizedReport(t, strings.Repeat("b", 40))
			case "oversized-author":
				parts.before = strings.Repeat("author\n", maxPullRequestBodyBytes)
			case "oversized-suffix":
				parts.after = strings.Repeat("Closes example/widgets#7\n", maxPullRequestBodyBytes)
			case "provider-limit":
				limit = 100
			case "code-branch":
				sctx.Config.Test.Evidence.Branch = "main"
			case "push-refused":
				if err := os.WriteFile(filepath.Join(remote, "hooks", "pre-receive"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			content, err := composeOwnedPRContentWithArchive(sctx, parts, "", report, limit)
			if err == nil || content.Body != "" {
				t.Fatal("refused archive reported usable PR content")
			}
			if refs := gitCmd(t, remote, "for-each-ref", "refs/heads/no-mistakes/evidence"); refs != "" {
				t.Fatal("failed preflight or refused push published evidence")
			}
		})
	}
}

func TestPRArchiveUpdatePreservesConcurrentAuthorAndChecksWrites(t *testing.T) {
	t.Parallel()
	for _, failure := range []string{"", "write", "post-write"} {
		t.Run(failure, func(t *testing.T) {
			sctx, _ := newEvidencePublishContext(t, "feature/update")
			initial, err := composeOwnedPRContent(prOwnedBody{before: "Original author"}, "", compliantPipelineBody(t, sctx.Run.HeadSHA), 0)
			if err != nil {
				t.Fatal(err)
			}
			host := &ownershipRaceHost{body: initial.Body, read: func(h *ownershipRaceHost) error {
				if h.reads == 1 {
					h.body = strings.Replace(h.body, "Original author", "Updated author", 1) + "\nCloses example/widgets#9"
				}
				return nil
			}}
			if failure == "write" {
				host.writeError = errors.New("uncertain write")
			} else if failure == "post-write" {
				host.afterWrite = "\nConcurrent later edit"
			}
			err = updateOwnedPR(sctx, host, &scm.PR{Number: "42"}, scm.PRContent(initial), "", "", oversizedReport(t, sctx.Run.HeadSHA), 0)
			if failure != "" {
				if err == nil {
					t.Fatal("write/verification failure reported success")
				}
				return
			}
			if err != nil || host.writes != 1 || !strings.Contains(host.body, "Updated author") || !strings.HasSuffix(host.body, "Closes example/widgets#9") {
				t.Fatalf("concurrent author lost: writes=%d, err=%v", host.writes, err)
			}
			archivedReport(t, sctx, host.body)
		})
	}
}

func TestPRArchiveTemplatePublicationAndReadbackUsesRecordedEvidence(t *testing.T) {
	t.Parallel()
	sctx, _, _ := templateTestContext(t)
	_, remote := newEvidencePublishContext(t, "feature/target")
	gitCmd(t, sctx.WorkDir, "config", "url."+remote+".insteadOf", evidenceTestUpstream)
	sctx.Repo.UpstreamURL = evidenceTestUpstream
	sctx.Config.Test.Evidence = config.Evidence{StoreInRepo: true, Branch: "no-mistakes/evidence", Dir: ".no-mistakes/evidence"}
	summary := strings.Repeat("Recorded live evidence: Zażółć 😀. ", 3000)
	insertCompletedStep(t, sctx, types.StepTest, findingsJSON(t, types.Findings{TestingSummary: summary}), "")
	env, _ := fakeGH(t, "")
	sctx.Env = env
	out, err := (&PRStep{}).Execute(sctx)
	if err != nil || out == nil || out.PRURL == "" {
		t.Fatalf("PR publication: %+v, %v", out, err)
	}
	body, err := os.ReadFile(envEntry(env, "FAKE_CLI_PR_BODY_FILE"))
	if err != nil {
		t.Fatal(err)
	}
	parts, err := parsePROwnedBody(string(body))
	if err != nil || !strings.HasPrefix(parts.before, filledPRTemplate) {
		t.Fatalf("template author content lost: %v", err)
	}
	data, _ := archivedReport(t, sctx, string(body))
	if !bytes.Contains(data, []byte(strings.TrimSpace(summary))) {
		t.Fatal("recorded evidence was not preserved in full")
	}
}

func TestPRArchiveRetainsClosingRefsAndOverrideWithoutInventingPass(t *testing.T) {
	t.Parallel()
	sctx, _ := newEvidencePublishContext(t, "feature/overridden")
	sctx.ClosingIssueRefs = []string{"42", "other/repo#9"}
	report := oversizedReport(t, sctx.Run.HeadSHA)
	original := extractPipelineAttestationMarker(report)
	attestation := parsePipelineAttestationForTest(t, report)
	attestation.Steps = []pipelineAttestationStep{{Step: types.StepTest, Status: types.StepStatusCompleted, OverrideReason: "recorded unresolved command failure"}}
	attestation.LiveValidation = &pipelineAttestationLiveValidation{Verdict: "inconclusive", Live: 0, Total: 4}
	payload, err := json.Marshal(attestation)
	if err != nil {
		t.Fatal(err)
	}
	marker := pipelineAttestationCommentPrefix + string(payload) + pipelineAttestationCommentClosingToken
	report = strings.Replace(report, original, marker, 1)
	report += "\nPrivate home: /Users/synthetic/evidence.log\nControl: \x1b\n"
	content, err := composeOwnedPRContentWithArchive(sctx, prOwnedBody{before: "Human narrative"}, "", report, 0)
	if err != nil {
		t.Fatal(err)
	}
	if extractPipelineAttestationMarker(content.Body) != marker {
		t.Fatal("recorded override or inconclusive evidence was rewritten")
	}
	if err := verifyClosingIssuesInBody(content.Body, sctx); err != nil {
		t.Fatalf("requested closing references left the live body: %v", err)
	}
	data, _ := archivedReport(t, sctx, content.Body)
	if bytes.Contains(data, []byte("/Users/synthetic")) || bytes.Contains(data, []byte{0x1b}) {
		t.Fatal("archive bypassed publication redaction/control normalization")
	}
	if !bytes.Contains(data, []byte("~/evidence.log")) {
		t.Fatal("archive dropped rather than sanitized evidence")
	}
}
