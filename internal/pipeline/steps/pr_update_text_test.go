package steps

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const (
	keptAuthorTitle  = "Detect plaintext and keyring-backed Cloudflare cf profiles"
	keptAuthorBody   = "How it was tested: local gate run.\n\nRefs #414\n"
	keptRiskJSON     = `{"risk_level":"low","risk_rationale":"gate is local"}`
	keptRiskLine     = "✅ Low: gate is local"
	keptNextRisk     = `{"risk_level":"low","risk_rationale":"head moved"}`
	keptNextRiskLine = "✅ Low: head moved"
)

func TestPRUpdateTextKeepOnUpdate(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{config.PRAppendixFull, config.PRAppendixMinimal} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			dir, baseSHA, headSHA := setupGitRepo(t)
			env, logFile := fakeGH(t, "https://github.com/test/repo/pull/42")
			bodyFile := envEntry(env, "FAKE_CLI_PR_BODY_FILE")
			if err := os.WriteFile(bodyFile, []byte(keptAuthorBody), 0o644); err != nil {
				t.Fatal(err)
			}
			ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
				t.Fatal("keep update drafted a replacement")
				return nil, nil
			}}
			sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
			sctx.Env = append(env, "FAKE_CLI_PR_TITLE="+keptAuthorTitle)
			sctx.Config.PR.UpdateText = config.PRUpdateTextKeep
			sctx.Config.PR.Appendix = mode
			sr, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepReview)
			if err != nil {
				t.Fatal(err)
			}
			if err := sctx.DB.UpdateStepStatus(sr.ID, types.StepStatusCompleted); err != nil {
				t.Fatal(err)
			}
			if err := sctx.DB.SetStepFindings(sr.ID, keptRiskJSON); err != nil {
				t.Fatal(err)
			}

			if _, err := (&PRStep{}).Execute(sctx); err != nil {
				t.Fatal(err)
			}
			body := string(mustRead(t, bodyFile))
			if !strings.HasPrefix(body, keptAuthorBody) {
				t.Fatalf("author narrative = %q", body)
			}
			if publishedAttestationHead(t, body) != headSHA {
				t.Fatalf("attestation head = %s, want %s", publishedAttestationHead(t, body), headSHA)
			}
			assertKeptEvidence(t, body, mode, keptRiskLine)
			if strings.Count(body, "<!-- no-mistakes-pipeline-attestation:v1 ") != 1 {
				t.Fatalf("attestation count:\n%s", body)
			}
			logs := string(mustRead(t, logFile))
			if strings.Contains(logs, "stdin --title") || len(ag.calls) != 0 {
				t.Fatalf("title or narrative was redrafted: calls=%d\n%s", len(ag.calls), logs)
			}

			if err := sctx.DB.SetStepFindings(sr.ID, keptNextRisk); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "next.txt"), []byte("next\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			gitCmd(t, dir, "add", "next.txt")
			gitCmd(t, dir, "commit", "-m", "move the head")
			sctx.Run.HeadSHA = gitCmd(t, dir, "rev-parse", "HEAD")
			if _, err := (&PRStep{}).Execute(sctx); err != nil {
				t.Fatal(err)
			}
			body = string(mustRead(t, bodyFile))
			if !strings.HasPrefix(body, keptAuthorBody) {
				t.Fatalf("second update rewrote the narrative: %q", body)
			}
			if strings.Contains(body, "gate is local") {
				t.Fatalf("stale risk line survived:\n%s", body)
			}
			if publishedAttestationHead(t, body) != sctx.Run.HeadSHA || sctx.Run.HeadSHA == headSHA {
				t.Fatalf("refreshed head = %s, recorded %s, previous %s", publishedAttestationHead(t, body), sctx.Run.HeadSHA, headSHA)
			}
			assertKeptEvidence(t, body, mode, keptNextRiskLine)
			logs = string(mustRead(t, logFile))
			if strings.Contains(logs, "stdin --title") || len(ag.calls) != 0 {
				t.Fatalf("second update redrafted title or narrative: calls=%d\n%s", len(ag.calls), logs)
			}
		})
	}
}

