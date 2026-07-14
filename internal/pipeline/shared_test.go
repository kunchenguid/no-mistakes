package pipeline

import "testing"

func TestRunSharedDocumentationDecisionLifecycle(t *testing.T) {
	t.Parallel()
	shared := &RunShared{}
	if _, ok := shared.TakeDocumentationDecision(); ok {
		t.Fatal("empty shared state unexpectedly returned a documentation decision")
	}

	shared.SetDocumentationDecision(DocumentationDecision{Required: false, Rationale: "internal only", HeadSHA: "abc"})
	decision, ok := shared.TakeDocumentationDecision()
	if !ok || decision.Required || decision.Rationale != "internal only" || decision.HeadSHA != "abc" {
		t.Fatalf("unexpected decision: ok=%t decision=%+v", ok, decision)
	}
	if _, ok := shared.TakeDocumentationDecision(); ok {
		t.Fatal("documentation decision was not consumed")
	}

	shared.SetDocumentationDecision(DocumentationDecision{Required: false, Rationale: "stale"})
	shared.ClearDocumentationDecision()
	if _, ok := shared.TakeDocumentationDecision(); ok {
		t.Fatal("cleared documentation decision remained available")
	}
}
