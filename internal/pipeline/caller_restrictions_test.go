package pipeline

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/callerrestrictions"
	"github.com/kunchenguid/no-mistakes/internal/db"
)

// The original failing-first version decoded this durable wire shape against
// the old reader, which silently discarded the bounds.
func TestRunAgentCallerRestrictionsReachEveryTurn(t *testing.T) {
	var run db.Run
	err := json.Unmarshal([]byte(`{"ID":"run-one","RepoID":"repo-one","Branch":"feature","HeadSHA":"current-head","SubmittedHeadSHA":"submitted-head","CallerRestrictions":{"source":"caller --caller-restrictions","source_path":"/private/caller.txt","content":"Never rerun a suite or install dependencies.\n","sha256":"5b4b87be55d7333566f305ca37ede5388861fc932d72bf703b3d80e2d1d617e65","captured_at":1}}`), &run)
	if err != nil {
		t.Fatal(err)
	}
	run.CallerRestrictions.SourcePath = filepath.Join(t.TempDir(), "caller.txt")
	run.CallerRestrictions.SHA256 = fmt.Sprintf("%x", sha256.Sum256([]byte(run.CallerRestrictions.Content)))
	ag := &hangingAgent{name: "fake", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		for _, want := range []string{"Never rerun a suite or install dependencies.\n", "caller --caller-restrictions", "run-one", "submitted-head", "current-head", "trusted configuration", "report"} {
			if !strings.Contains(opts.Prompt, want) {
				t.Errorf("actual invocation input lost %q: %s", want, opts.Prompt)
			}
		}
		return &agent.Result{Text: "ok"}, nil
	}}
	sctx := &StepContext{Ctx: context.Background(), Run: &run, Agent: ag}
	for i := 0; i < 2; i++ {
		if _, err := sctx.RunAgent(agent.RunOpts{Prompt: "perform step"}); err != nil {
			t.Fatal(err)
		}
		if _, err := sctx.RunAgentSessionContext(context.Background(), SessionRoleFixer, agent.RunOpts{Prompt: "continue step"}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRunAgentCallerRestrictionsSurviveSessionsFallbackAndDatabaseReload(t *testing.T) {
	database, initial := sessionTestDB(t)
	source := filepath.Join(t.TempDir(), "restrictions.txt")
	const content = "Never install a missing prerequisite or rerun a suite.\n"
	if err := os.WriteFile(source, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	record, err := callerrestrictions.Capture(source)
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRunWithCallerRestrictions(initial.RepoID, "restrictions", "head", "base", nil, "", "", "", "", false, nil, db.RunPRText{}, nil, record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("install and rerun"), 0600); err != nil {
		t.Fatal(err)
	}
	fake := newFakeSessionAgent()
	sctx := &StepContext{Ctx: context.Background(), Run: run, Agent: fake, Sessions: NewRunSessions(database, run.ID, fake, true)}
	invoke := func() {
		t.Helper()
		if _, err := sctx.RunAgentSessionBudget(context.Background(), time.Second, 0, nil, SessionRoleFixer, agent.RunOpts{Prompt: "continue"}); err != nil {
			t.Fatal(err)
		}
	}
	invoke()
	invoke()
	fake.failResumes["sess-1"] = fmt.Errorf("synthetic stale session")
	invoke()
	// Recovery has only the database row and durable session identity.
	reloaded, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	sctx.Run, sctx.Sessions = reloaded, NewRunSessions(database, run.ID, fake, true)
	invoke()
	if len(fake.calls) != 5 || fake.calls[1].session.ID != "sess-1" || !fake.calls[3].fallback {
		t.Fatalf("continuation/fallback paths not exercised: %+v", fake.calls)
	}
	for _, call := range fake.calls {
		if !strings.Contains(call.prompt, content) || strings.Contains(call.prompt, "install and rerun") || !strings.Contains(call.prompt, run.ID) {
			t.Fatalf("lost immutable instructions: %s", call.prompt)
		}
	}
}

func TestRunAgentCallerRestrictionsUnsetPreservesPrompt(t *testing.T) {
	fake := &hangingAgent{runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		if opts.Prompt != "unchanged prompt" {
			t.Fatalf("unset control changed: %q", opts.Prompt)
		}
		return &agent.Result{}, nil
	}}
	if _, err := (&StepContext{Ctx: context.Background(), Run: &db.Run{}, Agent: fake}).RunAgent(agent.RunOpts{Prompt: "unchanged prompt"}); err != nil {
		t.Fatal(err)
	}
}
