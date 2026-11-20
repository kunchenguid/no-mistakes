package db

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/callerrestrictions"
	"github.com/kunchenguid/no-mistakes/internal/integration"
)

func TestCallerRestrictionsAreImmutableAndRunBound(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepo(t.TempDir(), "https://example.test/repo.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, branch := range []string{"one", "two"} {
		path := filepath.Join(t.TempDir(), "caller.txt")
		if err := os.WriteFile(path, []byte("restrictions for "+branch+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		r, err := callerrestrictions.Capture(path)
		if err != nil {
			t.Fatal(err)
		}
		run, err := d.InsertRunWithCallerRestrictions(repo.ID, branch, "submitted-"+branch, "base", nil, "", "", "", "", false, nil, RunPRText{}, nil, r)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, run.ID)
		if _, err := d.sql.Exec(`UPDATE runs SET caller_restrictions = NULL WHERE id = ?`, run.ID); err == nil {
			t.Fatal("immutable record removed")
		}
	}
	for i, id := range ids {
		run, err := d.GetRun(id)
		if err != nil {
			t.Fatal(err)
		}
		branch := []string{"one", "two"}[i]
		if run.CallerRestrictions == nil || run.CallerRestrictions.Content != "restrictions for "+branch+"\n" || *run.SubmittedHeadSHA != "submitted-"+branch {
			t.Fatalf("cross-run record mismatch: %+v", run)
		}
	}
	unset, err := d.InsertRun(repo.ID, "unset", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	// An absent record also cannot acquire authority from later evidence.
	first, _ := d.GetRun(ids[0])
	if _, err := d.sql.Exec(`UPDATE runs SET caller_restrictions = ? WHERE id = ?`, first.CallerRestrictions, unset.ID); err == nil {
		t.Fatal("late caller authority accepted")
	}
}

func TestCallerRestrictionsAndIntegrationBindTheSameLaunch(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepo(t.TempDir(), "https://example.test/repo.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "caller.txt")
	if err := os.WriteFile(path, []byte("Use only the private Linux guest.\n"), 0600); err != nil {
		t.Fatal(err)
	}
	restrictions, err := callerrestrictions.Capture(path)
	if err != nil {
		t.Fatal(err)
	}
	destination := &integration.Destination{
		Remote: "custom", URL: "https://github.com/customer/repo.git", TransportURL: "https://github.com/customer/repo.git",
		DefaultBranch: "main", Key: "abcdefghijklmnopqrstuvwx",
	}
	run, err := d.InsertRunWithDestinationAndCallerRestrictions(repo.ID, "topic", "head", "base", nil, "nonce", "generation", "intent-digest", "", false, nil, RunPRText{}, nil, destination, restrictions)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := d.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.CallerRestrictions == nil || *stored.CallerRestrictions != *restrictions || stored.Integration == nil || *stored.Integration != *destination {
		t.Fatalf("creation did not preserve both records: %+v", stored)
	}
	for _, mismatch := range []struct{ remote, digest string }{
		{"other", restrictions.Digest()},
		{"custom", "different-restrictions"},
	} {
		_, claimed, _ := d.ClaimLaunchReceiptWithDestinationAndCallerRestrictions(repo.ID, "topic", "nonce", "head", "generation", "intent-digest", "", false, "", mismatch.remote, mismatch.digest)
		if claimed {
			t.Fatalf("mismatched request consumed receipt: %+v", mismatch)
		}
		stored, err := d.GetRun(run.ID)
		if err != nil || stored.LaunchReceiptClaimedAt != nil {
			t.Fatalf("mismatch changed receipt: %+v, %v", stored, err)
		}
	}
	stored, claimed, err := d.ClaimLaunchReceiptWithDestinationAndCallerRestrictions(repo.ID, "topic", "nonce", "head", "generation", "intent-digest", "", false, "", "custom", restrictions.Digest())
	if err != nil || !claimed || stored.ID != run.ID {
		t.Fatalf("matching request did not claim exact run: %+v, %t, %v", stored, claimed, err)
	}
}
