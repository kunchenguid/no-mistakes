package steps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
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
			sctx := newPRTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
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
			recordPRTestPush(t, sctx)
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
	sctx := newPRTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
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
	sctx := newPRTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
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
	sctx := newPRTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
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
	sctx := newPRTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
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
			sctx := newPRTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
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
	sctx := newPRTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
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

// Keep mode with no template owns the created body's appendix, so the public
// Intent section must survive whichever drafting path produced the narrative.
func TestPRUpdateTextKeepOnCreatePublishesIntentOnEveryDraftPath(t *testing.T) {
	t.Parallel()
	const intent = "Keep the published intent on every path."
	for name, run := range map[string]func(context.Context, agent.RunOpts) (*agent.Result, error){
		"valid draft": func(context.Context, agent.RunOpts) (*agent.Result, error) {
			data, _ := json.Marshal(prContent{Title: "feat: x", Body: "## What Changed\n\n- x"})
			return &agent.Result{Output: data}, nil
		},
		"agent error": func(context.Context, agent.RunOpts) (*agent.Result, error) {
			return nil, errors.New("agent crashed")
		},
		"invalid draft": func(context.Context, agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(`{"title":"","body":""}`)}, nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir, baseSHA, headSHA := setupGitRepo(t)
			env, _ := fakeGH(t, "")
			sctx := newPRTestContext(t, &mockAgent{name: "test", runFn: run}, dir, baseSHA, headSHA, config.Commands{})
			sctx.Env = env
			sctx.Config.PR.UpdateText = config.PRUpdateTextKeep
			sctx.UserIntent = intent
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
			body := string(mustRead(t, envEntry(env, "FAKE_CLI_PR_BODY_FILE")))
			if n := strings.Count(body, "## Intent\n\n"+intent); n != 1 {
				t.Fatalf("created body has %d Intent sections:\n%s", n, body)
			}
			assertKeptEvidence(t, body, config.PRAppendixFull, keptRiskLine)
		})
	}
}

const (
	suppliedPRTitleText = "Detect plaintext profiles"
	suppliedPRBodyText  = "Detects plaintext cf profiles.\n\n## Testing\n\nRan it against a keyring profile by hand.\n"
)

// suppliedPRContext is a create-path PR step whose run carries supplied text.
func suppliedPRContext(t *testing.T, ag *mockAgent, title, body string, branches ...string) (*pipeline.StepContext, []string, string) {
	t.Helper()
	dir, baseSHA, headSHA := setupGitRepo(t)
	env, logFile := fakeGH(t, "")
	sctx := newPRTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{}, branches...)
	sctx.Env = env
	if title != "" {
		sctx.Run.PRTitle = &title
	}
	if body != "" {
		sctx.Run.PRBody = &body
	}
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
	return sctx, env, logFile
}

func noDraftAgent(t *testing.T) *mockAgent {
	return &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		t.Fatal("supplied PR text was redrafted")
		return nil, nil
	}}
}

func TestPRSuppliedTextCreatesOwnedBody(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{config.PRAppendixFull, config.PRAppendixCollapsed, config.PRAppendixMinimal} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			ag := noDraftAgent(t)
			sctx, env, logFile := suppliedPRContext(t, ag, suppliedPRTitleText, suppliedPRBodyText)
			sctx.Config.PR.Appendix = mode
			out, err := (&PRStep{}).Execute(sctx)
			if err != nil || out == nil || out.PRURL == "" {
				t.Fatalf("create: %+v, %v", out, err)
			}
			body := string(mustRead(t, envEntry(env, "FAKE_CLI_PR_BODY_FILE")))
			if !strings.HasPrefix(body, suppliedPRBodyText+"\n\n<!-- no-mistakes-pr-appendix:v1 ") {
				t.Fatalf("supplied narrative was not published verbatim above the appendix:\n%s", body)
			}
			parts, err := parsePROwnedBody(body)
			if err != nil || !parts.managed || !strings.Contains(parts.before, "## Testing\n\nRan it against") {
				t.Fatalf("supplied body is not owned, or its Testing heading was stripped: %+v, %v", parts, err)
			}
			assertKeptEvidence(t, body, mode, keptRiskLine)
			if publishedAttestationHead(t, body) != sctx.Run.HeadSHA {
				t.Fatalf("attestation head = %s", publishedAttestationHead(t, body))
			}
			logs := string(mustRead(t, logFile))
			if !strings.Contains(logs, "--title "+suppliedPRTitleText) || strings.Contains(logs, "chore: ") {
				t.Fatalf("supplied title was not used verbatim:\n%s", logs)
			}
			if len(ag.calls) != 0 {
				t.Fatalf("agent calls = %d", len(ag.calls))
			}
		})
	}
}

