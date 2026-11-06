package steps

import (
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/reviewqa"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const (
	mr1016RunID          = "01M6NB7HE9AM2PB1JWRS60EN63"
	mr1016LinkBase       = "https://vps.nevilleweb.sk/no-mistakes"
	evidenceLinkTestBase = "https://evidence.example.com/no-mistakes"
)

// mr1016Fixture reads one file of the MR 1016 fixture under testdata.
func mr1016Fixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "evidence_links", "mr1016", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// mr1016Expected is a rendered-section fixture with its {{EVIDENCE_DIR}}/
// placeholder replaced by the run's evidence directory as the renderer cites
// it: this platform's spelling, HTML-escaped inside <code>.
func mr1016Expected(t *testing.T, name, evidenceDir string) string {
	t.Helper()
	text := strings.TrimRight(mr1016Fixture(t, name), "\n")
	return strings.ReplaceAll(text, "{{EVIDENCE_DIR}}/", html.EscapeString(evidenceDir+string(filepath.Separator)))
}

// newMR1016Context rebuilds run 01M6NB7HE9AM2PB1JWRS60EN63's Test step on a
// GitLab repository: the four screenshots and three transcripts its evidence
// directory held, and the findings the step recorded, so the PR step renders
// the same Testing section the merge request published.
func newMR1016Context(t *testing.T) *pipeline.StepContext {
	t.Helper()
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, prDraftAgent(), dir, baseSHA, headSHA, config.Commands{})
	enableDefaultEvidence(sctx)
	sctx.Repo.UpstreamURL = "https://gitlab.example.com/group/project.git"
	sctx.EvidenceDir = filepath.Join(t.TempDir(), mr1016RunID)
	for _, name := range []string{"hu-excluded.png", "both-pending.png", "hu-done-cz-pending.png", "rejected.png"} {
		writeEvidenceFile(t, sctx.EvidenceDir, name, []byte("png-bytes"))
	}
	for _, name := range []string{"roundtrip.json", "concurrent.json", "rejected.json"} {
		writeEvidenceFile(t, sctx.EvidenceDir, name, []byte(mr1016Fixture(t, name)))
	}
	escaped, err := json.Marshal(sctx.EvidenceDir + string(filepath.Separator))
	if err != nil {
		t.Fatal(err)
	}
	findings := strings.ReplaceAll(mr1016Fixture(t, "findings.json"), "{{EVIDENCE_DIR}}/", strings.Trim(string(escaped), `"`))
	insertCompletedStep(t, sctx, types.StepTest, findings, "")
	return sctx
}