func TestPRUpdateTextKeepOnCreate(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	env, logFile := fakeGH(t, "")
	const (
		title   = "feat: detect plaintext profiles"
		drafted = "## What Changed\n\n- detect plaintext profiles"
	)
	ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		data, _ := json.Marshal(prContent{Title: title, Body: drafted})
		return &agent.Result{Output: data}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Config.PR.UpdateText = config.PRUpdateTextKeep
	sr, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.UpdateStepStatus(sr.ID, types.StepStatusCompleted); err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.SetStepFindings(sr.ID, keptRiskJSON); err != nil {
		t.Fatal(err)
	}

	out, err := (&PRStep{}).Execute(sctx)
	if err != nil || out == nil || out.PRURL != "https://github.com/test/repo/pull/99" {
		t.Fatalf("create: %+v, %v", out, err)
	}
	body := string(mustRead(t, envEntry(env, "FAKE_CLI_PR_BODY_FILE")))
	if !strings.HasPrefix(body, drafted) {
		t.Fatalf("created narrative = %q", body)
	}
	if publishedAttestationHead(t, body) != headSHA {
		t.Fatalf("created attestation head = %s, want %s", publishedAttestationHead(t, body), headSHA)
	}
	assertKeptEvidence(t, body, config.PRAppendixFull, keptRiskLine)
	logs := string(mustRead(t, logFile))
	if !strings.Contains(logs, "pr create") || !strings.Contains(logs, "--title "+title) || len(ag.calls) != 1 {
		t.Fatalf("create draft: calls=%d\n%s", len(ag.calls), logs)
	}
}

func TestPRUpdateTextKeepCreateThenUpdatePublishesOneIntent(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	createEnv, _ := fakeGH(t, "")
	ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		data, _ := json.Marshal(prContent{Title: "feat: x", Body: "## What Changed\n\n- x"})
		return &agent.Result{Output: data}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = createEnv
	sctx.Config.PR.UpdateText = config.PRUpdateTextKeep
	sctx.UserIntent = "Ship the intent exactly once."
	sr, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.UpdateStepStatus(sr.ID, types.StepStatusCompleted); err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.SetStepFindings(sr.ID, keptRiskJSON); err != nil {
		t.Fatal(err)
	}
	if _, err := (&PRStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	created := string(mustRead(t, envEntry(createEnv, "FAKE_CLI_PR_BODY_FILE")))
	if n := strings.Count(created, "## Intent"); n != 1 {
		t.Fatalf("created body has %d Intent sections:\n%s", n, created)
	}

	updateEnv, _ := fakeGH(t, "https://github.com/test/repo/pull/42")
	bodyFile := envEntry(updateEnv, "FAKE_CLI_PR_BODY_FILE")
	if err := os.WriteFile(bodyFile, []byte(created), 0o644); err != nil {
		t.Fatal(err)
	}
	sctx.Env = updateEnv
	if _, err := (&PRStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	updated := string(mustRead(t, bodyFile))
	if n := strings.Count(updated, "## Intent"); n != 1 {
		t.Fatalf("updated body has %d Intent sections:\n%s", n, updated)
	}
}

func TestPRUpdateTextKeepCreateRefusesRewrittenBody(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	env, _ := fakeGH(t, "")
	ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		data, _ := json.Marshal(prContent{Title: "feat: x", Body: "## What Changed\n\n- x"})
		return &agent.Result{Output: data}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = append(env, "FAKE_CLI_PR_CREATE_REWRITE_BODY=1")
	sctx.Config.PR.UpdateText = config.PRUpdateTextKeep
	sr, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.UpdateStepStatus(sr.ID, types.StepStatusCompleted); err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.SetStepFindings(sr.ID, keptRiskJSON); err != nil {
		t.Fatal(err)
	}
	_, err = (&PRStep{}).Execute(sctx)
	if err == nil || !strings.Contains(err.Error(), "created PR body differs") || !strings.Contains(err.Error(), "pr.update_text: keep") {
		t.Fatalf("error = %v", err)
	}
}

func TestPRUpdateTextKeepCreateRefusesUnreadableIdentity(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	env, _ := fakeGH(t, "")
	ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		data, _ := json.Marshal(prContent{Title: "feat: x", Body: "## What Changed\n\n- x"})
		return &agent.Result{Output: data}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = append(env, "FAKE_CLI_PR_CREATE_EMPTY=1")
	sctx.Config.PR.UpdateText = config.PRUpdateTextKeep
	sr, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.UpdateStepStatus(sr.ID, types.StepStatusCompleted); err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.SetStepFindings(sr.ID, keptRiskJSON); err != nil {
		t.Fatal(err)
	}
	_, err = (&PRStep{}).Execute(sctx)
	if err == nil || !strings.Contains(err.Error(), "no readable PR identity") || !strings.Contains(err.Error(), "pr.update_text: keep") {
		t.Fatalf("error = %v", err)
	}
}

func TestPRUpdateTextRegenerateRewrites(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"", config.PRUpdateTextRegenerate} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			dir, baseSHA, headSHA := setupGitRepo(t)
			env, logFile := fakeGH(t, "https://github.com/test/repo/pull/42")
			bodyFile := envEntry(env, "FAKE_CLI_PR_BODY_FILE")
			if err := os.WriteFile(bodyFile, []byte(keptAuthorBody), 0o644); err != nil {
				t.Fatal(err)
			}
			const (
				title   = "feat(isotopes): detect plaintext profiles"
				drafted = "## What Changed\n\n- replace the author narrative"
			)
			ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
				data, _ := json.Marshal(prContent{Title: title, Body: drafted})
				return &agent.Result{Output: data}, nil
			}}
			sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
			sctx.Env = append(env, "FAKE_CLI_PR_TITLE="+keptAuthorTitle)
			sctx.Config.PR.UpdateText = mode
			sr, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepReview)
			if err != nil {
				t.Fatal(err)
			}
			if err := sctx.DB.UpdateStepStatus(sr.ID, types.StepStatusCompleted); err != nil {
				t.Fatal(err)
			}

			if _, err := (&PRStep{}).Execute(sctx); err != nil {
				t.Fatal(err)
			}
			body := string(mustRead(t, bodyFile))
			if strings.Contains(body, "Refs #414") || !strings.Contains(body, "- replace the author narrative") {
				t.Fatalf("regenerate body = %q", body)
			}
			if strings.Contains(body, "no-mistakes-pr-appendix") {
				t.Fatalf("regenerate published an owned appendix:\n%s", body)
			}
			logs := string(mustRead(t, logFile))
			if !strings.Contains(logs, "stdin --title "+title) || len(ag.calls) != 1 {
				t.Fatalf("regenerate title: calls=%d\n%s", len(ag.calls), logs)
			}
		})
	}
}

