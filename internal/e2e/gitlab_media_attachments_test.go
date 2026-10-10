//go:build e2e

package e2e

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// The GitLab media journey's test step reports five evidence files: two the
// adapter uploads (a screenshot and a recording), one GitLab does not render
// inline (an SVG, refused client-side before any upload), one the server
// refuses (a 403), and one the server answers with a URL on a foreign host
// (which the adapter must refuse to embed). Only the first two reach the
// merge request as attachments; the rest keep today's local-file rendering.
func writeGitLabMediaScenario(t *testing.T, artifacts string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gitlab-media-scenario.yaml")
	content := `actions:
  - match: "Review the code changes and return structured findings"
    text: "review clean"
    structured:
      findings: []
      summary: "review clean"
      risk_level: low
      risk_rationale: "one text file changed"
      risk_scope: source-or-external
  - match: "You are validating a code change by driving the product itself. Derive the scenarios this change must satisfy, then run each one against the real running product."
    text: "captured media evidence"
    write_evidence:
      - path: "checkout.png"
        content: "fake png bytes"
      - path: "checkout.webm"
        content: "fake webm bytes"
      - path: "diagram.svg"
        content: "<svg xmlns='http://www.w3.org/2000/svg'/>"
      - path: "broken.png"
        content: "fake png bytes the server refuses"
      - path: "hostile.png"
        content: "fake png bytes the server answers with a foreign URL"
    structured:
      findings: []
      summary: "targeted test passed"
      tested:
        - "fakeagent: drove the checkout flow"
      testing_summary: "Checkout flow validated with a screenshot and a recording."
      scenarios:
        - name: "fakeagent: checkout flow"
          result: pass
          live: true
          evidence: "checkout.png"
          reason: ""
      verdict: go
      artifacts:
` + artifacts + `
  - match: "Perform the combined documentation and lint housekeeping pass for this change."
    text: "nothing to document"
    structured:
      findings: []
      summary: "no documentation changes needed"
  - match: "Draft a pull request title and summary for the full branch delta."
    text: "PR drafted"
    structured:
      title: "feat: add gitlab media file"
      body: |
        ## What Changed

        - Add gitlab-media.txt.
  - text: "no issues found"
    structured:
      findings: []
      summary: "no issues found"
      risk_level: low
      risk_rationale: "no risks detected"
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write gitlab media scenario: %v", err)
	}
	return path
}

const gitLabMediaArtifacts = `        - kind: screenshot
          label: "Checkout screenshot"
          path: "{{evidence_dir}}/checkout.png"
        - kind: video
          label: "Checkout recording"
          path: "{{evidence_dir}}/checkout.webm"
        - kind: image
          label: "Checkout diagram"
          path: "{{evidence_dir}}/diagram.svg"
        - kind: screenshot
          label: "Refused screenshot"
          path: "{{evidence_dir}}/broken.png"
        - kind: screenshot
          label: "Hostile screenshot"
          path: "{{evidence_dir}}/hostile.png"`

// driveGitLabMediaJourney pushes one branch through the real daemon against
// a GitLab remote (a git URL rewrite stands in for gitlab.com, the fakeagent
// glab stub for the authenticated CLI) and returns the merge request
// description the PR step published plus the stub's invocation log.
func driveGitLabMediaJourney(t *testing.T, h *Harness, branch string) (body string, invocations []glabStubInvocation, runID string) {
	t.Helper()
	const remoteURL = "https://gitlab.com/example/widgets.git"
	configureGitURLRewrite(t, h, remoteURL, h.UpstreamDir)
	if out, err := h.runGit(t.Context(), h.WorkDir, "remote", "set-url", "origin", remoteURL); err != nil {
		t.Fatalf("set gitlab origin: %v\n%s", err, out)
	}

	glabLog := filepath.Join(filepath.Dir(h.AgentLog), "glab-"+strings.ReplaceAll(branch, "/", "-")+".log")
	t.Setenv("FAKEAGENT_GLAB_MODE", "project-mr")
	t.Setenv("FAKEAGENT_GLAB_LOG", glabLog)
	t.Setenv("FAKEAGENT_GLAB_HOST", "gitlab.com")
	t.Setenv("FAKEAGENT_GLAB_PROJECT", "example/widgets")
	t.Setenv("FAKEAGENT_GLAB_UPLOAD_REJECT", "broken.png")
	t.Setenv("FAKEAGENT_GLAB_UPLOAD_ABSOLUTE_URL", "hostile.png")

	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}

	h.CommitChange(branch, "gitlab-media.txt", "gitlab media e2e\n", "add gitlab media e2e file")
	h.PushToGate(branch)

	run := h.WaitForRun(branch, 120*time.Second)
	if run.Status != types.RunCompleted {
		t.Fatalf("run did not complete: status=%s error=%v", run.Status, deref(run.Error))
	}
	if run.PRURL == nil || *run.PRURL != "https://gitlab.com/example/widgets/-/merge_requests/7" {
		t.Fatalf("PR URL = %v, want the stub merge request URL", run.PRURL)
	}

	invocations = readGlabStubInvocations(t, glabLog)
	for _, inv := range invocations {
		if len(inv.Args) >= 2 && inv.Args[0] == "mr" && inv.Args[1] == "create" {
			if inv.SourceBranch != branch {
				t.Fatalf("mr create --source-branch = %q, want %q", inv.SourceBranch, branch)
			}
			body = inv.Description
		}
	}
	if body == "" {
		t.Fatalf("no `glab mr create` with a description in %+v", invocations)
	}
	t.Logf("published merge request description:\n%s", body)
	return body, invocations, run.ID
}

var gitLabUploadURL = regexp.MustCompile(`/uploads/[0-9a-f]{32}/[A-Za-z0-9._-]+`)

// TestGitLabMediaAttachmentsJourney drives the whole GitLab media path through
// the real daemon: the test step's image and video evidence is uploaded to the
// project through `glab api --method POST projects/<id>/uploads --form
// file=@<path>` at PR render time, the merge request description embeds the
// relative upload URLs GitLab expands (the recording in image syntax, which is
// what GitLab turns into an inline player), and every file the forge cannot
// take keeps today's local-file rendering instead of a dead or hostile link.
func TestGitLabMediaAttachmentsJourney(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: writeGitLabMediaScenario(t, gitLabMediaArtifacts)})
	body, invocations, runID := driveGitLabMediaJourney(t, h, "feature/gitlab-media")
	evidenceDir := filepath.Join(h.NMHome, "evidence", runID)

	uploads := map[string]glabStubInvocation{}
	for _, inv := range invocations {
		if len(inv.Args) == 0 || inv.Args[0] != "api" {
			continue
		}
		if inv.UploadFile == "" {
			t.Fatalf("api call without an upload file: %+v", inv)
		}
		if filepath.Dir(inv.UploadFile) != evidenceDir {
			t.Fatalf("upload %q is not the run's own evidence file under %s", inv.UploadFile, evidenceDir)
		}
		joined := strings.Join(inv.Args, " ")
		for _, want := range []string{"api --method POST projects/example%2Fwidgets/uploads --form file=@" + inv.UploadFile, "--hostname gitlab.com"} {
			if !strings.Contains(joined, want) {
				t.Fatalf("upload call %q lacks %q", joined, want)
			}
		}
		uploads[filepath.Base(inv.UploadFile)] = inv
		t.Logf("upload call: glab %s", joined)
	}
	for _, name := range []string{"checkout.png", "checkout.webm", "broken.png", "hostile.png"} {
		if _, ok := uploads[name]; !ok {
			t.Fatalf("no upload attempted for %s; uploads = %v", name, uploadNames(uploads))
		}
	}
	if _, ok := uploads["diagram.svg"]; ok {
		t.Fatalf("diagram.svg must be refused client-side before any upload; uploads = %v", uploadNames(uploads))
	}
	if len(uploads) != 4 {
		t.Fatalf("uploads = %v, want exactly the four candidates", uploadNames(uploads))
	}

	// The two the server accepted are embedded as relative upload URLs, the
	// recording with image syntax so GitLab renders a player.
	for _, want := range []string{
		"![Checkout screenshot](/uploads/" + uploadSecretFor("checkout.png") + "/checkout.png)",
		"![Checkout recording](/uploads/" + uploadSecretFor("checkout.webm") + "/checkout.webm)",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("merge request description lacks %q:\n%s", want, body)
		}
	}
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "/uploads/") {
			t.Fatalf("a GitLab upload rendered as a bare URL, which GitLab leaves as text: %q", line)
		}
	}
	for _, embedded := range gitLabUploadURL.FindAllString(body, -1) {
		switch filepath.Base(embedded) {
		case "checkout.png", "checkout.webm":
		default:
			t.Fatalf("unexpected upload URL embedded: %s", embedded)
		}
	}
	for _, name := range []string{"checkout.png", "checkout.webm"} {
		if strings.Contains(body, "local file: <code>"+filepath.Join(evidenceDir, name)) || strings.Contains(body, "local file: `"+filepath.Join(evidenceDir, name)) {
			t.Fatalf("uploaded %s still cites its local path:\n%s", name, body)
		}
	}

	// Everything the forge could not take keeps today's rendering.
	for _, name := range []string{"diagram.svg", "broken.png", "hostile.png"} {
		if !strings.Contains(body, "(local file:") || !strings.Contains(body, name) {
			t.Fatalf("%s must keep its local-file rendering:\n%s", name, body)
		}
	}
	for _, forbidden := range []string{"evil.example", "user-attachments", "GitHub"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("merge request description must not contain %q:\n%s", forbidden, body)
		}
	}
}

