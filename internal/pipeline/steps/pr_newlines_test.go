package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// crlfHTTPEvidence has the shape of the evidence in every #1795 report: a
// captured HTTP exchange whose header lines end in CRLF and whose body line
// does not.
const crlfHTTPEvidence = "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\n\r\nlocal-upload-evidence\nHTTP/1.1 404 Not Found\r\nContent-Length: 314\r\n\r\n"

// gitlabRead is what GitLab returns for a description written as body.
func gitlabRead(body string) string { return strings.ReplaceAll(body, "\r\n", "\n") }

// suppliedTextWithEvidence is a create-path PR step whose run carries supplied
// text and a Test step that recorded a CRLF evidence file.
func suppliedTextWithEvidence(t *testing.T, upstream string, env []string) *pipeline.StepContext {
	t.Helper()
	dir, base, head := setupGitRepo(t)
	sctx := newPRTestContext(t, noDraftAgent(t), dir, base, head, config.Commands{})
	sctx.Repo.UpstreamURL = upstream
	sctx.Env = env
	completeCleanReview(t, sctx)
	enableDefaultEvidence(sctx)
	path := writeEvidenceFile(t, sctx.EvidenceDir, "uploads.log", []byte(crlfHTTPEvidence))
	insertCompletedStep(t, sctx, types.StepTest, fmt.Sprintf(`{"findings":[],"summary":"","testing_summary":"Uploads were probed.","artifacts":[{"kind":"log","label":"Uploads","path":%q}]}`, path), "")
	title, body := suppliedPRTitleText, suppliedPRBodyText
	sctx.Run.PRTitle, sctx.Run.PRBody = &title, &body
	return sctx
}

func storedMRDescription(t *testing.T, stateFile string) string {
	t.Helper()
	var mr struct {
		Description string `json:"description"`
	}
	if err := json.Unmarshal(mustRead(t, stateFile), &mr); err != nil {
		t.Fatal(err)
	}
	return mr.Description
}

func completedPipelineSteps() []*db.StepResult {
	return []*db.StepResult{
		{StepName: types.StepReview, Status: types.StepStatusCompleted},
		{StepName: types.StepTest, Status: types.StepStatusCompleted},
		{StepName: types.StepDocument, Status: types.StepStatusCompleted},
	}
}

// TestPRStep_GitLabAcceptsTheBodyItWroteFromCRLFEvidence is #1795 end to end:
// the PR step creates a merge request whose evidence carried CRLF line endings
// and reads it back from a fake GitLab that stores descriptions the way GitLab
// does. The next run's pre-push rebind must then recognise the appendix.
func TestPRStep_GitLabAcceptsTheBodyItWroteFromCRLFEvidence(t *testing.T) {
	t.Parallel()
	env, _, stateFile := fakeGlabWithMRState(t)
	sctx := suppliedTextWithEvidence(t, "https://gitlab.com/test/repo.git", env)

	out, err := (&PRStep{}).Execute(sctx)
	if err != nil || out == nil || out.PRURL == "" {
		t.Fatalf("create: %+v, %v", out, err)
	}
	stored := storedMRDescription(t, stateFile)
	if !strings.Contains(stored, "HTTP/1.1 200 OK\nContent-Type: text/plain\n\nlocal-upload-evidence\n") {
		t.Fatalf("evidence missing from the merge request:\n%s", stored)
	}
	if _, err := parsePROwnedBody(stored); err != nil {
		t.Fatalf("the next run cannot recognise the appendix: %v", err)
	}

	host, reason := buildHost(sctx, scm.ProviderGitLab)
	if host == nil {
		t.Fatal(reason)
	}
	next := strings.Repeat("b", 40)
	pr := &scm.PR{Number: "99", URL: out.PRURL}
	if err := restampPRAttestationWithSteps(context.Background(), host, pr, next, completedPipelineSteps(), nil, pipelineAttestationPolicy{}); err != nil {
		t.Fatalf("next run refused the body this run wrote: %v", err)
	}
	if got := publishedAttestationHead(t, storedMRDescription(t, stateFile)); got != next {
		t.Fatalf("attestation head = %s, want %s", got, next)
	}
}

