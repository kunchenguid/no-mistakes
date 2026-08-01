package pipeline

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// D5: every state-changing event is emitted AFTER its database write.
//
// This is the premise the whole subscription overflow contract rests on. The
// daemon stamps a snapshot with a revision sampled before it reads the
// database; that is only sound if a state event's write has already landed by
// the time the event exists. If any emitter ever moved ahead of its write, a
// subscriber could reconcile a snapshot that is older than the revision it was
// stamped with and then discard the delta that would have repaired it.
func TestExecutor_StateEventsAreEmittedAfterTheirDatabaseWrite(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	var mu sync.Mutex
	var violations []string
	var stateEvents int

	onEvent := func(e ipc.Event) {
		if ipc.ClassOf(e.Type) != ipc.ClassState {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		stateEvents++
		if e.Status == nil {
			return
		}
		if e.StepName != nil {
			steps, err := database.GetStepsByRun(run.ID)
			if err != nil {
				violations = append(violations, fmt.Sprintf("%s: read steps: %v", e.Type, err))
				return
			}
			found := false
			for _, s := range steps {
				if s.StepName != *e.StepName {
					continue
				}
				found = true
				if string(s.Status) != *e.Status {
					violations = append(violations, fmt.Sprintf(
						"%s(%s): event says %q but the row still says %q",
						e.Type, *e.StepName, *e.Status, s.Status))
				}
			}
			if !found {
				violations = append(violations, fmt.Sprintf("%s: no row for step %s", e.Type, *e.StepName))
			}
			return
		}
		current, err := database.GetRun(run.ID)
		if err != nil {
			violations = append(violations, fmt.Sprintf("%s: read run: %v", e.Type, err))
			return
		}
		if string(current.Status) != *e.Status {
			violations = append(violations, fmt.Sprintf(
				"%s: event says %q but the run row still says %q", e.Type, *e.Status, current.Status))
		}
	}

	steps := []Step{
		newPassStep(types.StepReview),
		newPassStep(types.StepTest),
		newFailStep(types.StepLint, fmt.Errorf("lint blew up")),
	}
	exec := NewExecutor(database, p, nil, nil, steps, onEvent)
	_ = exec.Execute(context.Background(), run, repo, workDir)

	mu.Lock()
	defer mu.Unlock()
	if stateEvents == 0 {
		t.Fatal("no state events observed; the ordering premise was not exercised")
	}
	if len(violations) > 0 {
		t.Fatalf("state events emitted before their database write:\n  %v", violations)
	}
}

// The skip path takes a different set of emitters, so exercise it too.
func TestExecutor_SkippedStepEventsAlsoFollowTheirDatabaseWrite(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	var mu sync.Mutex
	var violations []string
	onEvent := func(e ipc.Event) {
		if ipc.ClassOf(e.Type) != ipc.ClassState || e.StepName == nil || e.Status == nil {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		steps, err := database.GetStepsByRun(run.ID)
		if err != nil {
			violations = append(violations, err.Error())
			return
		}
		for _, s := range steps {
			if s.StepName == *e.StepName && string(s.Status) != *e.Status {
				violations = append(violations, fmt.Sprintf(
					"%s(%s): event %q, row %q", e.Type, *e.StepName, *e.Status, s.Status))
			}
		}
	}

	exec := NewExecutor(database, p, nil, nil, []Step{
		newPassStep(types.StepReview),
		newPassStep(types.StepTest),
	}, onEvent)
	exec.SetSkippedSteps([]types.StepName{types.StepTest})
	_ = exec.Execute(context.Background(), run, repo, workDir)

	mu.Lock()
	defer mu.Unlock()
	if len(violations) > 0 {
		t.Fatalf("skip-path events emitted before their database write:\n  %v", violations)
	}
}

func TestExecutor_ApprovalPersistenceFailureDoesNotPublishOrWaitAtGate(t *testing.T) {
	database, p, run, repo := setupTest(t)
	control, err := sql.Open("sqlite", p.DB()+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := control.Exec(`
		CREATE TRIGGER fail_gate_findings_update
		BEFORE UPDATE OF findings_json ON step_results
		BEGIN
			SELECT RAISE(FAIL, 'findings write failed');
		END
	`); err != nil {
		control.Close()
		t.Fatal(err)
	}
	if err := control.Close(); err != nil {
		t.Fatal(err)
	}
	var eventsMu sync.Mutex
	var events []ipc.Event
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(*StepContext) (*StepOutcome, error) {
			return &StepOutcome{
				NeedsApproval: true,
				Findings:      `{"items":[{"id":"review-1","severity":"error"}]}`,
			}, nil
		},
	}
	exec := NewExecutor(database, p, nil, nil, []Step{step}, func(event ipc.Event) {
		eventsMu.Lock()
		events = append(events, event)
		eventsMu.Unlock()
	})

	err = exec.Execute(context.Background(), run, repo, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "persist review approval gate") {
		t.Fatalf("Execute error = %v, want approval persistence failure", err)
	}
	if responseErr := exec.Respond(types.StepReview, types.ActionApprove, nil); responseErr == nil {
		t.Fatal("executor remained parked after approval persistence failed")
	}

	eventsMu.Lock()
	defer eventsMu.Unlock()
	for _, event := range events {
		if event.StepName == nil || *event.StepName != types.StepReview || event.Status == nil {
			continue
		}
		if *event.Status == string(types.StepStatusAwaitingApproval) || *event.Status == string(types.StepStatusFixReview) {
			t.Fatalf("unpersisted approval gate event was published: %#v", event)
		}
	}
}