func TestPRSuppliedBodySurvivesALaterRegenerateRun(t *testing.T) {
	t.Parallel()
	ag := noDraftAgent(t)
	sctx, env, _ := suppliedPRContext(t, ag, suppliedPRTitleText, suppliedPRBodyText)
	if _, err := (&PRStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	created := string(mustRead(t, envEntry(env, "FAKE_CLI_PR_BODY_FILE")))

	// The next run carries no supplied text and the default regenerate mode.
	updateEnv, logFile := fakeGH(t, "https://github.com/test/repo/pull/42")
	bodyFile := envEntry(updateEnv, "FAKE_CLI_PR_BODY_FILE")
	if err := os.WriteFile(bodyFile, []byte(created), 0o644); err != nil {
		t.Fatal(err)
	}
	sctx.Env = updateEnv
	sctx.Run.PRTitle, sctx.Run.PRBody = nil, nil
	if err := os.WriteFile(filepath.Join(sctx.WorkDir, "next.txt"), []byte("next\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, sctx.WorkDir, "add", "next.txt")
	gitCmd(t, sctx.WorkDir, "commit", "-m", "move the head")
	sctx.Run.HeadSHA = gitCmd(t, sctx.WorkDir, "rev-parse", "HEAD")
	recordPRTestPush(t, sctx)
	if _, err := (&PRStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	updated := string(mustRead(t, bodyFile))
	if !strings.HasPrefix(updated, suppliedPRBodyText+"\n\n") || publishedAttestationHead(t, updated) != sctx.Run.HeadSHA {
		t.Fatalf("later run did not keep the narrative and refresh the attestation:\n%s", updated)
	}
	if logs := string(mustRead(t, logFile)); strings.Contains(logs, "--title") || len(ag.calls) != 0 {
		t.Fatalf("later run redrafted the title: calls=%d\n%s", len(ag.calls), logs)
	}
}

func TestPRInheritedTextIsNotAppliedToAnExistingPR(t *testing.T) {
	t.Parallel()
	const drafted = "## What Changed\n\n- regenerated"
	ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		data, _ := json.Marshal(prContent{Title: "feat: regenerated", Body: drafted})
		return &agent.Result{Output: data}, nil
	}}
	sctx, _, _ := suppliedPRContext(t, ag, suppliedPRTitleText, suppliedPRBodyText)
	env, logFile := fakeGH(t, "https://github.com/test/repo/pull/42")
	bodyFile := envEntry(env, "FAKE_CLI_PR_BODY_FILE")
	if err := os.WriteFile(bodyFile, []byte(keptAuthorBody), 0o644); err != nil {
		t.Fatal(err)
	}
	sctx.Env = append(env, "FAKE_CLI_PR_TITLE="+keptAuthorTitle)
	var logged []string
	sctx.Log = func(s string) { logged = append(logged, s) }
	if _, err := (&PRStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	body := string(mustRead(t, bodyFile))
	if strings.Contains(body, "Detects plaintext cf profiles") || !strings.Contains(body, "- regenerated") {
		t.Fatalf("supplied text reached an existing PR:\n%s", body)
	}
	if logs := string(mustRead(t, logFile)); strings.Contains(logs, suppliedPRTitleText) {
		t.Fatalf("supplied title reached an existing PR:\n%s", logs)
	}
	want := "supplied PR title and description not applied: the pull request already exists, and inherited text does not request an update"
	if !slices.Contains(logged, want) {
		t.Fatalf("step did not say the text went unused: %q", logged)
	}
}

func TestPRFreshSuppliedTextUpdatesExistingPR(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, title, body                              string
		freshTitle, freshBody                          bool
		wantDraft, wantSuppliedBody, wantSuppliedTitle bool
	}{
		{name: "no_flags", wantDraft: true},
		{name: "title_only", title: suppliedPRTitleText, freshTitle: true, wantDraft: true, wantSuppliedTitle: true},
		{name: "body_only", body: suppliedPRBodyText, freshBody: true, wantSuppliedBody: true},
		{name: "both", title: suppliedPRTitleText, body: suppliedPRBodyText, freshTitle: true, freshBody: true, wantSuppliedTitle: true, wantSuppliedBody: true},
		{name: "inherited_both", title: suppliedPRTitleText, body: suppliedPRBodyText, wantDraft: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
				if !tc.wantDraft {
					t.Fatal("supplied body caused drafting")
				}
				data, _ := json.Marshal(prContent{Title: "feat: regenerated", Body: "## What Changed\n\n- regenerated"})
				return &agent.Result{Output: data}, nil
			}}
			sctx, _, _ := suppliedPRContext(t, ag, tc.title, tc.body)
			env, logFile := fakeGH(t, "https://github.com/test/repo/pull/42")
			bodyFile := envEntry(env, "FAKE_CLI_PR_BODY_FILE")
			if err := os.WriteFile(bodyFile, []byte(keptAuthorBody), 0o644); err != nil {
				t.Fatal(err)
			}
			sctx.Env = append(env, "FAKE_CLI_PR_TITLE="+keptAuthorTitle)
			sctx.Run.PRTitleFresh, sctx.Run.PRBodyFresh = tc.freshTitle, tc.freshBody
			if tc.freshBody {
				if err := sctx.DB.UpdateRunClosingIssueRefs(sctx.Run.ID, []string{"95"}); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := (&PRStep{}).Execute(sctx); err != nil {
				t.Fatal(err)
			}
			body := string(mustRead(t, bodyFile))
			if got := strings.HasPrefix(body, suppliedPRBodyText); got != tc.wantSuppliedBody {
				t.Fatalf("supplied narrative used = %v, want %v:\n%s", got, tc.wantSuppliedBody, body)
			}
			if got := strings.Contains(body, "- regenerated"); got != tc.wantDraft {
				t.Fatalf("drafted narrative used = %v, want %v:\n%s", got, tc.wantDraft, body)
			}
			if publishedAttestationHead(t, body) != sctx.Run.HeadSHA {
				t.Fatalf("attestation head mismatch:\n%s", body)
			}
			if tc.wantSuppliedBody {
				assertKeptEvidence(t, body, config.PRAppendixFull, keptRiskLine)
				if !strings.Contains(body, "## Issues\n\nCloses #95") {
					t.Fatalf("requested issue reference missing:\n%s", body)
				}
			}
			logs := string(mustRead(t, logFile))
			if got := strings.Contains(logs, "--title "+suppliedPRTitleText); got != tc.wantSuppliedTitle {
				t.Fatalf("supplied title used = %v, want %v:\n%s", got, tc.wantSuppliedTitle, logs)
			}
			if tc.name == "body_only" && strings.Contains(logs, "--title") {
				t.Fatalf("body-only update changed the title:\n%s", logs)
			}
		})
	}
}