// GitHub keeps CRLF line endings, so its bodies keep the evidence bytes as
// captured.
func TestPRStep_GitHubKeepsCRLFEvidenceBytes(t *testing.T) {
	t.Parallel()
	env, _ := fakeGH(t, "")
	sctx := suppliedTextWithEvidence(t, "https://github.com/test/repo", env)

	if _, err := (&PRStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	body := string(mustRead(t, envEntry(env, "FAKE_CLI_PR_BODY_FILE")))
	if !strings.Contains(body, "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\n\r\nlocal-upload-evidence\n") {
		t.Fatalf("GitHub body changed the captured evidence bytes:\n%q", body)
	}
	if _, err := parsePROwnedBody(body); err != nil {
		t.Fatal(err)
	}
}

func TestNormalizePRNewlines(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, in, want string }{
		{"crlf", "a\r\nb\r\n", "a\nb\n"},
		{"lone cr", "50%\r100%\n", "50%\n100%\n"},
		{"cr before crlf", "a\r\r\nb", "a\n\nb"},
		{"lf", "a\nb", "a\nb"},
	} {
		got := normalizePRNewlines(scm.ProviderGitLab, tc.in)
		if got != tc.want {
			t.Errorf("%s: normalizePRNewlines(gitlab, %q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
		if back := gitlabRead(got); back != got {
			t.Errorf("%s: GitLab reads %q back as %q", tc.name, got, back)
		}
		if again := normalizePRNewlines(scm.ProviderGitLab, got); again != got {
			t.Errorf("%s: not idempotent: %q then %q", tc.name, got, again)
		}
	}
	for _, provider := range []scm.Provider{scm.ProviderGitHub, scm.ProviderBitbucket, scm.ProviderUnknown} {
		if got := normalizePRNewlines(provider, "a\r\nb\rc"); got != "a\r\nb\rc" {
			t.Errorf("%s body changed: %q", provider, got)
		}
	}
}

// legacyGitLabBody is what GitLab returns for a body an earlier version
// published: the digest is stamped over an appendix whose evidence still
// carried CRLF line endings.
func legacyGitLabBody(t *testing.T, evidence ...string) string {
	t.Helper()
	_, appendix := ownedFixture(t)
	for _, text := range evidence {
		appendix += "\n\n**Evidence**\n\n```text\n" + text + "\n```"
	}
	sent := "## Overview\n\nAuthor text\n\n" + wrapPRAppendix(appendix) + "\nCloses test/repo#7"
	read := gitlabRead(sent)
	if read == sent {
		t.Fatal("fixture carries no CRLF")
	}
	return read
}

func TestParsePROwnedBodyRecognisesAppendixGitLabNormalized(t *testing.T) {
	t.Parallel()
	const lfLog = "$ npm test\nok 1 - renders\nok 2 - saves"
	multipart := "request:\nStatus: 200\n### Request Body\n" +
		"------Boundary\r\nContent-Disposition: form-data; name=\"ids[]\"\r\n\r\n01a1\r\n------Boundary--\r\n" +
		"\n### Response Body\n{\"ok\":true}"
	headers := "HTTP/1.1 403 Forbidden\r\nServer: Apache\r\nContent-Length: 317\r\n\r"
	for name, evidence := range map[string][]string{
		// GitLab.com and self-hosted #1795 shapes.
		"crlf throughout two previews":             {lfLog, headers, "HTTP/2 404 \r\nserver: openresty\r\n\r"},
		"crlf section inside lf output":            {multipart, lfLog},
		"lf section between crlf headers":          {lfLog, strings.TrimSuffix(crlfHTTPEvidence, "\n"), headers},
		"one mixed preview among many lf previews": {lfLog, lfLog, lfLog, multipart, lfLog, lfLog},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			read := legacyGitLabBody(t, evidence...)
			parts, err := parsePROwnedBody(read)
			if err != nil {
				t.Fatalf("an unedited legacy merge request was refused: %v", err)
			}
			if !strings.Contains(parts.before, "Author text") || !strings.Contains(parts.after, "Closes test/repo#7") {
				t.Fatalf("author text lost: %+v", parts)
			}
			for _, edit := range []struct{ old, new string }{
				{"ok 2 - saves", "ok 2 - saved"},
				{"Content-Length: ", "Content-Length:  "},
				{"Low recorded risk.", "Low recorded risk"},
			} {
				if !strings.Contains(read, edit.old) {
					continue
				}
				edited := strings.Replace(read, edit.old, edit.new, 1)
				if _, err := parsePROwnedBody(edited); err == nil || !strings.Contains(err.Error(), "edited or is malformed") {
					t.Fatalf("edited appendix (%q) accepted: %v", edit.new, err)
				}
			}
		})
	}
}