func TestPRUpdateTextKeepRefusesUnownedAttestation(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	env, logFile := fakeGH(t, "https://github.com/test/repo/pull/42")
	bodyFile := envEntry(env, "FAKE_CLI_PR_BODY_FILE")
	legacy := "## What Changed\n\n- old narrative\n\n<!-- no-mistakes-pipeline-attestation:v1 {\"head_sha\":\"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\",\"steps\":[]} -->\n"
	if err := os.WriteFile(bodyFile, []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Config.PR.UpdateText = config.PRUpdateTextKeep
	_, err := (&PRStep{}).Execute(sctx)
	if err == nil || !strings.Contains(err.Error(), "unowned legacy attestation") || !strings.Contains(err.Error(), "pr.update_text") || strings.Contains(err.Error(), "pr.template") {
		t.Fatalf("error = %v", err)
	}
	if got := string(mustRead(t, bodyFile)); got != legacy {
		t.Fatalf("legacy body changed: %q", got)
	}
	logs := string(mustRead(t, logFile))
	if strings.Contains(logs, "--method PATCH") || strings.Contains(logs, "pr create") || len(ag.calls) != 0 {
		t.Fatalf("refusal published: calls=%d\n%s", len(ag.calls), logs)
	}
}

func assertKeptEvidence(t *testing.T, body, mode, riskLine string) {
	t.Helper()
	if strings.Count(body, "no-mistakes-pr-appendix") != 2 {
		t.Fatalf("owned appendix markers = %d\n%s", strings.Count(body, "no-mistakes-pr-appendix"), body)
	}
	start := strings.Index(body, "<!-- no-mistakes-pr-appendix:v1 sha256=")
	end := strings.Index(body, "<!-- /no-mistakes-pr-appendix:v1 -->")
	if start < 0 || end < start {
		t.Fatalf("appendix bounds missing:\n%s", body)
	}
	appendix := body[start:end]
	if !strings.Contains(appendix, riskLine) || !strings.Contains(appendix, noMistakesPRSignature) {
		t.Fatalf("appendix evidence = %q", appendix)
	}
	if mode == config.PRAppendixMinimal {
		for _, heading := range []string{"## Risk Assessment", "## Testing", "## Pipeline"} {
			if strings.Contains(appendix, heading) {
				t.Fatalf("minimal appendix contains %s:\n%s", heading, appendix)
			}
		}
	} else if !strings.Contains(appendix, "## Risk Assessment\n\n"+riskLine) || !strings.Contains(appendix, "## Pipeline") {
		t.Fatalf("full appendix = %q", appendix)
	}
}

func publishedAttestationHead(t *testing.T, body string) string {
	t.Helper()
	const prefix = "<!-- no-mistakes-pipeline-attestation:v1 "
	start := strings.Index(body, prefix)
	if start < 0 {
		t.Fatalf("body has no attestation:\n%s", body)
	}
	rest := body[start+len(prefix):]
	end := strings.Index(rest, " -->")
	if end < 0 {
		t.Fatal("attestation comment does not close")
	}
	var payload struct {
		HeadSHA string `json:"head_sha"`
	}
	if err := json.Unmarshal([]byte(rest[:end]), &payload); err != nil {
		t.Fatalf("attestation json: %v\n%s", err, rest[:end])
	}
	return payload.HeadSHA
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