func TestPRFreshDescriptionReplacesLegacyUnownedAttestation(t *testing.T) {
	t.Parallel()
	sctx, _, _ := suppliedPRContext(t, noDraftAgent(t), suppliedPRTitleText, suppliedPRBodyText)
	env, _ := fakeGH(t, "https://github.com/test/repo/pull/42")
	bodyFile := envEntry(env, "FAKE_CLI_PR_BODY_FILE")
	legacy := "## What Changed\n\nOld generated text.\n\n<!-- no-mistakes-pipeline-attestation:v1 {} -->"
	if err := os.WriteFile(bodyFile, []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	sctx.Env = env
	sctx.Run.PRTitleFresh, sctx.Run.PRBodyFresh = true, true
	if _, err := (&PRStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	body := string(mustRead(t, bodyFile))
	if !strings.HasPrefix(body, suppliedPRBodyText) || strings.Contains(body, "Old generated text") {
		t.Fatalf("legacy body was not replaced:\n%s", body)
	}
	if _, err := parsePROwnedBody(body); err != nil {
		t.Fatalf("replacement body lacks a valid owned appendix: %v", err)
	}
}

func TestPRFreshTitleKeepsBodyUnderUpdateTextKeep(t *testing.T) {
	t.Parallel()
	sctx, _, _ := suppliedPRContext(t, noDraftAgent(t), suppliedPRTitleText, "")
	env, logFile := fakeGH(t, "https://github.com/test/repo/pull/42")
	bodyFile := envEntry(env, "FAKE_CLI_PR_BODY_FILE")
	if err := os.WriteFile(bodyFile, []byte(keptAuthorBody), 0o644); err != nil {
		t.Fatal(err)
	}
	sctx.Env = env
	sctx.Config.PR.UpdateText = config.PRUpdateTextKeep
	sctx.Run.PRTitleFresh = true
	if _, err := (&PRStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	body := string(mustRead(t, bodyFile))
	if !strings.HasPrefix(body, keptAuthorBody) || publishedAttestationHead(t, body) != sctx.Run.HeadSHA {
		t.Fatalf("keep body or attestation changed unexpectedly:\n%s", body)
	}
	if logs := string(mustRead(t, logFile)); !strings.Contains(logs, "--title "+suppliedPRTitleText) {
		t.Fatalf("fresh title did not update the PR:\n%s", logs)
	}
}

func TestPRFreshDescriptionOnExistingPRRequiresTemplateHeadings(t *testing.T) {
	t.Parallel()
	sctx, _, _ := templateTestContext(t)
	env, logFile := fakeGH(t, "https://github.com/test/repo/pull/42")
	bodyFile := envEntry(env, "FAKE_CLI_PR_BODY_FILE")
	if err := os.WriteFile(bodyFile, []byte(keptAuthorBody), 0o644); err != nil {
		t.Fatal(err)
	}
	sctx.Env = env
	body := "## Testing\n\nNo overview heading.\n"
	sctx.Run.PRBody = &body
	sctx.Run.PRBodyFresh = true
	_, err := (&PRStep{}).Execute(sctx)
	if err == nil || !strings.Contains(err.Error(), "# Overview") {
		t.Fatalf("template heading refusal = %v", err)
	}
	if got := string(mustRead(t, bodyFile)); got != keptAuthorBody {
		t.Fatalf("invalid description changed live body: %q", got)
	}
	if logs := string(mustRead(t, logFile)); strings.Contains(logs, "pr edit") {
		t.Fatalf("invalid description reached publication:\n%s", logs)
	}
}

func TestPRSuppliedTitleOnlyKeepsTheDraftedBody(t *testing.T) {
	t.Parallel()
	for _, agentFails := range []bool{false, true} {
		t.Run(fmt.Sprintf("agent_fails=%v", agentFails), func(t *testing.T) {
			t.Parallel()
			ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
				if agentFails {
					return nil, errors.New("agent unavailable")
				}
				data, _ := json.Marshal(prContent{Title: "feat: drafted title", Body: "## What Changed\n\n- drafted"})
				return &agent.Result{Output: data}, nil
			}}
			sctx, env, logFile := suppliedPRContext(t, ag, suppliedPRTitleText, "")
			if _, err := (&PRStep{}).Execute(sctx); err != nil {
				t.Fatal(err)
			}
			logs := string(mustRead(t, logFile))
			if !strings.Contains(logs, "--title "+suppliedPRTitleText) || strings.Contains(logs, "drafted title") || strings.Contains(logs, "update pull request") {
				t.Fatalf("supplied title did not replace the drafted one:\n%s", logs)
			}
			body := string(mustRead(t, envEntry(env, "FAKE_CLI_PR_BODY_FILE")))
			if !strings.Contains(body, "## What Changed") || strings.Contains(body, "no-mistakes-pr-appendix") {
				t.Fatalf("a title-only run must keep an ordinary unowned body:\n%s", body)
			}
		})
	}
}

func TestPRSuppliedBodyOnlyDraftsAConventionalTitle(t *testing.T) {
	t.Parallel()
	ag := &mockAgent{name: "test", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		data, _ := json.Marshal(prContent{Title: "detect plaintext profiles"})
		return &agent.Result{Output: data}, nil
	}}
	sctx, env, logFile := suppliedPRContext(t, ag, "", suppliedPRBodyText)
	if _, err := (&PRStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if len(ag.calls) != 1 || !strings.Contains(ag.calls[0].Prompt, "Draft only the bare concise pull request title") || !strings.Contains(ag.calls[0].Prompt, "conventional commit format") {
		t.Fatalf("body-only run did not draft a conventional title alone: calls=%d", len(ag.calls))
	}
	if logs := string(mustRead(t, logFile)); !strings.Contains(logs, "--title chore: detect plaintext profiles") {
		t.Fatalf("drafted title was not tightened:\n%s", logs)
	}
	if body := string(mustRead(t, envEntry(env, "FAKE_CLI_PR_BODY_FILE"))); !strings.HasPrefix(body, suppliedPRBodyText) {
		t.Fatalf("supplied body = %q", body)
	}
}

func TestPRSuppliedBodyOnlyFallsBackToTheOrdinaryTitleWhenDraftingFails(t *testing.T) {
	t.Parallel()
	ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		return nil, errors.New("agent unavailable")
	}}
	sctx, env, logFile := suppliedPRContext(t, ag, "", suppliedPRBodyText)
	if _, err := (&PRStep{}).Execute(sctx); err != nil {
		t.Fatalf("a failed title draft failed the PR step: %v", err)
	}
	if logs := string(mustRead(t, logFile)); !strings.Contains(logs, "--title chore: update pull request") {
		t.Fatalf("body-only run did not use the ordinary fallback title:\n%s", logs)
	}
	if body := string(mustRead(t, envEntry(env, "FAKE_CLI_PR_BODY_FILE"))); !strings.HasPrefix(body, suppliedPRBodyText+"\n\n<!-- no-mistakes-pr-appendix:v1 ") {
		t.Fatalf("supplied body was not published verbatim above the appendix:\n%s", body)
	}
}