// TestPRStep_EvidenceLinkBaseURL_MR1016Fixture is the acceptance test for
// test.evidence.link_base_url. The fixture is the Testing section a real
// GitLab merge request (run 01M6NB7HE9AM2PB1JWRS60EN63) published, in which
// every screenshot was a local path the reviewer had to copy out of the
// description. Unset, the PR step still renders that section byte for byte
// and adds no Evidence section. Set, every evidence file mention is a link
// under the base with no local path left, and the Evidence section lists the
// run's images as links without uploading anything.
func TestPRStep_EvidenceLinkBaseURL_MR1016Fixture(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, base, testing, evidence string
	}{
		{name: "unset keeps the published body", testing: "testing.md"},
		{name: "set links every evidence file", base: mr1016LinkBase, testing: "testing.linked.md", evidence: "evidence.linked.md"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sctx := newMR1016Context(t)
			sctx.Config.Test.Evidence.LinkBaseURL = tc.base
			var logs []string
			sctx.Log = func(line string) { logs = append(logs, line) }
			step := &PRStep{}

			_, _, testingMD, evidenceMD := step.buildPipelineSection(sctx, scm.ProviderGitLab)
			if want := mr1016Expected(t, tc.testing, sctx.EvidenceDir); testingMD != want {
				t.Fatalf("Testing section differs from the fixture\n--- want ---\n%s\n--- got ---\n%s", want, testingMD)
			}
			wantEvidence := ""
			if tc.evidence != "" {
				wantEvidence = mr1016Expected(t, tc.evidence, sctx.EvidenceDir)
			}
			if evidenceMD != wantEvidence {
				t.Fatalf("Evidence section differs from the fixture\n--- want ---\n%s\n--- got ---\n%s", wantEvidence, evidenceMD)
			}

			content, err := step.buildPRContent(sctx, "feature", "main", sctx.Run.BaseSHA, scm.ProviderGitLab, 0)
			if err != nil {
				t.Fatal(err)
			}
			joinedLogs := strings.Join(logs, "\n")
			if tc.base == "" {
				if !strings.Contains(content.Body, "(local file: <code>") || strings.Contains(content.Body, "## Evidence") {
					t.Fatalf("an unset base must publish today's body, got:\n%s", content.Body)
				}
				if strings.Contains(joinedLogs, "without uploading") {
					t.Fatalf("an unset base must not enter link mode, logs:\n%s", joinedLogs)
				}
				return
			}
			for _, forbidden := range []string{"local file:", sctx.EvidenceDir} {
				if strings.Contains(content.Body, forbidden) {
					t.Fatalf("published body still cites a local path (%q):\n%s", forbidden, content.Body)
				}
			}
			for _, section := range []string{testingMD, evidenceMD} {
				if !strings.Contains(content.Body, section) {
					t.Fatalf("published body lost a rendered section:\n%s", content.Body)
				}
			}
			prefix := "](" + mr1016LinkBase + "/" + mr1016RunID + "/"
			if strings.Count(testingMD, prefix) != 14 || strings.Count(evidenceMD, prefix) != 4 {
				t.Fatalf("links = %d in Testing and %d in Evidence, want 14 in Testing and 4 in Evidence", strings.Count(testingMD, prefix), strings.Count(evidenceMD, prefix))
			}
			for _, name := range []string{"roundtrip.json", "hu-excluded.png", "concurrent.json", "both-pending.png", "hu-done-cz-pending.png", "rejected.json", "rejected.png"} {
				if !strings.Contains(testingMD, prefix+name+")") {
					t.Fatalf("missing Testing link for %s", name)
				}
			}
			if !strings.Contains(joinedLogs, "without uploading") {
				t.Fatalf("link mode must say so in the step log, got:\n%s", joinedLogs)
			}
		})
	}
}

func TestEvidenceFileURL(t *testing.T) {
	t.Parallel()
	runDir := filepath.Join(t.TempDir(), "01RUN")
	for _, tc := range []struct {
		name, base, dir, path, want string
	}{
		{name: "unset base", dir: runDir, path: filepath.Join(runDir, "shot.png")},
		{name: "file in the run directory", base: evidenceLinkTestBase, dir: runDir, path: filepath.Join(runDir, "shot.png"), want: evidenceLinkTestBase + "/01RUN/shot.png"},
		{name: "nested file with a space", base: evidenceLinkTestBase, dir: runDir, path: filepath.Join(runDir, "browser", "step 2.png"), want: evidenceLinkTestBase + "/01RUN/browser/step%202.png"},
		{name: "file outside the run directory", base: evidenceLinkTestBase, dir: runDir, path: filepath.Join(filepath.Dir(runDir), "other.png")},
		{name: "the run directory itself", base: evidenceLinkTestBase, dir: runDir, path: runDir},
		{name: "unknown run directory", base: evidenceLinkTestBase, path: filepath.Join(runDir, "shot.png")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := evidenceFileURL(tc.base, tc.dir, tc.path); got != tc.want {
				t.Fatalf("evidenceFileURL(%q, %q, %q) = %q, want %q", tc.base, tc.dir, tc.path, got, tc.want)
			}
		})
	}
}

