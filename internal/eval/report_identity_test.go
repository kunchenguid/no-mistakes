package eval

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/buildinfo"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func writeSessionPlan(t *testing.T, store *Store, session Session) {
	t.Helper()
	dir := filepath.Join(store.root, "sessions")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(dir, session.ID+".json"), session); err != nil {
		t.Fatal(err)
	}
}

func persistSyntheticEvaluation(t *testing.T, store *Store, evaluation Evaluation) {
	t.Helper()
	c := Case{Manifest: Manifest{ID: evaluation.CaseID, SourceRunID: "run-" + evaluation.CaseID, SourceRoundID: "round-" + evaluation.CaseID}, Dir: store.caseDir(evaluation.CaseID)}
	if err := os.MkdirAll(c.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(c.Dir, "labels.json"), Labels{Version: labelsVersion}); err != nil {
		t.Fatal(err)
	}
	if err := store.registerCase(c); err != nil {
		t.Fatal(err)
	}
	if evaluation.Repeat == 0 {
		evaluation.Repeat = 1
	}
	if evaluation.ID == "" {
		evaluation.ID = fmt.Sprintf("%s-%s-%d", evaluation.SessionID, evaluation.CaseID, evaluation.Repeat)
	}
	if err := store.persistEvaluation(c, evaluation); err != nil {
		t.Fatal(err)
	}
}

func setReplayCommit(t *testing.T, commit string) {
	t.Helper()
	previous := buildinfo.Commit
	buildinfo.Commit = commit
	t.Cleanup(func() { buildinfo.Commit = previous })
}

func TestCohortIDSeparatesReplayBuilds(t *testing.T) {
	one := ReplayBuild{Version: "v1.0.0", Commit: "aaaaaaa"}
	two := ReplayBuild{Version: "v1.0.0", Commit: "bbbbbbb"}
	if cohortID([]string{"a", "b"}, 2, one) != cohortID([]string{"b", "a"}, 2, one) {
		t.Fatal("the same plan and build produced different cohorts")
	}
	if cohortID([]string{"a", "b"}, 2, one) == cohortID([]string{"a", "b"}, 2, two) {
		t.Fatal("two builds over the same cases and repeats shared a cohort")
	}
	if cohortID([]string{"a", "b"}, 2, one) == cohortID([]string{"a", "b"}, 2, ReplayBuild{Version: "v1.0.1", Commit: "aaaaaaa"}) {
		t.Fatal("two versions over the same cases and repeats shared a cohort")
	}
}

