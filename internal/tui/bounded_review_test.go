package tui

import (
	"slices"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

const boundedReviewTUIFindings = `{"review_strategy":"bounded","findings":[{"id":"review-1","severity":"error","description":"confirmed defect","evidence":"trace one","verification":"run test one","action":"auto-fix"},{"id":"review-2","severity":"warning","description":"unsupported claim","evidence":"trace two","verification":"inspect caller","action":"auto-fix"}],"summary":"two findings"}`

func newBoundedReviewTUIModel(t *testing.T) Model {
	t.Helper()
	run := testRun()
	run.Steps[0].Status = types.StepStatusAwaitingApproval
	run.Steps[0].FindingsJSON = ptr(boundedReviewTUIFindings)
	return NewModel("", nil, run)
}

func TestBoundedReviewDispositionEditorRequiresDecisionAndEvidence(t *testing.T) {
	m := newBoundedReviewTUIModel(t)
	next, _ := m.handleKey(keyMsg("v"))
	m = next.(Model)
	if !m.editorActive() || m.editor.kind != editorDisposition {
		t.Fatal("expected bounded Review disposition editor")
	}

	next, _ = m.handleKey(keyMsg("ctrl+s"))
	m = next.(Model)
	if m.editor == nil || !strings.Contains(m.editor.errorMsg, "choose disposition") {
		t.Fatalf("missing decision error = %q", m.editor.errorMsg)
	}

	next, _ = m.handleKey(keyMsg("1"))
	m = next.(Model)
	m.editor.dispositionReason.SetValue("reproduced with focused test")
	next, _ = m.handleKey(keyMsg("ctrl+s"))
	m = next.(Model)
	decision := m.findingDispositions[types.StepReview]["review-1"]
	if decision.Decision != types.FindingDispositionFix || decision.Reason != "reproduced with focused test" {
		t.Fatalf("saved disposition = %+v", decision)
	}
	if !strings.Contains(m.combinedFindingsJSON(types.StepReview), `"disposition":"confirmed-fix"`) {
		t.Fatal("locally entered disposition is not visible in rendered findings")
	}
}

func TestBoundedReviewTUISubmitsCompleteDispositionSet(t *testing.T) {
	sock, client, snapshot := captureRespond(t)
	m := newBoundedReviewTUIModel(t)
	m.socketPath = sock
	m.client = client
	m.findingDispositions[types.StepReview] = map[string]types.FindingDisposition{
		"review-1": {Decision: types.FindingDispositionFix, Reason: "reproduced"},
		"review-2": {Decision: types.FindingDispositionReject, Reason: "caller excludes state"},
	}

	cmd := m.respondCmd(types.ActionFix)
	if cmd == nil {
		t.Fatal("complete bounded dispositions did not enable submission")
	}
	if msg := cmd(); msg != nil {
		t.Fatalf("respond returned %#v", msg)
	}
	calls := snapshot()
	if len(calls) != 1 {
		t.Fatalf("respond calls = %d, want one", len(calls))
	}
	if !slices.Equal(calls[0].FindingIDs, []string{"review-1"}) {
		t.Fatalf("finding IDs = %v, want only confirmed fix", calls[0].FindingIDs)
	}
	if len(calls[0].Dispositions) != 2 || calls[0].Dispositions["review-2"].Decision != types.FindingDispositionReject {
		t.Fatalf("dispositions = %+v", calls[0].Dispositions)
	}
}

func TestBoundedReviewTUIRejectsIncompleteAndYoloResponses(t *testing.T) {
	m := newBoundedReviewTUIModel(t)
	m.findingDispositions[types.StepReview] = map[string]types.FindingDisposition{
		"review-1": {Decision: types.FindingDispositionFix, Reason: "reproduced"},
	}
	if cmd := m.respondCmd(types.ActionFix); cmd != nil {
		t.Fatal("incomplete bounded dispositions enabled submission")
	}
	m.yoloMode = true
	if cmd := m.maybeAutoApproveCmd(); cmd != nil {
		t.Fatal("yolo inferred bounded Review dispositions")
	}

	m.width = 120
	m.height = 50
	view := stripANSI(m.View())
	if !strings.Contains(view, "v disposition") || !strings.Contains(view, "dispositioned 1/2") {
		t.Fatalf("bounded Review action guidance missing:\n%s", view)
	}
	if strings.Contains(view, "a approve") || strings.Contains(view, "s skip") {
		t.Fatalf("bounded Review exposed bypass actions:\n%s", view)
	}
}