func TestPRSuppliedTitleTakesTheConfiguredTitleFormat(t *testing.T) {
	t.Parallel()
	sctx, _, logFile := suppliedPRContext(t, noDraftAgent(t), suppliedPRTitleText, suppliedPRBodyText, "feature/PROJ-123-add-widget")
	sctx.Config.Commit.BranchPattern = `([A-Z]+-[0-9]+)`
	sctx.Config.PR.TitleFormat = "{{.Branch}}: {{.Title}}"
	if _, err := (&PRStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if logs := string(mustRead(t, logFile)); !strings.Contains(logs, "--title PROJ-123: "+suppliedPRTitleText) {
		t.Fatalf("title_format was not applied on top of the supplied title:\n%s", logs)
	}
}

func TestPRSuppliedTitleIgnoresAnInvalidDraftedTitleUnderTitleFormat(t *testing.T) {
	t.Parallel()
	ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		data, _ := json.Marshal(prContent{Title: "drafted\nsecond line", Body: "## What Changed\n\n- drafted"})
		return &agent.Result{Output: data}, nil
	}}
	sctx, env, logFile := suppliedPRContext(t, ag, suppliedPRTitleText, "", "feature/PROJ-123-add-widget")
	sctx.Config.Commit.BranchPattern = `([A-Z]+-[0-9]+)`
	sctx.Config.PR.TitleFormat = "{{.Branch}}: {{.Title}}"
	if _, err := (&PRStep{}).Execute(sctx); err != nil {
		t.Fatalf("a discarded drafted title blocked publishing: %v", err)
	}
	if logs := string(mustRead(t, logFile)); !strings.Contains(logs, "--title PROJ-123: "+suppliedPRTitleText) {
		t.Fatalf("supplied title was not published through title_format:\n%s", logs)
	}
	if body := string(mustRead(t, envEntry(env, "FAKE_CLI_PR_BODY_FILE"))); !strings.Contains(body, "- drafted") {
		t.Fatalf("drafted body was not kept:\n%s", body)
	}
}

