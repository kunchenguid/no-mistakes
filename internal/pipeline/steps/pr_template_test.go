package steps

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const testPRTemplate = "# Overview\n\n<!-- Describe the final change. -->\n\n## Testing\n\n- [ ] Maintainer approves rollout\n"
const filledPRTemplate = "# Overview\n\nAdd a Bar helper.\n\n## Testing\n\n- [ ] Maintainer approves rollout\n"

func templateTestContext(t *testing.T) (*pipeline.StepContext, *mockAgent, string) {
	t.Helper()
	dir, base, _ := setupGitRepo(t)
	name := ".github/pull_request_template.md"
	if err := os.MkdirAll(filepath.Join(dir, ".github"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(name)), []byte(testPRTemplate), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", ".github")
	gitCmd(t, dir, "commit", "-m", "trusted template")
	trusted := gitCmd(t, dir, "rev-parse", "HEAD")
	// Neither the current file nor a later committed pushed version may supply
	// the PR agent's template. This also models config recovery with a pin.
	if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(name)), []byte("## CONTRIBUTOR TEMPLATE MUST NOT WIN\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", ".github")
	gitCmd(t, dir, "commit", "-m", "pushed template override")
	head := gitCmd(t, dir, "rev-parse", "HEAD")
	ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		data, _ := json.Marshal(prContent{Title: "feat(pipeline): fill template", Body: filledPRTemplate})
		return &agent.Result{Output: data}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
	sctx.Config.PR.Template = name
	sctx.Config.TrustedConfigSHA = trusted
	sr, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.UpdateStepStatus(sr.ID, types.StepStatusCompleted); err != nil {
		t.Fatal(err)
	}
	return sctx, ag, trusted
}

func TestPRTemplateReadsPinnedBlobNotWorktree(t *testing.T) {
	t.Parallel()
	sctx, _, trusted := templateTestContext(t)
	got, err := loadPRTemplate(sctx.Ctx, sctx.WorkDir, trusted, sctx.Config.PR.Template)
	if err != nil || got != testPRTemplate {
		t.Fatalf("pinned bytes = %q, err %v", got, err)
	}
	for _, sha := range []string{"", "main", "HEAD", "deadbeef", strings.Repeat("f", 40)} {
		if _, err := loadPRTemplate(sctx.Ctx, sctx.WorkDir, sha, sctx.Config.PR.Template); err == nil {
			t.Errorf("unpinned/unreadable %q accepted", sha)
		}
	}
	for _, name := range []string{"missing.md", ".github", "../outside", "/etc/passwd", "main:x", `C:\x`} {
		if _, err := loadPRTemplate(sctx.Ctx, sctx.WorkDir, trusted, name); err == nil {
			t.Errorf("invalid or missing path %q accepted", name)
		}
	}
}