// The recovery restores line endings only in the shapes it searches; any other
// placement keeps failing closed, like an edit.
func TestParsePROwnedBodyRefusesCRLFPlacementOutsideTheSearch(t *testing.T) {
	t.Parallel()
	for name, extend := range map[string]func(string) string{
		"two crlf sections in one preview": func(appendix string) string {
			return appendix + "\n\n```text\na\nb\r\nc\nd\r\ne\nf\n```"
		},
		"crlf outside a code fence": func(appendix string) string {
			return strings.Replace(appendix, "Low recorded risk.\n", "Low recorded risk.\r\n", 1) + "\n\n```text\nx\n```"
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, appendix := ownedFixture(t)
			read := gitlabRead("## Overview\n\n" + wrapPRAppendix(extend(appendix)))
			if _, err := parsePROwnedBody(read); err == nil {
				t.Fatal("an appendix the search does not cover was accepted")
			}
		})
	}
}

func TestRebindRecoversAnAppendixGitLabNormalized(t *testing.T) {
	t.Parallel()
	env, _, stateFile := fakeGlabWithMRState(t)
	read := legacyGitLabBody(t, strings.TrimSuffix(crlfHTTPEvidence, "\n"))
	record, err := json.Marshal(map[string]any{"iid": 99, "title": "Detect plaintext profiles", "description": read, "web_url": "https://gitlab.com/test/repo/-/merge_requests/99"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stateFile, record, 0o644); err != nil {
		t.Fatal(err)
	}
	dir, base, head := setupGitRepo(t)
	sctx := newPRTestContext(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
	sctx.Repo.UpstreamURL = "https://gitlab.com/test/repo.git"
	sctx.Env = env
	host, reason := buildHost(sctx, scm.ProviderGitLab)
	if host == nil {
		t.Fatal(reason)
	}
	next := strings.Repeat("c", 40)
	pr := &scm.PR{Number: "99", URL: "https://gitlab.com/test/repo/-/merge_requests/99"}
	if err := restampPRAttestationWithSteps(context.Background(), host, pr, next, completedPipelineSteps(), nil, pipelineAttestationPolicy{}); err != nil {
		t.Fatalf("rebind refused a merge request nobody edited: %v", err)
	}
	rebound := storedMRDescription(t, stateFile)
	if publishedAttestationHead(t, rebound) != next {
		t.Fatalf("attestation was not rebound:\n%s", rebound)
	}
	if _, err := parsePROwnedBody(rebound); err != nil {
		t.Fatalf("the recovered body is not owned: %v", err)
	}
	if !strings.Contains(rebound, "local-upload-evidence") || !strings.HasPrefix(rebound, "## Overview\n\nAuthor text\n\n") {
		t.Fatalf("rebind changed author text or evidence:\n%s", rebound)
	}
}

// An existing merge request is updated through the same boundary, and the
// update is verified by reading it back.
func TestUpdateOwnedPR_GitLabSettlesWithCRLFEvidence(t *testing.T) {
	t.Parallel()
	env, _, stateFile := fakeGlabWithMRState(t)
	existing, appendix := ownedFixture(t)
	record, err := json.Marshal(map[string]any{"iid": 99, "title": "feat: story", "description": existing.Body, "web_url": "https://gitlab.com/test/repo/-/merge_requests/99"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stateFile, record, 0o644); err != nil {
		t.Fatal(err)
	}
	dir, base, head := setupGitRepo(t)
	sctx := newPRTestContext(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
	sctx.Repo.UpstreamURL = "https://gitlab.com/test/repo.git"
	sctx.Env = env
	host, reason := buildHost(sctx, scm.ProviderGitLab)
	if host == nil {
		t.Fatal(reason)
	}
	pr := &scm.PR{Number: "99", URL: "https://gitlab.com/test/repo/-/merge_requests/99"}
	live, err := host.(scm.PRContentReader).GetPRContent(context.Background(), pr)
	if err != nil {
		t.Fatal(err)
	}
	evidence := appendix + "\n\n```text\n" + crlfHTTPEvidence + "```"
	if err := updateOwnedPR(sctx, host, pr, live, "", "", evidence, 0); err != nil {
		t.Fatalf("update of an existing merge request did not settle: %v", err)
	}
	updated := storedMRDescription(t, stateFile)
	if !strings.Contains(updated, "HTTP/1.1 404 Not Found\nContent-Length: 314\n") {
		t.Fatalf("evidence missing from the merge request:\n%s", updated)
	}
	if _, err := parsePROwnedBody(updated); err != nil {
		t.Fatalf("the next run cannot recognise the appendix: %v", err)
	}
}