func TestPRSuppliedBodyOverProviderBudgetFailsLoudly(t *testing.T) {
	t.Parallel()
	body := strings.Repeat("a", 4000)
	sctx, _, _ := suppliedPRContext(t, noDraftAgent(t), suppliedPRTitleText, body)
	_, err := (&PRStep{}).buildSuppliedPRContent(sctx, "feature", "main", sctx.Run.BaseSHA, "", scm.ProviderAzureDevOps, scm.MaxPRBodyChars(scm.ProviderAzureDevOps))
	if err == nil || !strings.Contains(err.Error(), "exceeds provider budget") {
		t.Fatalf("error = %v", err)
	}
}

func TestPRSuppliedBodyOnBitbucketCarriesTheVisibleAttestation(t *testing.T) {
	t.Parallel()
	sctx, _, _ := suppliedPRContext(t, noDraftAgent(t), suppliedPRTitleText, suppliedPRBodyText)
	content, err := (&PRStep{}).buildSuppliedPRContent(sctx, "feature", "main", sctx.Run.BaseSHA, "", scm.ProviderBitbucket, scm.MaxPRBodyChars(scm.ProviderBitbucket))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(content.Body, suppliedPRBodyText) || !strings.Contains(content.Body, "```text\n<!-- no-mistakes-pipeline-attestation:v1 ") {
		t.Fatalf("Bitbucket body = %q", content.Body)
	}
}

