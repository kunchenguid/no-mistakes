package db

import (
	"reflect"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestRunSkipPolicyIsDurableAndImmutable(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/project", "https://example.com/repo", "main")
	r, err := d.InsertRun(repo.ID, "feature", "abc", "base")
	if err != nil {
		t.Fatal(err)
	}
	if r.SkipStepsJSON != nil {
		t.Fatal("legacy row claims skip evidence")
	}
	want := []types.StepName{types.StepPush, types.StepPR, types.StepCI}
	if err := d.SetRunSkippedSteps(r.ID, want); err != nil {
		t.Fatal(err)
	}
	stored, err := d.GetRun(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	got, err := stored.RequestedSkips()
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("policy = %v, %v", got, err)
	}
	if err := d.SetRunSkippedSteps(r.ID, nil); err == nil {
		t.Fatal("pinned policy changed")
	}
	active, _ := d.InsertRun(repo.ID, "other", "abc", "base")
	if err := d.UpdateRunStatus(active.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	if err := d.SetRunSkippedSteps(active.ID, want); err == nil {
		t.Fatal("active legacy policy changed")
	}
}

func TestMalformedRunSkipPolicyFailsClosed(t *testing.T) {
	for _, raw := range []string{`null`, `{}`, `[`, `["unknown"]`, `["push","push"]`, `[null]`, `["gate.review.foo"]`} {
		t.Run(raw, func(t *testing.T) {
			r := &Run{ID: "bad-policy", SkipStepsJSON: &raw}
			if _, err := r.RequestedSkips(); err == nil {
				t.Fatal("malformed policy accepted")
			}
		})
	}
}
