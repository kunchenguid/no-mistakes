package cli

import (
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"path/filepath"
	"strings"
	"testing"
)

func TestFixProgressStatusOnlineAndDisconnectedAgree(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	repo, err := d.InsertRepo("/fixture", "https://example.com/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := d.InsertRun(repo.ID, "feature", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	run.Status = types.RunFailed
	p, err := d.BeginWorkRescue(run, "review", "", run.HeadSHA, "/retained")
	if err != nil {
		t.Fatal(err)
	}
	p.State = "saved"
	p.Ref = "refs/no-mistakes/rescue/" + run.ID + "/review/" + p.StopID
	p.SHA = "full-snapshot-sha"
	if err = d.SaveWorkRescue(p); err != nil {
		t.Fatal(err)
	}
	online := runViewFromIPC(&ipc.RunInfo{ID: run.ID, Branch: run.Branch, Status: run.Status, HeadSHA: run.HeadSHA, PartialWork: p})
	offline := runViewFromDB(run, nil, d)
	a, b := axiDoc(runObjectField(online)), axiDoc(runObjectField(offline))
	if a != b {
		t.Fatalf("online/offline differ:\n%s\n%s", a, b)
	}
	for _, want := range []string{"partial_work", p.Ref, p.SHA, "saved", "failed"} {
		if !strings.Contains(a, want) {
			t.Errorf("status missing %q: %s", want, a)
		}
	}
}
