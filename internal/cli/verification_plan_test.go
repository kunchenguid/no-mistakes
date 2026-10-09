package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/verificationplan"
	"github.com/spf13/cobra"
	toon "github.com/toon-format/toon-go"
)

func TestVerificationPlanStatusIncludesSnapshotOrExplicitAbsence(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	repo, err := d.InsertRepo(t.TempDir(), "https://example.com/repo.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	source := filepath.Join(root, "plan.txt")
	if err := os.WriteFile(source, []byte("verify observable output\n"), 0600); err != nil {
		t.Fatal(err)
	}
	plan, err := verificationplan.Capture(filepath.Join(root, "inputs"), source, repo.ID, "feature", "head")
	if err != nil {
		t.Fatal(err)
	}
	run, err := d.InsertRunWithIntentAndLaunchNonce(repo.ID, "feature", "head", "base", nil, "", "", "", "", false, plan)
	if err != nil {
		t.Fatal(err)
	}
	run, err = d.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, view := range []runView{runViewFromDB(run, nil, d), runViewFromIPC(&ipc.RunInfo{ID: run.ID, VerificationPlan: plan})} {
		var out bytes.Buffer
		cmd := &cobra.Command{}
		cmd.SetOut(&out)
		emitDoc(cmd, runObjectField(view))
		var doc struct {
			Run struct {
				Plan struct {
					Path       string `toon:"path"`
					SourcePath string `toon:"source_path"`
					SHA256     string `toon:"sha256"`
					CapturedAt int64  `toon:"captured_at"`
				} `toon:"verification_plan"`
			} `toon:"run"`
		}
		if err := toon.Unmarshal(out.Bytes(), &doc); err != nil {
			t.Fatal(err)
		}
		got := doc.Run.Plan
		if got.Path != plan.Path || got.SHA256 != plan.SHA256 || got.SourcePath != plan.SourcePath || got.CapturedAt != plan.CapturedAt {
			t.Fatalf("status attachment: %+v", got)
		}
	}
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	emitDoc(cmd, runObjectField(runView{ID: "legacy"}))
	var doc struct {
		Run struct {
			Plan string `toon:"verification_plan"`
		} `toon:"run"`
	}
	if err := toon.Unmarshal(out.Bytes(), &doc); err != nil || doc.Run.Plan != "none" {
		t.Fatalf("absence not explicit: %s %v", out.String(), err)
	}
}

func TestVerificationPlanRefusesOlderDaemonBeforeLaunch(t *testing.T) {
	launched := olderDaemonFixture(t, func() (interface{}, error) { return &ipc.ProbeOmitIntentResult{OK: true}, nil })
	writeGlobalConfig(t, "")
	source := filepath.Join(t.TempDir(), "plan.txt")
	if err := os.WriteFile(source, []byte("verify output\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := newAxiRunCmd()
	cmd.SetContext(context.Background())
	cmd.SetArgs([]string{"--intent", "preserve goal", "--verification-plan", source})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	if err := cmd.Execute(); err == nil || !strings.Contains(out.String(), "capture verification plan before push") {
		t.Fatalf("old daemon accepted: %v\n%s", err, out.String())
	}
	if len(*launched) != 0 {
		t.Fatalf("old daemon took custody: %v", *launched)
	}
}

func TestVerificationPlanFailedLaunchReleasesCapture(t *testing.T) {
	var captured ipc.CaptureVerificationPlanParams
	var released ipc.ReleaseVerificationPlanParams
	var snapshot *verificationplan.Snapshot
	olderDaemonFixture(t, nil, func(srv *ipc.Server) {
		srv.Handle(ipc.MethodCaptureVerificationPlan, func(_ context.Context, raw json.RawMessage) (interface{}, error) {
			if err := json.Unmarshal(raw, &captured); err != nil {
				return nil, err
			}
			p, err := paths.New()
			if err != nil {
				return nil, err
			}
			snapshot, err = verificationplan.Capture(p.RunInputsDir(), captured.SourcePath, captured.RepoID, captured.Branch, captured.HeadSHA)
			return snapshot, err
		})
		srv.Handle(ipc.MethodReleaseVerificationPlan, func(_ context.Context, raw json.RawMessage) (interface{}, error) {
			return nil, json.Unmarshal(raw, &released)
		})
	})
	writeGlobalConfig(t, "")
	cmd := newAxiRunCmd()
	cmd.SetContext(context.Background())
	source := filepath.Join(t.TempDir(), "plan.txt")
	if err := os.WriteFile(source, []byte("verify output"), 0600); err != nil {
		t.Fatal(err)
	}
	cmd.SetArgs([]string{"--intent", "preserve goal", "--verification-plan", source})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	if err := cmd.Execute(); err == nil {
		t.Fatal("launch unexpectedly succeeded")
	}
	if snapshot == nil || released.CaptureID != snapshot.ID || released.RepoID != captured.RepoID || released.Branch != captured.Branch || released.HeadSHA != captured.HeadSHA {
		t.Fatalf("failed launch did not release its capture: %+v; output: %s", released, out.String())
	}
}
