package daemon

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestDLOCK31ShutdownPreservesResumedRepairAndEvidence(t *testing.T) {
	f := newDLOCK31Recovery(t)
	before, err := f.d.RetainedCIRepair(f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	evidence := filepath.Join(f.p.EvidenceDir(), f.run.ID, "proof.txt")
	if err := os.MkdirAll(filepath.Dir(evidence), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(evidence, []byte("retained evidence"), 0o600); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		plan, err := f.m.prepareRecoveredRun(context.Background(), f.run)
		if err != nil {
			t.Fatal(err)
		}
		f.m.resumeRecoveredRun(*plan)
		f.m.Shutdown()
		stored, err := f.d.GetRun(f.run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if stored.Status != types.RunRunning || stored.AwaitingAgentSince == nil || stored.HeadSHA != f.published {
			t.Fatalf("shutdown lost parked state: %+v", stored)
		}
		binding, err := pipeline.VerifyRetainedCIRepair(context.Background(), f.d, stored, plan.gateDir, f.work)
		if err != nil || !reflect.DeepEqual(before, binding) {
			t.Fatalf("shutdown lost binding: %+v %v", binding, err)
		}
		if contents, err := os.ReadFile(evidence); err != nil || string(contents) != "retained evidence" {
			t.Fatalf("lost evidence: %q %v", contents, err)
		}
		if f.review.execCnt.Load() != 0 || f.ci.execCnt.Load() != 0 {
			t.Fatal("shutdown reran a step")
		}
		f.run = stored
		f.m = NewRunManager(f.d, f.p, func() []pipeline.Step { return []pipeline.Step{f.review, f.ci} })
	}
}