// TestGitLabMediaAttachmentsRespectAttachMediaOff is the opt-out: with
// test.evidence.attach_media false and no evidence branch, a GitLab run never
// calls the uploads endpoint and the screenshot keeps its local-file line.
func TestGitLabMediaAttachmentsRespectAttachMediaOff(t *testing.T) {
	h := NewHarness(t, SetupOpts{
		Agent:             "claude",
		Scenario:          writeGitLabMediaScenario(t, gitLabMediaArtifacts),
		GlobalConfigExtra: "test:\n  evidence:\n    attach_media: false\n",
	})
	body, invocations, runID := driveGitLabMediaJourney(t, h, "feature/gitlab-media-off")
	for _, inv := range invocations {
		if len(inv.Args) > 0 && inv.Args[0] == "api" {
			t.Fatalf("attach_media: false must never upload, got %v", inv.Args)
		}
	}
	if gitLabUploadURL.MatchString(body) {
		t.Fatalf("attach_media: false must not embed an upload URL:\n%s", body)
	}
	evidenceDir := filepath.Join(h.NMHome, "evidence", runID)
	if !strings.Contains(body, "(local file:") || !strings.Contains(body, filepath.Join(evidenceDir, "checkout.png")) {
		t.Fatalf("screenshot must keep its local-file rendering:\n%s", body)
	}
}