// An A/B of two builds over the same cases and candidate is two subjects
// under test, so the report must show two rows, each naming its build, while
// a rerun of one build still aggregates into its row.
func TestReplaysOfDifferentBuildsNeverShareAReportRow(t *testing.T) {
	ctx := context.Background()
	p, sourceDB, run, _, _ := setupCapturedRun(t, ctx)
	defer sourceDB.Close()
	installFakeReviewAgent(t, p, `{"findings":[{"id":"real-bug","severity":"error","file":"main.go","line":3,"description":"bug","action":"ask-user","review_scope":"source"}],"risk_level":"high","risk_rationale":"bug","risk_scope":"source-or-external"}`)
	store, err := Open(p.EvalDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := Capture(ctx, store, p, sourceDB, run.ID); err != nil {
		t.Fatal(err)
	}
	opts := ReplayOptions{Set: "labeled", Candidate: Candidate{Agent: types.AgentClaude, Model: "test"}, Repeats: 1}

	setReplayCommit(t, "aaaaaaa")
	first, firstEvals, err := Replay(ctx, store, opts)
	if err != nil {
		t.Fatal(err)
	}
	rerun, rerunEvals, err := Replay(ctx, store, opts)
	if err != nil {
		t.Fatal(err)
	}
	setReplayCommit(t, "bbbbbbb")
	other, _, err := Replay(ctx, store, opts)
	if err != nil {
		t.Fatal(err)
	}

	if first.Build.Commit != "aaaaaaa" || other.Build.Commit != "bbbbbbb" {
		t.Fatalf("session builds = %+v and %+v, want the running binary's commit", first.Build, other.Build)
	}
	if first.Cohort != rerun.Cohort {
		t.Fatalf("a rerun of the same build changed cohort %q -> %q", first.Cohort, rerun.Cohort)
	}
	if first.Cohort == other.Cohort {
		t.Fatalf("two builds shared cohort %q", first.Cohort)
	}
	if firstEvals[0].PromptFingerprint == "" {
		t.Fatal("a prompted replay recorded no prompt fingerprint")
	}
	if firstEvals[0].PromptFingerprint != rerunEvals[0].PromptFingerprint {
		t.Fatalf("identical replays rendered different prompt fingerprints %q and %q; the per-replay sandbox path leaked into it", firstEvals[0].PromptFingerprint, rerunEvals[0].PromptFingerprint)
	}

	reports, err := Report(store)
	if err != nil {
		t.Fatal(err)
	}
	if len(reports) != 2 {
		t.Fatalf("reports = %#v, want one row per build", reports)
	}
	rows := map[string]CandidateReport{}
	for _, report := range reports {
		rows[report.Cohort] = report
	}
	if got := rows[first.Cohort]; got.Summary.Total != 2 || got.Completeness.Sessions != 2 || !strings.Contains(got.Build, "(aaaaaaa)") {
		t.Fatalf("first build row = %#v, want both of its sessions aggregated under its build", got)
	}
	if got := rows[other.Cohort]; got.Summary.Total != 1 || !strings.Contains(got.Build, "(bbbbbbb)") {
		t.Fatalf("second build row = %#v, want its single session under its own build", got)
	}
	output := RenderReport(reports)
	for _, want := range []string{first.Build.String() + "; review prompt+config fingerprint: " + rows[first.Cohort].PromptFingerprint, other.Build.String() + "; review prompt+config fingerprint: "} {
		if !strings.Contains(output, want) {
			t.Fatalf("report = %q, want %q", output, want)
		}
	}
}

// Two binaries can report the same build (a modified local tree) and still
// render different review prompts; their sessions must not aggregate, while a
// session that agrees on every case it shares still joins.
func TestReportSplitsSessionsWhoseReviewPromptsDiffer(t *testing.T) {
	store := openEvalStore(t)
	cases := []string{"case-a", "case-b"}
	for _, id := range []string{"s1", "s2", "s3"} {
		writeSessionPlan(t, store, Session{ID: id, Candidate: "claude,model=test", Repeats: 1, CaseIDs: cases, Cohort: "same"})
	}
	prompts := map[string]map[string]string{
		"s1": {"case-a": "p-a1", "case-b": "p-b"},
		"s2": {"case-a": "p-a2", "case-b": "p-b"},
		"s3": {"case-a": "p-a1", "case-b": "p-b"},
	}
	for _, session := range []string{"s1", "s2", "s3"} {
		for _, caseID := range cases {
			persistSyntheticEvaluation(t, store, Evaluation{SessionID: session, CaseID: caseID, Candidate: "claude,model=test", Cohort: "same",
				Status: "completed", HasFindingGold: true, GoldCount: 1, TruePositive: 1, PromptFingerprint: prompts[session][caseID]})
		}
	}

	reports, err := Report(store)
	if err != nil {
		t.Fatal(err)
	}
	if len(reports) != 2 {
		t.Fatalf("reports = %#v, want the disagreeing session in its own row", reports)
	}
	if reports[0].PromptFingerprint == "" || reports[0].PromptFingerprint == reports[1].PromptFingerprint {
		t.Fatalf("prompt fingerprints = %q and %q, want two distinct recorded variants", reports[0].PromptFingerprint, reports[1].PromptFingerprint)
	}
	sessions := map[int]int{}
	for _, report := range reports {
		sessions[report.Completeness.Sessions]++
		if report.Incomplete {
			t.Fatalf("row %#v is incomplete, want every planned replay counted complete", report)
		}
	}
	if sessions[2] != 1 || sessions[1] != 1 {
		t.Fatalf("rows hold %v session(s), want s1+s3 together and s2 alone", sessions)
	}
}

// The reported failure: a session stopped after part of its plan, and the
// report scored the cases that ran as if the experiment were finished.
func TestReportMarksAStoppedSessionIncomplete(t *testing.T) {
	store := openEvalStore(t)
	cost := func(row Evaluation) Evaluation {
		row.Cohort = "same"
		row.Status = "completed"
		row.HasFindingGold = true
		row.GoldCount = 1
		row.TruePositive = 1
		row.TokensReported = true
		row.FreshInputTokens = 10
		return row
	}
	writeSessionPlan(t, store, Session{ID: "stopped", Candidate: "cheap", Repeats: 2, CaseIDs: []string{"c1", "c2", "c3"}, Cohort: "same"})
	persistSyntheticEvaluation(t, store, cost(Evaluation{SessionID: "stopped", CaseID: "c1", Candidate: "cheap", Repeat: 1}))
	persistSyntheticEvaluation(t, store, cost(Evaluation{SessionID: "stopped", CaseID: "c1", Candidate: "cheap", Repeat: 2}))
	persistSyntheticEvaluation(t, store, cost(Evaluation{SessionID: "stopped", CaseID: "c2", Candidate: "cheap", Repeat: 1}))
	writeSessionPlan(t, store, Session{ID: "finished", Candidate: "pricey", Repeats: 1, CaseIDs: []string{"c1"}, Cohort: "same"})
	pricey := cost(Evaluation{SessionID: "finished", CaseID: "c1", Candidate: "pricey"})
	pricey.FreshInputTokens = 1000
	persistSyntheticEvaluation(t, store, pricey)

	reports, err := Report(store)
	if err != nil {
		t.Fatal(err)
	}
	byCandidate := map[string]CandidateReport{}
	for _, report := range reports {
		byCandidate[report.Summary.Candidate] = report
	}
	stopped := byCandidate["cheap"]
	want := Completeness{PlansKnown: true, Sessions: 1, PlannedCases: 3, CompletedCases: 1, PlannedReplays: 6, RecordedReplays: 3, CompletedReplays: 3}
	if stopped.Completeness != want || !stopped.Incomplete {
		t.Fatalf("stopped session = %+v (incomplete %v), want %+v and incomplete", stopped.Completeness, stopped.Incomplete, want)
	}
	if stopped.OnFrontier {
		t.Fatal("an incomplete slice joined the frontier; its partial cost and recall are not comparable")
	}
	if finished := byCandidate["pricey"]; finished.Incomplete || !finished.OnFrontier {
		t.Fatalf("finished slice = %#v, want complete and on the frontier once the cheaper partial slice is ineligible", finished)
	}
	output := RenderReport(reports)
	for _, want := range []string{
		"cheap (cohort same) INCOMPLETE\n",
		"completeness: incomplete - 1 of 3 planned case(s) and 3 of 6 planned replay(s) completed across 1 session(s) (3 missing, 0 failed); scores cover only the recorded replays",
		"completeness: complete - 1 of 1 planned case(s) and 1 of 1 planned replay(s) completed across 1 session(s)",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("report = %q, want %q", output, want)
		}
	}
	if strings.Contains(output, "pricey (cohort same) INCOMPLETE") {
		t.Fatalf("report = %q, marked the finished slice incomplete", output)
	}
}

