//go:build e2e

package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// A daemon that dies between recording an external settlement and starting the
// review round that settlement asks for must replay it on recovery, not park
// the gate again: the decision and its reason are already durable, and every
// continuation of a settled round is refused, so re-parking would strand the
// run with no answerable response.
//
// The crash window inside the live loop is a few microseconds wide and the
// product has no fault-injection seam in it, so the recorded settlement is
// written with the product's own persistence API (db.SetStepRoundUserDecision,
// the exact call the gate response makes) into this run's disposable database
// while the daemon is stopped. Everything after that is the real daemon's own
// recovery path.
//
// The oracle is the last paragraph of the Coverage continuation section in
// docs/src/content/docs/reference/pipeline-steps.md.
func TestReviewSettlementReplayedAfterDaemonRestart(t *testing.T) {
	const concern = "the deploy reads a rotation token this worktree cannot see, so the rotation cannot be verified here"
	const privateReason = "rotation applied in the private infra repository, verified by the platform owner"

	scenario := filepath.Join(t.TempDir(), "agent.yaml")
	body := `actions:
  - match: "user_resolved_external"
    structured:
      findings:
        - id: rotation-again
          severity: error
          file: deploy.sh
          line: 31
          description: ` + concern + `
          action: ask-user
          review_scope: source
      summary: the rotation concern is still visible from here
      risk_level: high
      risk_rationale: the deploy still depends on a secret this worktree cannot read
      risk_scope: source-or-external
      reviewed_paths: [deploy.sh]
  - match: "Review the code changes and return structured findings"
    structured:
      findings:
        - id: rotation
          severity: error
          file: deploy.sh
          line: 4
          description: ` + concern + `
          action: ask-user
          review_scope: source
      summary: one rotation concern
      risk_level: high
      risk_rationale: the deploy depends on a secret outside this repository
      risk_scope: source-or-external
      reviewed_paths: [deploy.sh]
  - structured:
      findings: []
      summary: clean
      risk_level: low
      risk_rationale: nothing else to report
      risk_scope: source-or-external
      tested: ["fixture"]
      testing_summary: fixture
      scenarios:
        - name: fixture
          result: pass
          live: true
          evidence: fixture
          reason: ""
      verdict: go
      artifacts: []
      title: "feat: rotate the deploy token"
      body: Deploy token rotation
`
	if err := os.WriteFile(scenario, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}

	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: scenario})
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	const branch = "feature/settlement-recovery"
	h.CommitChange(branch, "deploy.sh", "#!/usr/bin/env bash\n# deploy using the rotation token\ndeploy --token \"${DEPLOY_TOKEN}\"\n", "add deploy script")
	h.CommitChange(branch, "snapshot.sh", "#!/usr/bin/env bash\n# archive the release\ntar -czf /srv/snapshots/app.tar.gz /srv/app\n", "add snapshot script")

	gate1, err := h.Run("axi", "run", "--intent", "Rotate the deploy token", "--skip", "ci")
	if err != nil || !strings.Contains(gate1, "rotation") {
		t.Fatalf("initial review gate: %v\n%s", err, gate1)
	}
	run := h.ActiveRun(branch)
	if run == nil {
		t.Fatal("no active run at the review gate")
	}

	if out, err := h.Run("daemon", "stop", "--force"); err != nil {
		t.Fatalf("daemon stop: %v\n%s", err, out)
	}

	// Record the settlement exactly as the gate response does, then let the
	// daemon come back up and find it.
	settledRound := recordExternalSettlement(t, h, run.ID, "rotation", privateReason)
	if out, err := h.Run("daemon", "start"); err != nil {
		t.Fatalf("daemon start: %v\n%s", err, out)
	}

	rounds := waitForReviewRounds(t, h, branch, 2, 90*time.Second)
	settlementEvidence(t, "20-recovered-settlement-rounds.json", rounds.raw)
	if got := rounds.export.Rounds[0].SelectionSource; got != "resolved_external" {
		t.Fatalf("the recovered round's selection_source = %q, want resolved_external:\n%s", got, rounds.raw)
	}
	if got := rounds.export.Rounds[1].Trigger; got != "external" {
		t.Fatalf("recovery started a %q round, want the settlement replayed as an external round:\n%s", got, rounds.raw)
	}
	for _, item := range rounds.export.Rounds[1].Findings {
		if item.Description == concern {
			t.Fatalf("the replayed round still reports the settled concern:\n%s", rounds.raw)
		}
	}

	waitForStepStatus(t, h, branch, types.StepReview, types.StepStatusAwaitingApproval, 60*time.Second)
	status, err := h.Run("axi", "status")
	if err != nil {
		t.Fatalf("axi status after recovery: %v\n%s", err, status)
	}
	settlementEvidence(t, "21-recovered-settlement-status.txt", "$ no-mistakes axi status\n"+status)
	if strings.Contains(status, "rotation-again") {
		t.Fatalf("the settled concern is back on the recovered gate:\n%s", status)
	}
	if !strings.Contains(status, "--action complete-coverage") {
		t.Fatalf("the recovered gate does not offer the coverage continuation:\n%s", status)
	}
	t.Logf("settlement recorded on round %s was replayed after the restart", settledRound)
}

