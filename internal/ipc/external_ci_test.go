package ipc

import (
	"encoding/json"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestExternalCIOwnerAndPendingHandoffSurviveWire(t *testing.T) {
	owner := types.ExternalCIOwnerControllerShipPR
	for _, original := range []any{
		PushReceivedParams{ExternalCIOwner: owner},
		StartFreshRunParams{ExternalCIOwner: owner},
		ClaimLaunchReceiptParams{ExternalCIOwner: owner},
		RerunParams{ExternalCIOwner: owner},
		RunInfo{ExternalCIOwner: owner, PendingCISupport: []types.PendingCISupport{{ClaimID: "claim-1", HistoricalCheckID: "check-1", SourceHeadSHA: "head", Owner: owner}}},
	} {
		encoded, err := json.Marshal(original)
		if err != nil {
			t.Fatal(err)
		}
		switch original.(type) {
		case PushReceivedParams:
			var got PushReceivedParams
			if err := json.Unmarshal(encoded, &got); err != nil || got.ExternalCIOwner != owner {
				t.Fatalf("push wire: %+v, %v", got, err)
			}
		case StartFreshRunParams:
			var got StartFreshRunParams
			if err := json.Unmarshal(encoded, &got); err != nil || got.ExternalCIOwner != owner {
				t.Fatalf("fresh wire: %+v, %v", got, err)
			}
		case ClaimLaunchReceiptParams:
			var got ClaimLaunchReceiptParams
			if err := json.Unmarshal(encoded, &got); err != nil || got.ExternalCIOwner != owner {
				t.Fatalf("claim wire: %+v, %v", got, err)
			}
		case RerunParams:
			var got RerunParams
			if err := json.Unmarshal(encoded, &got); err != nil || got.ExternalCIOwner != owner {
				t.Fatalf("rerun wire: %+v, %v", got, err)
			}
		case RunInfo:
			var got RunInfo
			if err := json.Unmarshal(encoded, &got); err != nil || got.ExternalCIOwner != owner || len(got.PendingCISupport) != 1 || got.PendingCISupport[0].HistoricalCheckID != "check-1" {
				t.Fatalf("run wire: %+v, %v", got, err)
			}
		}
	}
}
