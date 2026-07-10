package types

import "testing"

func TestPurposeRegistryCoversApprovedInvocationFamilies(t *testing.T) {
	want := []Purpose{
		PurposeInitialReview,
		PurposeStructuredFindingRepair,
		PurposeIntentSensitiveRepair,
		PurposeUnstructuredTestRepair,
		PurposeUnstructuredCIRepair,
		PurposeUnstructuredConflictRepair,
		PurposeTestEvidence,
		PurposeLintInspection,
		PurposeDocumentationAuthoring,
		PurposeDocumentationVerification,
		PurposePRComposition,
		PurposeIntentSummarization,
		PurposeIntentDisambiguation,
		PurposeBranchCommitSuggestion,
		PurposeNormalAggregateVerification,
		PurposeEscalatedAggregateVerification,
	}
	definitions := AllPurposeDefinitions()
	if len(definitions) != len(want) {
		t.Fatalf("Purpose registry has %d entries, want %d", len(definitions), len(want))
	}
	for i, purpose := range want {
		if definitions[i].Purpose != purpose {
			t.Fatalf("Purpose registry entry %d = %q, want %q", i, definitions[i].Purpose, purpose)
		}
		if definitions[i].Role != InvocationRoleFixer && definitions[i].Role != InvocationRoleVerifier {
			t.Fatalf("Purpose %q has invalid role %q", purpose, definitions[i].Role)
		}
		if registered, err := PurposeDefinitionFor(purpose); err != nil || registered != definitions[i] {
			t.Fatalf("PurposeDefinitionFor(%q) = %+v, %v; want %+v", purpose, registered, err, definitions[i])
		}
	}

	testEvidence, err := PurposeDefinitionFor(PurposeTestEvidence)
	if err != nil {
		t.Fatalf("PurposeDefinitionFor(test evidence): %v", err)
	}
	if testEvidence.Role != InvocationRoleFixer {
		t.Fatalf("test evidence role = %q, want fixer because it may write or repair tests", testEvidence.Role)
	}

	definitions[0].Purpose = "mutated-copy"
	registered, err := PurposeDefinitionFor(PurposeInitialReview)
	if err != nil || registered.Purpose != PurposeInitialReview {
		t.Fatalf("caller mutated registry through returned slice: %+v, %v", registered, err)
	}
}
