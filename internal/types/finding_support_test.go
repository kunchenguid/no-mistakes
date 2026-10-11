package types

import (
	"reflect"
	"testing"
)

func TestFindingSupportValidate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		support FindingSupport
		valid   bool
	}{
		{"source", FindingSupport{ClaimType: FindingClaimSource, Source: &FindingSourceSupport{Path: "a.go", Line: 12, Quote: "return err"}}, true},
		{"historical source context", FindingSupport{ClaimType: FindingClaimSource, Source: &FindingSourceSupport{Path: "a.go", Line: 12, Quote: "return err", HeadSHA: "abcdef", RunID: "run-1", ObservedAt: "2026-09-29T12:00:00Z"}}, true},
		{"test", FindingSupport{ClaimType: FindingClaimTest, Test: &FindingTestSupport{Command: "go test ./internal/types"}}, true},
		{"ci", FindingSupport{ClaimType: FindingClaimCI, CI: &FindingCISupport{CheckID: "check-42", HeadSHA: "abcdef"}}, true},
		{"runtime", FindingSupport{ClaimType: FindingClaimRuntime, Runtime: &FindingRuntimeSupport{Entity: "order-42", Transition: "pending->filled", TransitionAt: "2026-09-29T11:59:00Z", ObservedAt: "2026-09-29T12:00:00Z", Revision: "0123456789abcdef0123456789abcdef01234567"}}, true},
		{"unknown kind", FindingSupport{ClaimType: "anecdote", Source: &FindingSourceSupport{Path: "a.go", Line: 1, Quote: "x"}}, false},
		{"missing detail", FindingSupport{ClaimType: FindingClaimSource}, false},
		{"mixed detail", FindingSupport{ClaimType: FindingClaimSource, Source: &FindingSourceSupport{Path: "a.go", Line: 1, Quote: "x"}, Test: &FindingTestSupport{Command: "go test"}}, false},
		{"source missing quote", FindingSupport{ClaimType: FindingClaimSource, Source: &FindingSourceSupport{Path: "a.go", Line: 1}}, false},
		{"source invalid line", FindingSupport{ClaimType: FindingClaimSource, Source: &FindingSourceSupport{Path: "a.go", Quote: "x"}}, false},
		{"source invalid time", FindingSupport{ClaimType: FindingClaimSource, Source: &FindingSourceSupport{Path: "a.go", Line: 1, Quote: "x", ObservedAt: "yesterday"}}, false},
		{"test missing command", FindingSupport{ClaimType: FindingClaimTest, Test: &FindingTestSupport{}}, false},
		{"ci missing head", FindingSupport{ClaimType: FindingClaimCI, CI: &FindingCISupport{CheckID: "check-42"}}, false},
		{"runtime missing time", FindingSupport{ClaimType: FindingClaimRuntime, Runtime: &FindingRuntimeSupport{Entity: "order-42", Transition: "pending->filled"}}, false},
		{"runtime invalid time", FindingSupport{ClaimType: FindingClaimRuntime, Runtime: &FindingRuntimeSupport{Entity: "order-42", Transition: "pending->filled", ObservedAt: "yesterday"}}, false},
		{"runtime missing transition time", FindingSupport{ClaimType: FindingClaimRuntime, Runtime: &FindingRuntimeSupport{Entity: "order-42", Transition: "pending->filled", ObservedAt: "2026-09-29T12:00:00Z", Revision: "0123456789abcdef0123456789abcdef01234567"}}, false},
		{"runtime missing revision", FindingSupport{ClaimType: FindingClaimRuntime, Runtime: &FindingRuntimeSupport{Entity: "order-42", Transition: "pending->filled", TransitionAt: "2026-09-29T11:59:00Z", ObservedAt: "2026-09-29T12:00:00Z"}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.support.Validate(); (got == nil) != tt.valid {
				t.Fatalf("Validate() = %v, want valid=%v", got, tt.valid)
			}
		})
	}
}

func TestFindingSupportSurvivesJSONAndFindingCopies(t *testing.T) {
	t.Parallel()
	for _, support := range []*FindingSupport{
		{ClaimType: FindingClaimSource, Source: &FindingSourceSupport{Path: "a.go", Line: 12, Quote: "return err", HeadSHA: "abcdef", RunID: "run-1", ObservedAt: "2026-09-29T12:00:00Z"}},
		{ClaimType: FindingClaimTest, Test: &FindingTestSupport{Command: "go test ./internal/types"}},
		{ClaimType: FindingClaimCI, CI: &FindingCISupport{CheckID: "check-42", HeadSHA: "abcdef"}},
		{ClaimType: FindingClaimRuntime, Runtime: &FindingRuntimeSupport{Entity: "order-42", Transition: "pending->filled", TransitionAt: "2026-09-29T11:59:00Z", ObservedAt: "2026-09-29T12:00:00Z", Revision: "0123456789abcdef0123456789abcdef01234567"}},
	} {
		original := Findings{Items: []Finding{{ID: "f-1", Severity: FindingSeverityError, Description: "claim", Action: ActionAutoFix, Support: support}}}
		encoded, err := MarshalFindingsJSON(original)
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := ParseFindingsJSON(encoded)
		if err != nil {
			t.Fatal(err)
		}
		for name, got := range map[string]Findings{
			"json":      decoded,
			"normalize": NormalizeFindings(decoded, "review"),
			"filter":    FilterFindings(decoded, []string{"f-1"}),
			"exclude":   ExcludeFindings(decoded, []string{"other"}),
			"autofix":   AutoFixableFindings(decoded),
			"override":  MergeUserOverrides(decoded, map[string]string{"f-1": "note"}, nil),
		} {
			if len(got.Items) != 1 || !reflect.DeepEqual(got.Items[0].Support, support) {
				t.Fatalf("%s support = %+v, want %+v", name, got.Items, support)
			}
		}
	}
}

func TestReviewSupportClaimIDIgnoresOwnerResultAndBindsIdentity(t *testing.T) {
	t.Parallel()
	f := Finding{ID: "review-1", Support: &FindingSupport{ClaimType: FindingClaimTest, Test: &FindingTestSupport{Command: "go test ./..."}}}
	key := ReviewSupportClaimID(f)
	if key == "" {
		t.Fatal("empty claim key")
	}
	f.Support.OwnerResult = &FindingOwnerResult{ReviewFindingID: "review-1", HeadSHA: "head", TargetSHA: "target", DiffDigest: "digest", Generation: 1, ObservedAt: "2026-09-29T12:00:00Z", Disposition: "disproven"}
	encoded, err := MarshalFindingsJSON(Findings{Items: []Finding{f}})
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseFindingsJSON(encoded)
	if err != nil || len(parsed.Items) != 1 || !reflect.DeepEqual(parsed.Items[0].Support.OwnerResult, f.Support.OwnerResult) {
		t.Fatalf("owner result JSON round-trip = %+v, %v", parsed.Items, err)
	}
	if got := ReviewSupportClaimID(f); got != key {
		t.Fatalf("owner result changed claim key: %q != %q", got, key)
	}
	f.ID = "review-2"
	if got := ReviewSupportClaimID(f); got == key {
		t.Fatal("different review finding reused claim key")
	}
}