func TestReportCountsFailedReplaysAsIncomplete(t *testing.T) {
	store := openEvalStore(t)
	writeSessionPlan(t, store, Session{ID: "s", Candidate: "c", Repeats: 1, CaseIDs: []string{"c1", "c2"}, Cohort: "same"})
	persistSyntheticEvaluation(t, store, Evaluation{SessionID: "s", CaseID: "c1", Candidate: "c", Cohort: "same", Status: "completed"})
	persistSyntheticEvaluation(t, store, Evaluation{SessionID: "s", CaseID: "c2", Candidate: "c", Cohort: "same", Status: "failed"})
	reports, err := Report(store)
	if err != nil {
		t.Fatal(err)
	}
	if len(reports) != 1 || !reports[0].Incomplete || reports[0].Completeness.Failed() != 1 || reports[0].Completeness.Missing() != 0 {
		t.Fatalf("reports = %#v, want one incomplete slice with one failed replay", reports)
	}
	if !strings.Contains(RenderReport(reports), "(0 missing, 1 failed)") {
		t.Fatalf("report = %q, want the failed replay counted", RenderReport(reports))
	}
}

// Sessions written before the build field still read, and a plan that cannot
// be read makes completeness unknown rather than failing the report or
// passing the slice as complete.
func TestReportReadsSessionsRecordedBeforeBuildIdentity(t *testing.T) {
	store := openEvalStore(t)
	if err := os.MkdirAll(filepath.Join(store.root, "sessions"), 0o755); err != nil {
		t.Fatal(err)
	}
	legacy := `{"id":"legacy","started_at":"2026-01-01T00:00:00Z","set":"labeled","candidate":"c","repeats":1,"case_ids":["c1"],"cohort":"old"}`
	if err := os.WriteFile(filepath.Join(store.root, "sessions", "legacy.json"), []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	persistSyntheticEvaluation(t, store, Evaluation{SessionID: "legacy", CaseID: "c1", Candidate: "c", Cohort: "old", Status: "completed"})
	persistSyntheticEvaluation(t, store, Evaluation{SessionID: "unplanned", CaseID: "c1", Candidate: "d", Cohort: "old", Status: "completed"})

	reports, err := Report(store)
	if err != nil {
		t.Fatal(err)
	}
	byCandidate := map[string]CandidateReport{}
	for _, report := range reports {
		byCandidate[report.Summary.Candidate] = report
	}
	if got := byCandidate["c"]; got.Build != "not recorded" || got.PromptFingerprint != "" || got.Incomplete {
		t.Fatalf("legacy slice = %#v, want build and prompt not recorded and a complete plan", got)
	}
	if got := byCandidate["d"]; got.Completeness.PlansKnown || !got.Incomplete {
		t.Fatalf("unplanned slice = %#v, want an unknown plan marked incomplete", got)
	}
	output := RenderReport(reports)
	for _, want := range []string{
		"build: not recorded; review prompt+config fingerprint: not recorded",
		"completeness: unknown - a session plan was not recorded; 1 of 1 recorded replay(s) completed",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("report = %q, want %q", output, want)
		}
	}
}