// recordExternalSettlement stores an external finding disposition on the review
// step's latest round through the product's own writer, and returns the round
// id it wrote.
func recordExternalSettlement(t *testing.T, h *Harness, runID, findingID, reason string) string {
	t.Helper()
	database, err := db.Open(paths.WithRoot(h.NMHome).DB())
	if err != nil {
		t.Fatalf("open e2e db: %v", err)
	}
	defer database.Close()
	steps, err := database.GetStepsByRun(runID)
	if err != nil {
		t.Fatalf("read steps of run %s: %v", runID, err)
	}
	var review *db.StepResult
	for _, s := range steps {
		if s.StepName == types.StepReview {
			review = s
		}
	}
	if review == nil {
		t.Fatalf("run %s has no review step", runID)
	}
	rounds, err := database.GetRoundsByStep(review.ID)
	if err != nil || len(rounds) == 0 {
		t.Fatalf("read review rounds: %v", err)
	}
	last := rounds[len(rounds)-1]
	if last.FindingsJSON == nil {
		t.Fatalf("review round %s recorded no findings to settle", last.ID)
	}
	parsed, err := types.ParseFindingsJSON(*last.FindingsJSON)
	if err != nil {
		t.Fatalf("parse review round findings: %v", err)
	}
	var settled types.Findings
	for _, item := range parsed.Items {
		if item.ID == findingID {
			item.UserInstructions = reason
			settled.Items = append(settled.Items, item)
		}
	}
	if len(settled.Items) != 1 {
		t.Fatalf("round %s does not show finding %s once", last.ID, findingID)
	}
	encoded, err := types.MarshalFindingsJSON(settled)
	if err != nil {
		t.Fatal(err)
	}
	empty := db.DeclinedSelectionJSON
	if err := database.SetStepRoundUserDecision(last.ID, &empty, db.RoundSelectionSourceExternal, &encoded, types.ResponseOriginInteractive); err != nil {
		t.Fatalf("record the external settlement: %v", err)
	}
	return last.ID
}

type reviewRoundsExport struct {
	raw    string
	export struct {
		Rounds []struct {
			Round           int    `json:"round"`
			Trigger         string `json:"trigger"`
			SelectionSource string `json:"selection_source"`
			Findings        []struct {
				ID          string `json:"id"`
				Description string `json:"description"`
			} `json:"findings"`
		} `json:"rounds"`
	}
}

func waitForReviewRounds(t *testing.T, h *Harness, branch string, want int, timeout time.Duration) reviewRoundsExport {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last reviewRoundsExport
	for time.Now().Before(deadline) {
		out, err := h.Run("axi", "rounds", "--step", "review", "--json")
		if err == nil {
			last = reviewRoundsExport{raw: out}
			if json.Unmarshal([]byte(out), &last.export) == nil && len(last.export.Rounds) >= want {
				return last
			}
		} else {
			last = reviewRoundsExport{raw: out}
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("review never recorded %d round(s) on %s; last export:\n%s", want, branch, last.raw)
	return last
}