func TestRenderEvidenceFileLinkEscapesTheDestination(t *testing.T) {
	t.Parallel()
	runDir := filepath.Join(t.TempDir(), "01RUN")
	for _, tc := range []struct{ name, base, file, want string }{
		{name: "closing parenthesis in base", base: "https://evidence.example.com/archive)", file: "shot.png", want: "[shot](https://evidence.example.com/archive%29/01RUN/shot.png)"},
		{name: "HTML entity in base", base: "https://evidence.example.com/&copy;", file: "shot.png", want: "[shot](https://evidence.example.com/%26copy;/01RUN/shot.png)"},
		{name: "HTML entity in filename", base: "https://evidence.example.com/archive", file: "&copy;.png", want: "[shot](https://evidence.example.com/archive/01RUN/%26copy%3B.png)"},
		{name: "already escaped ampersand in base", base: "https://evidence.example.com/%26copy;", file: "shot.png", want: "[shot](https://evidence.example.com/%26copy;/01RUN/shot.png)"},
		{name: "parentheses in base", base: "https://evidence.example.com/(archive)", file: "shot.png", want: "[shot](https://evidence.example.com/%28archive%29/01RUN/shot.png)"},
		{name: "angle brackets and space in base", base: "https://evidence.example.com/<archive copy>", file: "shot.png", want: "[shot](https://evidence.example.com/%3Carchive%20copy%3E/01RUN/shot.png)"},
		{name: "backslash in base", base: `https://evidence.example.com/archive\copy`, file: "shot.png", want: "[shot](https://evidence.example.com/archive%5Ccopy/01RUN/shot.png)"},
		{name: "already escaped base and filename", base: "https://evidence.example.com/archive%29", file: "shot (1).png", want: "[shot](https://evidence.example.com/archive%29/01RUN/shot%20%281%29.png)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := renderEvidenceFileLink(tc.base, runDir, filepath.Join(runDir, tc.file), "shot")
			if got != tc.want {
				t.Fatalf("link = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRenderEvidenceFileLinkEscapesTheLabel(t *testing.T) {
	t.Parallel()
	runDir := filepath.Join(t.TempDir(), "01RUN")
	got := renderEvidenceFileLink(evidenceLinkTestBase, runDir, filepath.Join(runDir, "cart.png"), "Cart [HU] & <b>")
	want := "[Cart (HU) &amp; &lt;b&gt;](" + evidenceLinkTestBase + "/01RUN/cart.png)"
	if got != want {
		t.Fatalf("link = %q, want %q", got, want)
	}
	if got := renderEvidenceFileLink("", runDir, filepath.Join(runDir, "cart.png"), "Cart"); got != "" {
		t.Fatalf("unset base rendered %q, want nothing", got)
	}
}

// TestBuildTestingSummaryForPR_LinkBaseURLRendersLocalArtifactsAsLinks covers
// every Testing-section path that cites a local evidence file: the compact
// evidence line, the Source line of a captioned artifact, the expanded
// presentation, and the no-HTML Bitbucket flavor. An artifact with its own
// URL keeps it; the base only replaces local path citations.
func TestBuildTestingSummaryForPR_LinkBaseURLRendersLocalArtifactsAsLinks(t *testing.T) {
	t.Parallel()
	repoRoot := t.TempDir()
	evidenceRoot := filepath.Join(t.TempDir(), "01RUN")
	shot := filepath.Join(evidenceRoot, "checkout.png")
	writeEvidenceFile(t, evidenceRoot, "checkout.png", []byte("png-bytes"))
	writeEvidenceFile(t, filepath.Join(evidenceRoot, "nested"), "transcript.txt", []byte("Checkout transcript"))
	findings := fmt.Sprintf(`{"findings":[],"summary":"","testing_summary":"Evidence was collected.","scenarios":[{"name":"Checkout","result":"pass","live":true,"evidence":"checkout.png; unrelated, nested/transcript.txt missing.png | extra"}],"artifacts":[{"kind":"screenshot","label":"Checkout screenshot","path":%q},{"kind":"screenshot","label":"Captioned screenshot","path":%q,"content":"Checkout completed visually."},{"kind":"screenshot","label":"Remote screenshot","url":"https://example.com/remote.png","path":%q},{"kind":"text","label":"Transcript","path":%q}]}`, shot, shot, shot, filepath.Join(evidenceRoot, "nested", "transcript.txt"))

	steps := []*db.StepResult{{ID: "s1", StepName: types.StepTest, Status: types.StepStatusCompleted, FindingsJSON: &findings}}
	rounds := map[string][]*db.StepRound{"s1": {{Round: 1, Trigger: "initial", FindingsJSON: &findings, DurationMS: 300}}}
	link := evidenceLinkTestBase + "/01RUN/checkout.png"
	for _, tc := range []struct {
		name     string
		provider scm.Provider
		compact  bool
		want     []string
	}{
		{name: "compact", provider: scm.ProviderGitLab, compact: true, want: []string{"- Evidence: [Checkout screenshot](" + link + ")\n", "Source: [Captioned screenshot](" + link + ")\n"}},
		{name: "compact markdown flavor", provider: scm.ProviderBitbucket, compact: true, want: []string{"- Evidence: [Checkout screenshot](" + link + ")\n", "Source: [Captioned screenshot](" + link + ")\n"}},
		{name: "expanded", provider: scm.ProviderGitLab, compact: false, want: []string{"- Evidence: [Checkout screenshot](" + link + ")\n", "- Evidence: [Captioned screenshot](" + link + ")\n"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			md := buildPRTestingSummary(steps, rounds, "https://gitlab.example.com/group/project.git", "abc123", repoRoot, evidenceRoot, nil, tc.provider, nil, tc.compact, evidenceLinkTestBase)
			sourcePrefix := "- Evidence"
			if tc.compact {
				sourcePrefix = "Source"
			}
			for _, want := range append(tc.want, "- Evidence: [Remote screenshot](https://example.com/remote.png)", sourcePrefix+": [Transcript]("+evidenceLinkTestBase+"/01RUN/nested/transcript.txt)", "Checkout transcript", "[checkout.png]("+link+"); unrelated, [nested/transcript.txt]("+evidenceLinkTestBase+"/01RUN/nested/transcript.txt) missing.png \\| extra") {
				if !strings.Contains(md, want) {
					t.Fatalf("missing %q in:\n%s", want, md)
				}
			}
			for _, forbidden := range []string{"local file:", shot, "![Checkout screenshot]", "[Remote screenshot](" + evidenceLinkTestBase} {
				if strings.Contains(md, forbidden) {
					t.Fatalf("unexpected %q in:\n%s", forbidden, md)
				}
			}
		})
	}
}

func TestBuildTestingSummaryForPR_LinkBaseURLLeavesProseUntouched(t *testing.T) {
	t.Parallel()
	repoRoot := t.TempDir()
	evidenceRoot := filepath.Join(t.TempDir(), "01RUN")
	writeEvidenceFile(t, evidenceRoot, "shot.png", []byte("png-bytes"))
	writeEvidenceFile(t, evidenceRoot, "transcript.txt", []byte("Captured transcript"))
	findings := fmt.Sprintf(`{"findings":[],"summary":"","testing_summary":"Screenshot: shot.png","tested":["Inspected shot.png"],"artifacts":[{"kind":"screenshot","label":"Screenshot evidence","path":%q},{"kind":"text","label":"Transcript","path":%q,"content":"Caption: shot.png"}]}`, filepath.Join(evidenceRoot, "shot.png"), filepath.Join(evidenceRoot, "transcript.txt"))
	steps := []*db.StepResult{{ID: "s1", StepName: types.StepTest, Status: types.StepStatusCompleted, FindingsJSON: &findings}}
	for _, provider := range []scm.Provider{scm.ProviderGitLab, scm.ProviderBitbucket} {
		for _, compact := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/compact=%t", provider, compact), func(t *testing.T) {
				t.Parallel()
				render := func(base string) string {
					return buildPRTestingSummary(steps, nil, "", "", repoRoot, evidenceRoot, nil, provider, nil, compact, base)
				}
				unset, linked := render(""), render(evidenceLinkTestBase)
				prose := []string{"Screenshot: shot.png", "Caption: shot.png"}
				if !compact {
					prose = append(prose, "Inspected shot.png")
				}
				for _, text := range prose {
					if strings.Count(unset, text) != 1 || strings.Count(linked, text) != 1 {
						t.Fatalf("prose %q must survive once with either base setting:\nunset:\n%s\nlinked:\n%s", text, unset, linked)
					}
				}
				url := evidenceLinkTestBase + "/01RUN/shot.png"
				if !strings.Contains(linked, "- Evidence: [Screenshot evidence]("+url+")") || strings.Count(linked, url) != 1 {
					t.Fatalf("only the structured screenshot evidence entry must link to %s:\n%s", url, linked)
				}
			})
		}
	}
}

// TestPRStep_LinkBaseURLYieldsToUploadedAttachments: a GitHub user-attachments
// upload is still the rendering of choice, so the base changes nothing when
// the upload succeeded and only replaces the local-path fallback when it did
// not.
func TestPRStep_LinkBaseURLYieldsToUploadedAttachments(t *testing.T) {
	t.Parallel()
	t.Run("upload succeeded", func(t *testing.T) {
		t.Parallel()
		uploader := &stubMediaUploader{t: t, urls: map[string]string{"checkout.png": testAttachmentURL}}
		body, _ := renderPRWithScreenshot(t, uploader, func(ctx *testPRAttachCtx) {
			ctx.provider = scm.ProviderGitHub
			ctx.Config.Test.Evidence.LinkBaseURL = evidenceLinkTestBase
		})
		if !strings.Contains(body, "![Checkout screenshot]("+testAttachmentURL+")") || strings.Contains(body, evidenceLinkTestBase) {
			t.Fatalf("expected the uploaded attachment alone, got:\n%s", body)
		}
	})
	t.Run("upload failed", func(t *testing.T) {
		t.Parallel()
		uploader := &stubMediaUploader{err: errors.New("upload refused")}
		var evidenceDir string
		body, _ := renderPRWithScreenshot(t, uploader, func(ctx *testPRAttachCtx) {
			ctx.provider = scm.ProviderGitHub
			ctx.Config.Test.Evidence.LinkBaseURL = evidenceLinkTestBase
			evidenceDir = ctx.EvidenceDir
		})
		want := "- Evidence: [Checkout screenshot](" + evidenceLinkTestBase + "/" + filepath.Base(evidenceDir) + "/checkout.png)"
		if !strings.Contains(body, want) || strings.Contains(body, "local file:") {
			t.Fatalf("expected the local fallback to be a link %q, got:\n%s", want, body)
		}
	})
}

// TestScenarioEvidenceCellLinksCompleteFileReferences: with the base set, a
// scenario cell links every complete reference to a file that exists under
// the evidence root - a nested name with a space, an absolute in-root path, a
// name before sentence punctuation - and leaves prose, unknown names, and
// partial matches plain. The review conversation directory is never a
// candidate. Unset, the cell is today's cell byte for byte.
func TestScenarioEvidenceCellLinksCompleteFileReferences(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "Test Evidence", "01RUN")
	writeEvidenceFile(t, filepath.Join(root, "browser"), "step 2.png", []byte("png"))
	writeEvidenceFile(t, root, "roundtrip.json", []byte("{}"))
	writeEvidenceFile(t, filepath.Join(root, reviewqa.DirName), "questions.ndjson", []byte("{}"))
	names := evidenceFileNames(root)
	if got, want := strings.Join(names, ","), "browser/step 2.png,roundtrip.json"; got != want {
		t.Fatalf("evidenceFileNames = %q, want %q", got, want)
	}
	writeEvidenceFile(t, root, "shot,one.png", []byte("png"))
	writeEvidenceFile(t, root, "shot;two.png", []byte("png"))
	writeEvidenceFile(t, root, "roundtrip.json,extra", []byte("{}"))
	names = evidenceFileNames(root)
	opts := testingSummaryOptions{flavor: prBodyHTML, evidenceRoot: root, evidenceLinkBase: evidenceLinkTestBase}
	abs := filepath.Join(root, "roundtrip.json")
	step2 := "[browser/step 2.png](" + evidenceLinkTestBase + "/01RUN/browser/step%202.png)"
	roundtrip := "[roundtrip.json](" + evidenceLinkTestBase + "/01RUN/roundtrip.json)"
	for _, tc := range []struct{ name, cell, want string }{
		{"name with a space", "browser/step 2.png; roundtrip.json", step2 + "; " + roundtrip},
		{"absolute path inside the root", "see " + abs + " and notes", "see " + roundtrip + " and notes"},
		{"absolute comma filename", filepath.Join(root, "shot,one.png"), "[shot,one.png](" + evidenceLinkTestBase + "/01RUN/shot%2Cone.png)"},
		{"absolute semicolon filename", filepath.Join(root, "shot;two.png"), "[shot;two.png](" + evidenceLinkTestBase + "/01RUN/shot%3Btwo.png)"},
		{"backticked relative name", "see `roundtrip.json` here", "see " + roundtrip + " here"},
		{"double quoted relative name", "see \"roundtrip.json\" here", "see " + roundtrip + " here"},
		{"single quoted relative name", "see 'roundtrip.json' here", "see " + roundtrip + " here"},
		{"quoted absolute path", "see `" + abs + "` here", "see " + roundtrip + " here"},
		{"double backticked relative name", "see `` roundtrip.json `` here", "see " + roundtrip + " here"},
		{"code span holding two names links both", "`roundtrip.json; browser/step 2.png` then roundtrip.json", roundtrip + "; " + step2 + " then " + roundtrip},
		{"grouped span with prose and absolute path", "`see " + abs + "; browser/step 2.png and notes`", "see " + roundtrip + "; " + step2 + " and notes"},
		{"double backtick grouped span", "``roundtrip.json; browser/step 2.png``", roundtrip + "; " + step2},
		{"name followed by comma", "roundtrip.json, then notes", roundtrip + ", then notes"},
		{"longest punctuation filename", "roundtrip.json,extra", "[roundtrip.json,extra](" + evidenceLinkTestBase + "/01RUN/roundtrip.json%2Cextra)"},
		{"sentence punctuation", "Captured roundtrip.json.", "Captured " + roundtrip + "."},
		{"unknown name and prose", "missing.png, then roundtrip.json | extra", "missing.png, then " + roundtrip + " \\| extra"},
		{"partial names are not references", "myroundtrip.json and roundtrip.json2", "myroundtrip.json and roundtrip.json2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			unset := testingSummaryOptions{flavor: prBodyHTML, evidenceRoot: root}
			if got, want := scenarioEvidenceCell(tc.cell, unset, names), scenarioCell(tc.cell, prBodyHTML); got != want {
				t.Fatalf("unset cell = %q, want today's %q", got, want)
			}
			if got := scenarioEvidenceCell(tc.cell, opts, names); got != tc.want {
				t.Fatalf("cell %q = %q, want %q", tc.cell, got, tc.want)
			}
		})
	}
	unset := testingSummaryOptions{flavor: prBodyHTML, evidenceRoot: root}
	if got, want := scenarioEvidenceCell("browser/step 2.png; roundtrip.json", unset, nil), scenarioCell("browser/step 2.png; roundtrip.json", prBodyHTML); got != want {
		t.Fatalf("unset cell = %q, want today's %q", got, want)
	}
}

