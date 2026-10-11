package types

import "testing"

func TestValidateExternalCIOwnerRequestRequiresExactSkips(t *testing.T) {
	want := []StepName{StepPush, StepPR, StepCI}
	if err := ValidateExternalCIOwnerRequest(ExternalCIOwnerControllerShipPR, want); err != nil {
		t.Fatal(err)
	}
	if err := ValidateExternalCIOwnerRequest(ExternalCIOwnerControllerShipPR, []StepName{StepCI, StepPush, StepPR}); err != nil {
		t.Fatal(err)
	}
	for _, skips := range [][]StepName{nil, {StepPush, StepPR}, {StepPush, StepPR, StepReview}, {StepPush, StepPR, StepCI, StepCI}} {
		if err := ValidateExternalCIOwnerRequest(ExternalCIOwnerControllerShipPR, skips); err == nil {
			t.Fatalf("accepted skips %v", skips)
		}
	}
	if err := ValidateExternalCIOwnerRequest("unknown", want); err == nil {
		t.Fatal("accepted unknown owner")
	}
	if err := ValidateExternalCIOwnerRequest("", nil); err != nil {
		t.Fatal(err)
	}
}