func TestReportIncludesPlansWithoutAnyResults(t *testing.T) {
	for _, withCompleted := range []bool{false, true} {
		t.Run(fmt.Sprint(withCompleted), func(t *testing.T) {
			store := openEvalStore(t)
			plan := Session{ID: "stopped", Candidate: "candidate", Repeats: 2, CaseIDs: []string{"c1", "c2"}, Cohort: "same"}
			writeSessionPlan(t, store, plan)
			want := Completeness{PlansKnown: true, Sessions: 1, PlannedCases: 2, PlannedReplays: 4}
			if withCompleted {
				plan.ID = "finished"
				writeSessionPlan(t, store, plan)
				for _, id := range plan.CaseIDs {
					for repeat := 1; repeat <= 2; repeat++ {
						persistSyntheticEvaluation(t, store, Evaluation{SessionID: plan.ID, CaseID: id, Repeat: repeat, Candidate: plan.Candidate, Cohort: plan.Cohort, Status: "completed"})
					}
				}
				want.Sessions = 2
				want.PlannedReplays = 8
				want.RecordedReplays = 4
				want.CompletedReplays = 4
			}
			reports, err := Report(store)
			if err != nil {
				t.Fatal(err)
			}
			if len(reports) != 1 {
				t.Fatalf("reports = %#v", reports)
			}
			got := reports[0]
			if got.Completeness != want || !got.Incomplete || got.OnFrontier || got.Summary.Candidate != plan.Candidate {
				t.Fatalf("report = %#v, want incomplete candidate with %+v", got, want)
			}
			if !strings.Contains(RenderReport(reports), "candidate (cohort same) INCOMPLETE") {
				t.Fatal("zero-result plan was not visibly incomplete")
			}
		})
	}
}

// The report's plan is the oracle: an unidentified stopped session must not
// make either completed A/B variant incomplete or remove it from the frontier.
func TestReportKeepsAmbiguousSessionsInAnUnknownVariant(t *testing.T) {
	for _, fixture := range []struct {
		name    string
		prompts map[string]string
		unique  bool
	}{
		{name: "stopped-before-first-result"},
		{name: "result-without-prompt", prompts: map[string]string{"c1": ""}},
		{name: "ambiguous-partial", prompts: map[string]string{"c1": "shared"}},
		{name: "identified-partial", prompts: map[string]string{"c2": "variant-a"}, unique: true},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			store := openEvalStore(t)
			cases := []string{"c1", "c2"}
			for _, id := range []string{"partial", "a", "b"} {
				writeSessionPlan(t, store, Session{ID: id, Candidate: "candidate", Repeats: 1, CaseIDs: cases, Cohort: "same"})
				prompts := fixture.prompts
				if id != "partial" {
					prompts = map[string]string{"c1": "shared", "c2": "variant-" + id}
				}
				for _, caseID := range cases {
					prompt, recorded := prompts[caseID]
					if !recorded {
						continue
					}
					persistSyntheticEvaluation(t, store, Evaluation{SessionID: id, CaseID: caseID, Candidate: "candidate", Cohort: "same",
						Status: "completed", HasFindingGold: true, GoldCount: 1, TruePositive: 1, PromptFingerprint: prompt,
						TokensReported: true, FreshInputTokens: 10})
				}
			}
			reports, err := Report(store)
			if err != nil {
				t.Fatal(err)
			}
			wantRows := 3
			if fixture.unique {
				wantRows = 2
			}
			if len(reports) != wantRows {
				t.Fatalf("reports = %#v, want %d rows", reports, wantRows)
			}
			unknown, complete, incomplete := 0, 0, 0
			for _, report := range reports {
				if report.PromptFingerprint == "" {
					unknown++
					want := Completeness{PlansKnown: true, Sessions: 1, PlannedCases: 2, PlannedReplays: 2,
						CompletedCases: len(fixture.prompts), RecordedReplays: len(fixture.prompts), CompletedReplays: len(fixture.prompts)}
					if report.Completeness != want || !report.Incomplete || report.OnFrontier {
						t.Fatalf("unknown variant = %#v, want incomplete stopped plan %+v", report, want)
					}
				} else if report.Incomplete {
					incomplete++
					if !fixture.unique || report.Completeness.Sessions != 2 || report.Completeness.CompletedReplays != 3 || report.Completeness.Missing() != 1 || report.OnFrontier {
						t.Fatalf("identified variant = %#v, want only the uniquely matching partial session merged", report)
					}
				} else {
					complete++
					if report.Completeness.Sessions != 1 || report.Completeness.CompletedReplays != 2 || !report.OnFrontier {
						t.Fatalf("completed variant = %#v, want its own complete plan on the frontier", report)
					}
				}
			}
			if fixture.unique {
				if unknown != 0 || complete != 1 || incomplete != 1 {
					t.Fatalf("unknown/complete/incomplete rows = %d/%d/%d, want 0/1/1", unknown, complete, incomplete)
				}
			} else if unknown != 1 || complete != 2 || incomplete != 0 {
				t.Fatalf("unknown/complete/incomplete rows = %d/%d/%d, want 1/2/0", unknown, complete, incomplete)
			}
		})
	}
}

