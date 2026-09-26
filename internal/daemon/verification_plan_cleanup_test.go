package daemon

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/verificationplan"
)

func TestReleaseVerificationPlanPreservesRunOwnership(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	repo, err := d.InsertRepo(t.TempDir(), "https://example.com/repo.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	mgr := &RunManager{paths: p, db: d}
	srv := ipc.NewServer()
	registerHandlers(srv, mgr, d, func() {})
	socketRoot, err := os.MkdirTemp("", "nm-cleanup-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(socketRoot)
	socket := paths.WithRoot(socketRoot).Socket()
	if err := srv.Listen(socket); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- srv.ServeReady() }()
	defer func() { srv.Close(); <-done }()
	client, err := ipc.Dial(socket)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	source := filepath.Join(t.TempDir(), "plan.txt")
	if err := os.WriteFile(source, []byte("private verification evidence"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, owned := range []bool{false, true} {
		plan, err := verificationplan.Capture(p.RunInputsDir(), source, repo.ID, "feature", "head")
		if err != nil {
			t.Fatal(err)
		}
		if owned {
			_, err := d.InsertRunWithIntentAndLaunchNonce(repo.ID, "feature", "head", "base", nil, "", "", "", "", false, plan)
			if err != nil {
				t.Fatal(err)
			}
		}
		request := &ipc.ReleaseVerificationPlanParams{CaptureID: plan.ID, RepoID: repo.ID, Branch: "wrong", HeadSHA: "head"}
		if !owned {
			if err := client.Call(ipc.MethodReleaseVerificationPlan, request, nil); err == nil {
				t.Fatal("wrong branch accepted")
			}
			if _, err := plan.Read(); err != nil {
				t.Fatal(err)
			}
		}
		request.Branch = "feature"
		if err := client.Call(ipc.MethodReleaseVerificationPlan, request, nil); err != nil {
			t.Fatal(err)
		}
		if owned {
			if _, err := plan.Read(); err != nil {
				t.Fatalf("owned capture removed: %v", err)
			}
			continue
		}
		if _, err := os.Stat(filepath.Dir(plan.Path)); !os.IsNotExist(err) {
			t.Fatalf("abandoned capture retained: %v", err)
		}
	}
}
