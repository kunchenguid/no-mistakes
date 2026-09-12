//go:build e2e

package e2e

import (
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const dlock31DocumentFindings = `  - match: "You are the document phase"
    structured:
      summary: "fixture requires selected behavior repair"
      findings:
        - id: document-behavior
          severity: warning
          action: ask-user
          category: documentation
          description: "The documented retention requires a behavior change"
        - id: deferred-document
          severity: info
          action: ask-user
          category: documentation
          description: "Separate documentation decision, do not repair"
`

const dlock31DocumentRepair = `  - match: "Previous review findings to address:"
    edits:
      - path: retention.go
        new: "package main\nconst retentionMonths = 24\n"
    structured:
      summary: "repair selected retention behavior"
`

func TestDLOCK31ExecutableDocumentRepair(t *testing.T) {
	h, provider := newDLOCK31Harness(t, dlock31DocumentFindings)
	const branch = "feature/dlock31"
	const instruction = "DLOCK31 captain selects 24 month retention; repair only document-behavior"
	h.CommitChange(branch, "retention.go", "package main\nconst retentionMonths = 12\n", "add fixture")
	h.PushToGate(branch)
	parked := waitForStepStatus(t, h, branch, types.StepDocument, types.StepStatusAwaitingApproval, 60*time.Second)
	database, err := db.Open(paths.WithRoot(h.NMHome).DB())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	workdir := dlock31Workdir(t, database, parked.ID)
	originalHead := dlock31Git(t, h, workdir, "rev-parse", "HEAD")
	beforeCalls := len(h.AgentInvocations())
	setDLOCK31Actions(t, provider, dlock31DocumentRepair)
	out, err := h.Run("axi", "respond", "--action", "fix", "--findings", "document-behavior", "--instructions", instruction, "--wait", "1s")
	if err != nil && !dlock31SubmittedWaitElapsed(err, out) {
		t.Fatalf("selected Document response: %v\n%s", err, out)
	}
	settled := waitForStepStatus(t, h, branch, types.StepCI, types.StepStatusAwaitingApproval, 60*time.Second)
	if settled.ID != parked.ID || settled.HeadSHA == originalHead {
		t.Fatal("selected repair changed run identity or failed to create a commit")
	}
	dlock31Git(t, h, workdir, "merge-base", "--is-ancestor", originalHead, settled.HeadSHA)
	if h.UpstreamBranchSHA(branch) != settled.HeadSHA || dlock31Git(t, h, h.UpstreamDir, "show", settled.HeadSHA+":retention.go") != "package main\nconst retentionMonths = 24" {
		t.Fatal("repaired descendant not published to owned upstream")
	}
	assertDLOCK31DocumentInvocations(t, h.AgentInvocations()[beforeCalls:], instruction, settled.HeadSHA)
	assertDLOCK31DocumentRounds(t, database, parked.ID, instruction, originalHead, settled.HeadSHA)
}

func assertDLOCK31DocumentInvocations(t *testing.T, calls []Invocation, instruction, head string) {
	t.Helper()
	repair, review, test := -1, -1, -1
	for i, call := range calls {
		prompt := call.Prompt
		if strings.Contains(prompt, "Previous review findings to address:") {
			if repair >= 0 {
				t.Fatal("more than one selected repair invocation")
			}
			repair = i
			targets := strings.SplitN(prompt, "Previous review findings to address:", 2)[1]
			if !strings.Contains(prompt, "You are the review phase") || strings.Contains(prompt, "You are the document phase") || !strings.Contains(targets, instruction) || strings.Contains(targets, "deferred-document") {
				t.Fatalf("wrong phase, selection, or instructions in repair prompt:\n%s", prompt)
			}
			continue
		}
		if strings.Contains(prompt, "You are the review phase") && strings.Contains(prompt, head) {
			review = i
		}
		if strings.Contains(prompt, "You are validating a code change by driving the product itself") && strings.Contains(prompt, head) {
			test = i
		}
	}
	if repair < 0 || review <= repair || test <= review {
		t.Fatalf("real process order repair=%d review=%d test=%d", repair, review, test)
	}
}

func assertDLOCK31DocumentRounds(t *testing.T, database *db.DB, runID, instruction, original, head string) {
	t.Helper()
	run, err := database.GetRun(runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.ReviewApprovedHeadSHA == nil || *run.ReviewApprovedHeadSHA != head {
		t.Fatal("repaired head lacks fresh review authority")
	}
	steps, err := database.GetStepsByRun(runID)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range steps {
		rounds, err := database.GetRoundsByStep(step.ID)
		if err != nil {
			t.Fatal(err)
		}
		switch step.StepName {
		case types.StepDocument:
			if len(rounds) != 3 {
				t.Fatalf("document rounds=%d want original, repair, revalidation", len(rounds))
			}
			first := rounds[0]
			if first.SelectedFindingIDs == nil || *first.SelectedFindingIDs != `["document-behavior"]` || first.SelectionSource == nil || *first.SelectionSource != db.RoundSelectionSourceUser || first.UserFindingsJSON == nil || !strings.Contains(*first.UserFindingsJSON, instruction) || first.FindingsJSON == nil || !strings.Contains(*first.FindingsJSON, "deferred-document") {
				t.Fatalf("original Document decision trace lost: %+v", first)
			}
		case types.StepReview:
			if len(rounds) < 2 || rounds[0].ReviewedHeadSHA == nil || *rounds[0].ReviewedHeadSHA != original || rounds[len(rounds)-1].ReviewedHeadSHA == nil || *rounds[len(rounds)-1].ReviewedHeadSHA != head {
				t.Fatalf("review did not certify original and repaired heads: %+v", rounds)
			}
		case types.StepTest:
			if len(rounds) < 2 {
				t.Fatalf("Test rounds=%d want independent revalidation", len(rounds))
			}
		}
	}
}
