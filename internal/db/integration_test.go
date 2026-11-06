package db

import (
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/integration"
)

func TestIntegrationRemotePinPersistsAndBindsReceipt(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	repo, err := database.InsertRepo(t.TempDir(), "https://github.com/product/repo.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	destination := &integration.Destination{
		Remote: "custom", URL: "https://github.com/customer/repo.git", TransportURL: "https://github.com/customer/repo.git",
		DefaultBranch: "main", Key: "abcdefghijklmnopqrstuvwx",
	}
	run, err := database.InsertRunWithDestination(repo.ID, "feature", "head", "base", nil, "nonce", "generation", "digest", "release", false, nil, RunPRText{}, nil, destination)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := database.GetRun(run.ID)
	if err != nil || stored.Integration == nil || *stored.Integration != *destination {
		t.Fatalf("persisted destination = %+v, %v", stored, err)
	}
	_, claimed, err := database.ClaimLaunchReceiptWithDestination(repo.ID, "feature", "nonce", "head", "generation", "digest", "release", false, "", "other")
	if err != nil || claimed {
		t.Fatalf("conflicting destination consumed receipt: claimed=%t err=%v", claimed, err)
	}
	stored, err = database.GetRun(run.ID)
	if err != nil || stored.LaunchReceiptClaimedAt != nil {
		t.Fatalf("conflicting claim changed the row: %+v, %v", stored, err)
	}
	_, claimed, err = database.ClaimLaunchReceiptWithDestination(repo.ID, "feature", "nonce", "head", "generation", "digest", "release", false, "", "custom")
	if err != nil || !claimed {
		t.Fatalf("matching destination did not claim receipt: claimed=%t err=%v", claimed, err)
	}
	if _, err := database.sql.Exec(`UPDATE runs SET integration_destination = '{"remote":"custom"}' WHERE id = ?`, run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.GetRun(run.ID); err == nil {
		t.Fatal("malformed persisted destination must fail closed")
	}
}
