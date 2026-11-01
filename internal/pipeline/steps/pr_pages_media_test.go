package steps

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const testPagesDeployment = "https://5d4c3b2a.nm-evidence.pages.dev"

type wranglerCall struct {
	Args        []string          `json:"args"`
	Dir         string            `json:"dir"`
	Credentials bool              `json:"credentials"`
	Metrics     string            `json:"metrics"`
	Files       map[string]string `json:"files"`
}

type pagesFixture struct {
	log  string
	vars map[string]string
}

func newPagesFixture(t *testing.T) *pagesFixture {
	t.Helper()
	return &pagesFixture{
		log: filepath.Join(t.TempDir(), "wrangler.log"),
		vars: map[string]string{
			"CLOUDFLARE_API_TOKEN":  "cf-token-example",
			"CLOUDFLARE_ACCOUNT_ID": "0123456789abcdef0123456789abcdef",
			"FAKE_WRANGLER_URL":     testPagesDeployment,
		},
	}
}

func (f *pagesFixture) install(t *testing.T, ctx *testPRAttachCtx, project string) {
	t.Helper()
	binDir := fakeCLIBinDir(t)
	linkTestBinary(t, binDir, "wrangler")
	vars := map[string]string{"FAKE_WRANGLER_LOG": f.log}
	for k, v := range f.vars {
		vars[k] = v
	}
	ctx.Env = fakeCLIEnv(binDir, vars)
	ctx.Config.Test.Evidence.CloudflarePagesProject = project
	ctx.provider = scm.ProviderGitHub
}

func (f *pagesFixture) calls(t *testing.T) []wranglerCall {
	t.Helper()
	data, err := os.ReadFile(f.log)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var calls []wranglerCall
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	scanner.Buffer(nil, 64<<20)
	for scanner.Scan() {
		var call wranglerCall
		if err := json.Unmarshal(scanner.Bytes(), &call); err != nil {
			t.Fatal(err)
		}
		calls = append(calls, call)
	}
	return calls
}

func pagesPathFor(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])[:16] + "/checkout.png"
}

func TestPRStep_CloudflarePagesScreenshotEmbedsTheDeploymentURLNotAUserAttachment(t *testing.T) {
	t.Parallel()
	pages := newPagesFixture(t)
	uploader := &stubMediaUploader{t: t}
	var head string
	body, _ := renderPRWithScreenshot(t, uploader, func(ctx *testPRAttachCtx) {
		pages.install(t, ctx, "nm-evidence")
		head = ctx.Run.HeadSHA
	})
	want := "![Checkout screenshot](" + testPagesDeployment + "/" + pagesPathFor("png-bytes") + ")"
	if !strings.Contains(body, want) {
		t.Fatalf("expected %s in body:\n%s", want, body)
	}
	if len(uploader.calls) != 0 {
		t.Fatalf("GitHub uploads = %v, want none once Pages published the file", uploader.calls)
	}
	calls := pages.calls(t)
	if len(calls) != 1 {
		t.Fatalf("wrangler calls = %d, want 1", len(calls))
	}
	call := calls[0]
	for _, flag := range []string{"--project-name=nm-evidence", "--branch=no-mistakes-evidence", "--commit-hash=" + head} {
		if !slices.Contains(call.Args, flag) {
			t.Fatalf("wrangler args %v lack %s", call.Args, flag)
		}
	}
	if !call.Credentials || call.Metrics != "false" {
		t.Fatalf("credentials=%t metrics=%q, want credentials passed and metrics off", call.Credentials, call.Metrics)
	}
	if got := call.Files[pagesPathFor("png-bytes")]; got != "png-bytes" {
		t.Fatalf("deployed screenshot = %q, want the evidence bytes; files %v", got, call.Files)
	}
	if !strings.Contains(call.Files["_headers"], "X-Robots-Tag: noindex") {
		t.Fatalf("_headers = %q, want noindex", call.Files["_headers"])
	}
	if len(call.Files) != 2 {
		t.Fatalf("deployed files = %v, want only the screenshot and _headers", call.Files)
	}
}

func TestPRStep_CloudflarePagesMissingCredentialFallsBackToUserAttachments(t *testing.T) {
	t.Parallel()
	pages := newPagesFixture(t)
	pages.vars["CLOUDFLARE_API_TOKEN"] = ""
	uploader := &stubMediaUploader{t: t, urls: map[string]string{"checkout.png": testAttachmentURL}}
	body, logs := renderPRWithScreenshot(t, uploader, func(ctx *testPRAttachCtx) {
		pages.install(t, ctx, "nm-evidence")
	})
	if !strings.Contains(body, "![Checkout screenshot]("+testAttachmentURL+")") {
		t.Fatalf("expected the user-attachments fallback, got:\n%s", body)
	}
	if !strings.Contains(logs, "CLOUDFLARE_API_TOKEN not set") {
		t.Fatalf("logs do not name the missing credential:\n%s", logs)
	}
	if calls := pages.calls(t); len(calls) != 0 {
		t.Fatalf("wrangler ran without credentials: %+v", calls)
	}
}

