package agent

import (
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestValidateInvocationRequestRejectsMixedOrIncompleteScopes(t *testing.T) {
	tests := []struct {
		name  string
		scope types.InvocationScope
	}{
		{name: "pipeline missing round", scope: types.InvocationScope{Kind: types.InvocationScopePipeline, RunID: "run", StepResultID: "step"}},
		{name: "utility missing ID", scope: types.InvocationScope{Kind: types.InvocationScopeUtility}},
		{name: "utility with pipeline ID", scope: types.InvocationScope{Kind: types.InvocationScopeUtility, UtilityScopeID: "utility", RunID: "run"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request := InvocationRequest{Purpose: types.PurposeInitialReview, Scope: tt.scope}
			if err := ValidateInvocationRequest(request); err == nil {
				t.Fatal("ValidateInvocationRequest() error = nil")
			}
		})
	}
}
