package main

import (
	"fmt"
	"strings"
	"testing"
)

func TestRequireActionRequiresCompletedRebase(t *testing.T) {
	const head = "0123456789abcdef0123456789abcdef01234567"
	const otherHead = "fedcba9876543210fedcba9876543210fedcba98"
	const body = `## Pipeline

Updates from [git push no-mistakes](https://github.com/kunchenguid/no-mistakes)

<!-- no-mistakes-pipeline-attestation:v1 {"head_sha":"%s","steps":[%s{"step":"review","status":"completed"},{"step":"test","status":"completed"},{"step":"document","status":"completed"}]} -->
`
	// Equal, ahead, and fast-forward Rebase outcomes all attest the same
	// completion status. The verifier only receives the final head and status.
	tests := []struct {
		name         string
		rebaseStatus string
		prHead       string
		wantFailure  string
	}{
		{name: "skipped", rebaseStatus: "skipped", prHead: head, wantFailure: "rebase (status=skipped)"},
		{name: "omitted", prHead: head, wantFailure: "rebase (missing)"},
		{name: "incomplete", rebaseStatus: "running", prHead: head, wantFailure: "rebase (status=running)"},
		{name: "equal head", rebaseStatus: "completed", prHead: head},
		{name: "already ahead", rebaseStatus: "completed", prHead: head},
		{name: "fast-forward", rebaseStatus: "completed", prHead: head},
		{name: "different head", rebaseStatus: "completed", prHead: otherHead, wantFailure: "does not match"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var rebase string
			if tc.rebaseStatus != "" {
				rebase = fmt.Sprintf(`{"step":"rebase","status":%q},`, tc.rebaseStatus)
			}
			got := runRequireAction(t, actionRun{body: fmt.Sprintf(body, head, rebase), headSHA: tc.prHead})
			if tc.wantFailure == "" {
				if got.conclusion != "success" || got.outputs["compliant"] != "true" {
					t.Fatalf("expected compliant success, got %q and compliant=%q\n%s", got.conclusion, got.outputs["compliant"], got.output)
				}
				return
			}
			if got.conclusion != "failure" || got.outputs["compliant"] != "false" || !strings.Contains(got.output, tc.wantFailure) {
				t.Fatalf("expected a noncompliant failure naming %q, got %q and compliant=%q\n%s", tc.wantFailure, got.conclusion, got.outputs["compliant"], got.output)
			}
		})
	}
}