func TestPRFreshSuppliedTextAppliesOncePerRun(t *testing.T) {
	t.Parallel()
	sctx, _, _ := suppliedPRContext(t, noDraftAgent(t), suppliedPRTitleText, suppliedPRBodyText)
	env, logFile := fakeGH(t, "https://github.com/test/repo/pull/42")
	bodyFile := envEntry(env, "FAKE_CLI_PR_BODY_FILE")
	if err := os.WriteFile(bodyFile, []byte(keptAuthorBody), 0o644); err != nil {
		t.Fatal(err)
	}
	sctx.Env = append(env, "FAKE_CLI_PR_TITLE="+keptAuthorTitle)
	sctx.Run.PRTitleFresh, sctx.Run.PRBodyFresh = true, true
	if _, err := (&PRStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	first := string(mustRead(t, bodyFile))
	if !strings.HasPrefix(first, suppliedPRBodyText) {
		t.Fatalf("first pass did not apply the supplied description:\n%s", first)
	}
	if logs := string(mustRead(t, logFile)); !strings.Contains(logs, "--title "+suppliedPRTitleText) {
		t.Fatalf("first pass did not apply the supplied title:\n%s", logs)
	}

	const maintainerNarrative = "Maintainer rewrote the description after the first publication.\n"
	edited := maintainerNarrative + strings.TrimPrefix(first, suppliedPRBodyText)
	if err := os.WriteFile(bodyFile, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logFile, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	const maintainerTitle = "Maintainer title"
	sctx.Env = append(env, "FAKE_CLI_PR_TITLE="+maintainerTitle)
	recovered, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.PRTextAppliedAt == nil {
		t.Fatal("first pass did not durably record that the supplied text was applied")
	}
	recovered.PRTitle, recovered.PRBody = sctx.Run.PRTitle, sctx.Run.PRBody
	recovered.PRTitleFresh, recovered.PRBodyFresh = true, true
	sctx.Run = recovered
	if _, err := (&PRStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	second := string(mustRead(t, bodyFile))
	if !strings.HasPrefix(second, maintainerNarrative) || strings.Contains(second, "Detects plaintext cf profiles") {
		t.Fatalf("second pass reverted the maintainer's description:\n%s", second)
	}
	if publishedAttestationHead(t, second) != sctx.Run.HeadSHA {
		t.Fatalf("second pass lost the attestation:\n%s", second)
	}
	assertKeptEvidence(t, second, config.PRAppendixFull, keptRiskLine)
	if logs := string(mustRead(t, logFile)); strings.Contains(logs, "--title") {
		t.Fatalf("second pass rewrote the maintainer's title:\n%s", logs)
	}
}

func TestPRFreshTitleSurvivesARegenerateRestartInTheSameRun(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		existing bool
	}{
		{name: "existing_pr", existing: true},
		{name: "created_then_restarted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
				data, _ := json.Marshal(prContent{Title: "feat: regenerated", Body: "## What Changed\n\n- regenerated"})
				return &agent.Result{Output: data}, nil
			}}
			sctx, createEnv, createLog := suppliedPRContext(t, ag, suppliedPRTitleText, "")
			sctx.Run.PRTitleFresh = true
			existingEnv, existingLog := fakeGH(t, "https://github.com/test/repo/pull/42")
			bodyFile := envEntry(existingEnv, "FAKE_CLI_PR_BODY_FILE")
			if tc.existing {
				if err := os.WriteFile(bodyFile, []byte(keptAuthorBody), 0o644); err != nil {
					t.Fatal(err)
				}
				sctx.Env = append(existingEnv, "FAKE_CLI_PR_TITLE="+keptAuthorTitle)
				if _, err := (&PRStep{}).Execute(sctx); err != nil {
					t.Fatal(err)
				}
				if logs := string(mustRead(t, existingLog)); !strings.Contains(logs, "--title "+suppliedPRTitleText) {
					t.Fatalf("first pass did not apply the supplied title:\n%s", logs)
				}
			} else {
				sctx.Env = createEnv
				if _, err := (&PRStep{}).Execute(sctx); err != nil {
					t.Fatal(err)
				}
				if logs := string(mustRead(t, createLog)); !strings.Contains(logs, "--title "+suppliedPRTitleText) {
					t.Fatalf("creation did not use the supplied title:\n%s", logs)
				}
				if err := os.WriteFile(bodyFile, []byte("## What Changed\n\n- created\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				sctx.Env = append(existingEnv, "FAKE_CLI_PR_TITLE="+suppliedPRTitleText)
			}
			if sctx.Run.PRTextAppliedAt == nil {
				t.Fatal("first pass did not record that the supplied text was applied")
			}
			if err := os.WriteFile(existingLog, nil, 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := (&PRStep{}).Execute(sctx); err != nil {
				t.Fatal(err)
			}
			logs := string(mustRead(t, existingLog))
			if !strings.Contains(logs, "--title "+suppliedPRTitleText) || strings.Contains(logs, "feat: regenerated") {
				t.Fatalf("regenerate restart replaced the supplied title:\n%s", logs)
			}
			if body := string(mustRead(t, bodyFile)); !strings.Contains(body, "- regenerated") {
				t.Fatalf("regenerate restart did not rewrite the body:\n%s", body)
			}
		})
	}
}