func TestPRTemplateRejectsUnsafePinnedFiles(t *testing.T) {
	t.Parallel()
	dir, _, head := setupGitRepo(t)
	files := map[string][]byte{
		"empty.md": {}, "blank.md": []byte(" \n\t"), "binary.md": {0xff, 0xfe},
		"nul.md": []byte("a\x00b"), "large.md": []byte(strings.Repeat("x", maxPRTemplateBytes+1)),
		"marker.md": []byte(pipelineAttestationCommentPrefix + `{"head_sha":"x"} -->`),
		"owner.md":  []byte(prAppendixEnd), "link.md": []byte("outside.md"),
		"[literal].md": []byte("## Literal path\n"),
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitCmd(t, dir, "add", ".")
	// Install symlink/submodule tree modes without OS symlink privileges.
	blob := gitCmd(t, dir, "hash-object", "link.md")
	gitCmd(t, dir, "update-index", "--cacheinfo", "120000,"+blob+",link.md")
	gitCmd(t, dir, "update-index", "--add", "--cacheinfo", "160000,"+head+",module")
	gitCmd(t, dir, "commit", "-m", "unsafe template fixtures")
	pin := gitCmd(t, dir, "rev-parse", "HEAD")
	for name := range files {
		if name == "[literal].md" {
			continue
		}
		if _, err := loadPRTemplate(context.Background(), dir, pin, name); err == nil {
			t.Errorf("unsafe %s accepted", name)
		}
	}
	for _, name := range []string{"module", "module/file.md", "link.md/child.md"} {
		if _, err := loadPRTemplate(context.Background(), dir, pin, name); err == nil {
			t.Errorf("unsafe %s accepted", name)
		}
	}
	if got, err := loadPRTemplate(context.Background(), dir, pin, "[literal].md"); err != nil || got != "## Literal path\n" {
		t.Fatalf("literal path treated as glob: %q, %v", got, err)
	}
}

func TestPRTemplateCreateThroughFakeGitHubAndReadback(t *testing.T) {
	t.Parallel()
	sctx, ag, _ := templateTestContext(t)
	no := false
	sctx.Config.PR.PublishIntent = &no
	sctx.UserIntent = "Complete reviewer context stays available."
	env, _ := fakeGH(t, "")
	bodyFile := filepath.Join(t.TempDir(), "body.md")
	sctx.Env = append(env, "FAKE_CLI_PR_BODY_FILE="+bodyFile)
	out, err := (&PRStep{}).Execute(sctx)
	if err != nil || out == nil || out.PRURL == "" {
		t.Fatalf("create: %+v, %v", out, err)
	}
	body, err := os.ReadFile(bodyFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(body), filledPRTemplate) || strings.Contains(string(body), "## Intent") || strings.Contains(string(body), "## What Changed") {
		t.Fatalf("wrong custom narrative/publication: %s", body)
	}
	parts, err := parsePROwnedBody(string(body))
	if err != nil || !parts.managed || !strings.Contains(parts.before, "## Testing") {
		t.Fatalf("author Testing heading was stripped or ownership missing: %+v, %v", parts, err)
	}
	if got := parsePipelineAttestationForTest(t, string(body)).HeadSHA; got != sctx.Run.HeadSHA {
		t.Fatalf("head = %q, want %s", got, sctx.Run.HeadSHA)
	}
	if len(ag.calls) != 1 || !strings.Contains(ag.calls[0].Prompt, "Complete reviewer context") || strings.Contains(ag.calls[0].Prompt, "CONTRIBUTOR TEMPLATE") {
		t.Fatal("template trust/public-private audience boundary lost")
	}
	if sctx.UserIntent != "Complete reviewer context stays available." {
		t.Fatal("publication setting changed reviewer intent")
	}
}

func TestPRTemplateRegenerationPreservesAuthorsAndClosingReferences(t *testing.T) {
	t.Parallel()
	sctx, ag, _ := templateTestContext(t)
	// An existing author body without old generated attestation can be adopted
	// without a model rewrite. Reserved-looking headings are not ownership.
	author := "## Overview\n\nHuman account.\n\n## Tests\n\n- [x] Maintainer approves rollout\n\nCloses https://github.com/test/repo/issues/7\n"
	bodyFile := filepath.Join(t.TempDir(), "body.md")
	if err := os.WriteFile(bodyFile, []byte(author), 0o644); err != nil {
		t.Fatal(err)
	}
	env, logFile := fakeGH(t, "https://github.com/test/repo/pull/42")
	sctx.Env = append(env, "FAKE_CLI_PR_BODY_FILE="+bodyFile)
	step := &PRStep{}
	if _, err := step.Execute(sctx); err != nil {
		t.Fatal(err)
	}
	first, _ := os.ReadFile(bodyFile)
	if !strings.HasPrefix(string(first), author) || len(ag.calls) != 0 {
		t.Fatalf("author narrative re-drafted: calls=%d, body=%s", len(ag.calls), first)
	}
	// Template removal must not make an already owned PR destructive again.
	sctx.Config.PR.Template = ""
	later := strings.Replace(string(first), "Human account.", "Human edited account.", 1) + "\n\nFixes test/other#9\n"
	if err := os.WriteFile(bodyFile, []byte(later), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := step.Execute(sctx); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(bodyFile)
	if string(got) != later || len(ag.calls) != 0 {
		t.Fatalf("regeneration changed author content: %s", got)
	}
	logs, _ := os.ReadFile(logFile)
	for _, line := range strings.Split(string(logs), "\n") {
		if strings.Contains(line, "pr edit") && strings.Contains(line, "--title") {
			t.Fatal("template update must not replace an author's title")
		}
	}
}

func TestPRTemplateDraftFailureDoesNotFallBackOrPublish(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"agent-error", "missing", "nested-json", "missing-heading", "heading", "ownership", "fenced", "oversized"} {
		t.Run(mode, func(t *testing.T) {
			sctx, ag, _ := templateTestContext(t)
			ag.runFn = func(context.Context, agent.RunOpts) (*agent.Result, error) {
				body := filledPRTemplate
				switch mode {
				case "agent-error":
					return nil, errors.New("agent unavailable")
				case "missing":
					return &agent.Result{}, nil
				case "nested-json":
					body = `{"body":"## Overview"}`
				case "missing-heading":
					body = strings.ReplaceAll(body, "# Overview", "")
				case "heading":
					body = strings.ReplaceAll(body, "# Overview", "# Summary")
				case "ownership":
					body += prAppendixEnd
				case "fenced":
					body = "```markdown\n" + body + "\n```"
				case "oversized":
					body += strings.Repeat("x", maxPullRequestBodyBytes)
				}
				data, _ := json.Marshal(prContent{Title: "feat: change", Body: body})
				return &agent.Result{Output: data}, nil
			}
			env, logFile := fakeGH(t, "")
			sctx.Env = env
			if _, err := (&PRStep{}).Execute(sctx); err == nil {
				t.Fatal("untrusted template output published")
			}
			logs, _ := os.ReadFile(logFile)
			if strings.Contains(string(logs), "pr create") || strings.Contains(string(logs), "pr edit") {
				t.Fatalf("publication after invalid template output: %s", logs)
			}
		})
	}
}

func TestPRTemplateStructureAllowsTaskEdits(t *testing.T) {
	t.Parallel()
	for _, line := range []string{"- [ ]", "*\t[ ] Approval", "+   [x] Approval", "1. [ ] Approval", "2) [X] Approval"} {
		template := "## Overview\n\n" + line + "\n"
		body := "## Overview\n\nFilled narrative.\n\n" + line + "\n"
		if err := validateTemplateStructure(template, body); err != nil {
			t.Errorf("unchanged checklist %q rejected: %v", line, err)
		}
		changed := strings.ReplaceAll(body, "[ ]", "[x]")
		if changed == body {
			changed = strings.ReplaceAll(strings.ReplaceAll(body, "[x]", "[ ]"), "[X]", "[ ]")
		}
		if err := validateTemplateStructure(template, changed); err != nil {
			t.Errorf("changed checklist state %q rejected: %v", line, err)
		}
		if err := validateTemplateStructure(template, "Filled narrative."); err != nil {
			t.Errorf("omitted checklist/subheading %q rejected: %v", line, err)
		}
	}
}

func TestPRTemplateUpdateErrorIsNotMaskedByLegacyWarning(t *testing.T) {
	t.Parallel()
	sctx, _, _ := templateTestContext(t)
	env, logFile := fakeGH(t, "https://github.com/test/repo/pull/42")
	sctx.Env = append(env, "FAKE_CLI_PR_BODY=## Human description", "FAKE_CLI_PR_EDIT_ERR=permission denied")
	out, err := (&PRStep{}).Execute(sctx)
	if err == nil || out != nil || !strings.Contains(err.Error(), "update templated PR") {
		t.Fatalf("write failure became a clean success: %+v, %v", out, err)
	}
	run, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil || run.PRURL != nil {
		t.Fatalf("failed first attachment recorded as success: %+v, %v", run, err)
	}
	logs, _ := os.ReadFile(logFile)
	if strings.Count(string(logs), "pr edit") != 1 || strings.Contains(string(logs), "pr create") {
		t.Fatalf("uncertain write retried or duplicated: %s", logs)
	}
}

func TestPRTemplateBarePinnedReadsUnderExplicitBarePolicy(t *testing.T) {
	// Environment injection is intentionally nonparallel and temp-only.
	sctx, _, pin := templateTestContext(t)
	bare := filepath.Join(t.TempDir(), "gate.git")
	gitCmd(t, sctx.WorkDir, "clone", "--bare", sctx.WorkDir, bare)
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "safe.bareRepository")
	t.Setenv("GIT_CONFIG_VALUE_0", "explicit")
	got, err := loadPRTemplate(context.Background(), bare, pin, sctx.Config.PR.Template)
	if err != nil || got != testPRTemplate {
		t.Fatalf("bare pinned template = %q, %v", got, err)
	}
}

func TestPRTemplateUnsupportedProviderIsExplicit(t *testing.T) {
	t.Parallel()
	sctx, ag, _ := templateTestContext(t)
	for _, provider := range []scm.Provider{scm.ProviderGitLab, scm.ProviderBitbucket, scm.ProviderAzureDevOps, scm.ProviderUnknown} {
		if _, err := (&PRStep{}).buildPRContent(sctx, "feature", "main", sctx.Run.BaseSHA, provider, 4000); err == nil {
			t.Errorf("provider %v silently accepted template preservation", provider)
		}
	}
	if len(ag.calls) != 0 {
		t.Fatal("unsupported template launched an agent")
	}
}