// uploadSecretFor mirrors the fakeagent glab stub's derivation of GitLab's
// 32-hex upload secret from the file name, so the journey can name the exact
// URL it expects to see embedded.
func uploadSecretFor(name string) string {
	sum := sha256.Sum256([]byte(name))
	return hex.EncodeToString(sum[:])[:32]
}

func uploadNames(uploads map[string]glabStubInvocation) []string {
	names := make([]string, 0, len(uploads))
	for name := range uploads {
		names = append(names, name)
	}
	return names
}

type glabStubInvocation struct {
	Args         []string `json:"args"`
	Hostname     string   `json:"hostname"`
	SourceBranch string   `json:"source_branch"`
	TargetBranch string   `json:"target_branch"`
	Title        string   `json:"title"`
	Description  string   `json:"description"`
	UploadFile   string   `json:"upload_file"`
}

func readGlabStubInvocations(t *testing.T, path string) []glabStubInvocation {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read glab log: %v", err)
	}
	var invocations []glabStubInvocation
	for _, line := range bytes.Split(data, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var inv glabStubInvocation
		if err := json.Unmarshal(line, &inv); err != nil {
			t.Fatalf("parse glab log line: %v\n%s", err, line)
		}
		invocations = append(invocations, inv)
	}
	if len(invocations) == 0 {
		t.Fatal("no glab invocations recorded; the pipeline never called the GitLab host")
	}
	return invocations
}