// TestEvidenceFileURLFollowsASymlinkedEvidenceRoot: an artifact may record
// the real path of a file under a symlinked evidence root; the URL still names
// the configured directory and the file.
func TestEvidenceFileURLFollowsASymlinkedEvidenceRoot(t *testing.T) {
	t.Parallel()
	real := filepath.Join(t.TempDir(), "real")
	writeEvidenceFile(t, real, "shot.png", []byte("png"))
	linkDir := filepath.Join(t.TempDir(), "01RUN")
	if err := os.Symlink(real, linkDir); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	realShot, err := filepath.EvalSymlinks(filepath.Join(real, "shot.png"))
	if err != nil {
		t.Fatal(err)
	}
	want := evidenceLinkTestBase + "/01RUN/shot.png"
	for _, path := range []string{realShot, filepath.Join(linkDir, "shot.png")} {
		if got := evidenceFileURL(evidenceLinkTestBase, linkDir, path); got != want {
			t.Fatalf("evidenceFileURL(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestScenarioEvidenceCellLinksSymlinkedRootReferences(t *testing.T) {
	t.Parallel()
	real := filepath.Join(t.TempDir(), "Test Evidence", "real")
	writeEvidenceFile(t, real, "shot.png", []byte("png"))
	root := filepath.Join(t.TempDir(), "01RUN")
	if err := os.Symlink(real, root); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	resolved, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatal(err)
	}
	opts := testingSummaryOptions{flavor: prBodyHTML, evidenceRoot: root, evidenceLinkBase: evidenceLinkTestBase}
	want := "[shot.png](" + evidenceLinkTestBase + "/01RUN/shot.png)"
	for _, reference := range []string{"shot.png", filepath.Join(root, "shot.png"), filepath.Join(resolved, "shot.png")} {
		if got := scenarioEvidenceCell(reference, opts, evidenceFileNames(root)); got != want {
			t.Fatalf("cell %q = %q, want %q", reference, got, want)
		}
	}
}

// TestScenarioEvidenceCellLeavesExistingLinksAlone: a cell that already
// carries a Markdown link, an image, an autolink, or a bare URL naming an
// evidence file is a complete reference. It renders exactly as it does with
// the setting unset, and only plain filename text around it becomes a link.
func TestScenarioEvidenceCellLeavesExistingLinksAlone(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "01RUN")
	writeEvidenceFile(t, root, "shot.png", []byte("png"))
	writeEvidenceFile(t, root, "roundtrip.json", []byte("{}"))
	names := evidenceFileNames(root)
	opts := testingSummaryOptions{flavor: prBodyHTML, evidenceRoot: root, evidenceLinkBase: evidenceLinkTestBase}
	linked := "[shot.png](" + evidenceLinkTestBase + "/01RUN/shot.png)"
	for _, cell := range []string{
		"[shot.png](https://example.com/shot.png)",
		"![shot.png](https://example.com/shot.png)",
		"<https://example.com/shot.png>",
		"https://example.com/shot.png",
		"[view shot.png here](https://example.com/shot.png)",
		"[shot.png](https://example.com/a(b))",
		`[shot.png](https://example.com/a(b) "shot.png")`,
		`![shot.png](https://example.com/a(b) "shot.png")`,
		`[shot.png](https://example.com/a(b) 'shot.png')`,
		`[shot.png](https://example.com/a(b) (shot.png))`,
		`![shot.png](<https://example.com/a(b)> "shot.png")`,
		`[shot.png](https://example.com/a\(b\) "a \"shot.png\" title")`,
		`[![shot.png](https://example.com/a)](https://example.com/b "shot.png")`,
		`[a \[shot.png\] b](https://example.com/c)`,
		"`go test ./...`",
	} {
		t.Run(cell, func(t *testing.T) {
			t.Parallel()
			for _, flavor := range []prBodyFlavor{prBodyHTML, prBodyMarkdown} {
				flavored := opts
				flavored.flavor = flavor
				if got, want := scenarioEvidenceCell(cell, flavored, names), scenarioCell(cell, flavor); got != want {
					t.Fatalf("existing reference %q (flavor %v) was rewritten to %q, want %q", cell, flavor, got, want)
				}
			}
		})
	}
	for _, tc := range []struct{ name, cell, want string }{
		{"link then plain name", "[shot.png](https://example.com/shot.png); shot.png", "[shot.png](https://example.com/shot.png); " + linked},
		{"titled link then plain name", `[shot.png](https://example.com/a(b) "shot.png"); shot.png`, `[shot.png](https://example.com/a(b) &#34;shot.png&#34;); ` + linked},
		{"titled image then plain name", `![shot.png](<https://example.com/a(b)> (shot.png)); shot.png`, `![shot.png](&lt;https://example.com/a(b)&gt; (shot.png)); ` + linked},
		{"plain name then bare URL", "see shot.png and https://example.com/shot.png", "see " + linked + " and https://example.com/shot.png"},
		{"bracketed prose before a plain name", "[HU] shot.png; [CZ](https://example.com)", "[HU] " + linked + "; [CZ](https://example.com)"},
		{"bracketed name is not a link", "[shot.png]", "[" + linked + "]"},
		{"double backtick single name", "``shot.png``", linked},
		{"grouped code span", "`shot.png; roundtrip.json`", linked + "; [roundtrip.json](" + evidenceLinkTestBase + "/01RUN/roundtrip.json)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := scenarioEvidenceCell(tc.cell, opts, names); got != tc.want {
				t.Fatalf("cell %q = %q, want %q", tc.cell, got, tc.want)
			}
		})
	}
}
