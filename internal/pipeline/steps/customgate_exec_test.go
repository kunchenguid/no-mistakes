package steps

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func recordCustomGateCommandStep(t *testing.T, sctx *pipeline.StepContext) {
	t.Helper()
	step, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepName("gate.test.local-ci"))
	if err != nil {
		t.Fatal(err)
	}
	sctx.StepResultID = step.ID
	if err := sctx.DB.StartStep(step.ID); err != nil {
		t.Fatal(err)
	}
}

func TestCustomGateCommandCannotRunBeforeOwnershipIsRecorded(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "mock"}, dir, base, head, config.Commands{})
	recordCustomGateCommandStep(t, sctx)
	if err := sctx.DB.Close(); err != nil {
		t.Fatal(err)
	}
	command := "echo ran > must-not-exist"
	_, _, err := runCustomGateCommand(sctx, command, t.TempDir())
	if err == nil {
		t.Fatal("gate ran without recording process ownership")
	}
	if _, err := os.Stat(filepath.Join(dir, "must-not-exist")); !os.IsNotExist(err) {
		t.Fatalf("gate executed before registration: %v", err)
	}
}

func TestCustomGateCommandPreservesOutputAndExitStatus(t *testing.T) {
	for _, tc := range []struct {
		command string
		code    int
	}{
		{"echo gate-output", 0},
		{"exit 7", 7},
	} {
		t.Run(tc.command, func(t *testing.T) {
			dir, base, head := setupGitRepo(t)
			sctx := newTestContextWithDBRecords(t, &mockAgent{name: "mock"}, dir, base, head, config.Commands{})
			recordCustomGateCommandStep(t, sctx)
			output, code, err := runCustomGateCommand(sctx, tc.command, t.TempDir())
			if err != nil || code != tc.code {
				t.Fatalf("command result = %q, %d, %v", output, code, err)
			}
			if tc.code == 0 && strings.TrimSpace(output) != "gate-output" {
				t.Fatalf("lost gate output: %q", output)
			}
			steps, err := sctx.DB.GetStepsByRun(sctx.Run.ID)
			if err != nil || steps[0].AgentPID != nil {
				t.Fatalf("finished gate retained ownership: %+v, %v", steps, err)
			}
		})
	}
}

func TestCustomGateCommandCancellationClearsOwnership(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX cancellation probe; Windows cooperative cleanup has shellenv coverage")
	}
	dir, base, head := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "mock"}, dir, base, head, config.Commands{})
	recordCustomGateCommandStep(t, sctx)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx
	done := make(chan error, 1)
	go func() {
		_, _, err := runCustomGateCommand(sctx, "echo started > started; while :; do sleep 1; done", t.TempDir())
		done <- err
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(dir, "started")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("gate never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	steps, err := sctx.DB.GetStepsByRun(sctx.Run.ID)
	if err != nil || len(steps) != 1 || steps[0].AgentPID == nil {
		t.Fatalf("gate started without process ownership: %+v, %v", steps, err)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("gate command did not stop on cancellation")
	}
	steps, err = sctx.DB.GetStepsByRun(sctx.Run.ID)
	if err != nil || steps[0].AgentPID != nil {
		t.Fatalf("gate left process ownership after cancellation: %+v, %v", steps, err)
	}
}