// Merging s1 and s2 builds a {c1:x,c2:y} variant that both sessions then leave
// for unknown because each also agrees with another variant; that emptied
// variant is a prompt combination nobody ran and must not become a row.
func TestPromptVariantsDropsAVariantEmptiedByAmbiguity(t *testing.T) {
	rows := make(map[string][]Evaluation)
	sessions := []string{"s1", "s2", "s3", "s4"}
	for id, prompts := range map[string]map[string]string{
		"s1": {"c1": "x"},
		"s2": {"c2": "y"},
		"s3": {"c1": "z", "c2": "y"},
		"s4": {"c1": "x", "c2": "z"},
	} {
		for caseID, prompt := range prompts {
			rows[id] = append(rows[id], Evaluation{SessionID: id, CaseID: caseID, PromptFingerprint: prompt})
		}
	}
	got := make(map[string][]string)
	for _, variant := range promptVariants(sessions, rows) {
		if len(variant.sessions) == 0 {
			t.Fatalf("variant %+v has no sessions", variant)
		}
		got[variant.fingerprint()] = variant.sessions
	}
	want := map[string][]string{
		promptVariant{prompts: map[string]string{"c1": "z", "c2": "y"}}.fingerprint(): {"s3"},
		promptVariant{prompts: map[string]string{"c1": "x", "c2": "z"}}.fingerprint(): {"s4"},
		"": {"s1", "s2"},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("variants = %v, want %v", got, want)
	}
}

func TestExecutableContentsSeparateReplayCohorts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "executable")
	builds := make([]ReplayBuild, 0, 2)
	for _, fixture := range []struct{ content, digest string }{
		{"", "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"},
		{"abc", "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"},
	} {
		if err := os.WriteFile(path, []byte(fixture.content), 0o600); err != nil {
			t.Fatal(err)
		}
		digest, err := executableDigest(path)
		if err != nil {
			t.Fatal(err)
		}
		if digest != fixture.digest {
			t.Fatalf("SHA-256 = %s, want standard vector %s", digest, fixture.digest)
		}
		builds = append(builds, ReplayBuild{Version: "dev", Commit: "same-dirty", ExecutableSHA256: digest})
	}
	if cohortID([]string{"case"}, 1, builds[0]) == cohortID([]string{"case"}, 1, builds[1]) {
		t.Fatal("different executable contents shared a cohort")
	}
	if _, err := executableDigest(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("unreadable executable accepted")
	}
	current, err := currentReplayBuild()
	if err != nil || len(current.ExecutableSHA256) != 64 {
		t.Fatalf("current build = %+v, %v", current, err)
	}
}

func TestObservedAgentCapturesSteeredFirstPrompt(t *testing.T) {
	observed := &observedAgent{inner: &fixedResultAgent{}}
	steered := agent.WithSteering(observed, "evidence")
	if _, err := steered.Run(context.Background(), agent.RunOpts{Prompt: "review this change"}); err != nil {
		t.Fatal(err)
	}
	want := agent.WorktreeSteering("evidence") + "review this change"
	if observed.firstPrompt != want {
		t.Fatalf("observed prompt = %q, want delivered prompt %q", observed.firstPrompt, want)
	}
	if _, err := steered.Run(context.Background(), agent.RunOpts{Prompt: "schema correction"}); err != nil {
		t.Fatal(err)
	}
	if observed.firstPrompt != want {
		t.Fatal("a later turn replaced the first prompt")
	}
}