func TestPRStep_CloudflarePagesDeployFailureFallsBackToUserAttachments(t *testing.T) {
	t.Parallel()
	pages := newPagesFixture(t)
	pages.vars["FAKE_WRANGLER_FAIL"] = "Authentication error [code: 10000]"
	uploader := &stubMediaUploader{t: t, urls: map[string]string{"checkout.png": testAttachmentURL}}
	body, logs := renderPRWithScreenshot(t, uploader, func(ctx *testPRAttachCtx) {
		pages.install(t, ctx, "nm-evidence")
	})
	if !strings.Contains(body, "![Checkout screenshot]("+testAttachmentURL+")") {
		t.Fatalf("expected the user-attachments fallback, got:\n%s", body)
	}
	if strings.Contains(body, "pages.dev") {
		t.Fatalf("failed deploy left a pages.dev link:\n%s", body)
	}
	if !strings.Contains(logs, "Authentication error [code: 10000]") {
		t.Fatalf("logs do not carry wrangler's error:\n%s", logs)
	}
}

func TestPRStep_CloudflarePagesSecondRenderDoesNotRedeploy(t *testing.T) {
	t.Parallel()
	pages := newPagesFixture(t)
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, prDraftAgent(), dir, baseSHA, headSHA, config.Commands{})
	enableDefaultEvidence(sctx)
	png := writeEvidenceFile(t, sctx.EvidenceDir, "checkout.png", []byte("png-bytes"))
	insertCompletedStep(t, sctx, types.StepTest, screenshotFindings(png), "")
	ctx := &testPRAttachCtx{StepContext: sctx, png: png}
	pages.install(t, ctx, "nm-evidence")
	step := &PRStep{mediaUploader: &stubMediaUploader{t: t}}
	want := testPagesDeployment + "/" + pagesPathFor("png-bytes")
	for i := 0; i < 2; i++ {
		content, err := step.buildPRContent(sctx, "feature", "main", baseSHA, scm.ProviderGitHub, 0)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(content.Body, want) {
			t.Fatalf("render %d lacks %s:\n%s", i+1, want, content.Body)
		}
	}
	if calls := pages.calls(t); len(calls) != 1 {
		t.Fatalf("wrangler calls = %d, want 1", len(calls))
	}
}

func TestPRStep_CloudflarePagesHonoursTheAttachMediaOptOut(t *testing.T) {
	t.Parallel()
	pages := newPagesFixture(t)
	uploader := &stubMediaUploader{t: t}
	body, _ := renderPRWithScreenshot(t, uploader, func(ctx *testPRAttachCtx) {
		pages.install(t, ctx, "nm-evidence")
		ctx.Config.Test.Evidence.AttachMedia = false
	})
	if strings.Contains(body, "pages.dev") {
		t.Fatalf("attach_media: false still published to Pages:\n%s", body)
	}
	if calls := pages.calls(t); len(calls) != 0 {
		t.Fatalf("wrangler ran under attach_media: false: %+v", calls)
	}
}

func TestPRStep_CloudflarePagesPublishesOnAForgeWithoutUserAttachments(t *testing.T) {
	t.Parallel()
	pages := newPagesFixture(t)
	body, _ := renderPRWithScreenshot(t, nil, func(ctx *testPRAttachCtx) {
		pages.install(t, ctx, "nm-evidence")
		ctx.provider = scm.ProviderGitLab
	})
	if !strings.Contains(body, testPagesDeployment+"/"+pagesPathFor("png-bytes")) {
		t.Fatalf("expected the Pages link on GitLab, got:\n%s", body)
	}
}

func TestPRStep_CloudflarePagesSkipsANonMediaFileTheKindCallsAScreenshot(t *testing.T) {
	t.Parallel()
	pages := newPagesFixture(t)
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, prDraftAgent(), dir, baseSHA, headSHA, config.Commands{})
	enableDefaultEvidence(sctx)
	secret := writeEvidenceFile(t, sctx.EvidenceDir, "session.har", []byte("cookie: secret"))
	insertCompletedStep(t, sctx, types.StepTest, screenshotFindings(secret), "")
	ctx := &testPRAttachCtx{StepContext: sctx}
	pages.install(t, ctx, "nm-evidence")
	if _, err := (&PRStep{mediaUploader: &stubMediaUploader{}}).buildPRContent(sctx, "feature", "main", baseSHA, scm.ProviderGitHub, 0); err != nil {
		t.Fatal(err)
	}
	if calls := pages.calls(t); len(calls) != 0 {
		t.Fatalf("a .har screenshot was deployed: %+v", calls)
	}
}
