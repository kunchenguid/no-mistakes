package bitbucket

import (
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func TestNormalizePRState(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want scm.PRState
	}{
		{"open canonical", "OPEN", scm.PRStateOpen},
		{"open lowercase", "open", scm.PRStateOpen},
		{"open mixed case", "Open", scm.PRStateOpen},
		{"open with surrounding whitespace", "  OPEN  ", scm.PRStateOpen},
		{"merged canonical", "MERGED", scm.PRStateMerged},
		{"merged lowercase", "merged", scm.PRStateMerged},
		{"merged with whitespace", "\tMERGED\n", scm.PRStateMerged},
		{"declined canonical", "DECLINED", scm.PRStateClosed},
		{"declined lowercase", "declined", scm.PRStateClosed},
		{"closed canonical", "CLOSED", scm.PRStateClosed},
		{"closed lowercase", "closed", scm.PRStateClosed},
		{"superseded canonical", "SUPERSEDED", scm.PRStateClosed},
		{"superseded lowercase", "superseded", scm.PRStateClosed},

		// The default branch returns the raw string verbatim: no trim, no case fold.
		// Unknown lifecycle states pass through untouched so callers can surface them.
		{"unknown state passes through raw", "DRAFT", "DRAFT"},
		{"unknown state preserves original casing", "Draft", "Draft"},
		{"unknown state preserves original whitespace", "  Draft  ", "  Draft  "},
		{"empty string stays empty", "", ""},
		{"whitespace-only with no recognized token returns raw", "   ", "   "},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := normalizePRState(tt.raw)
			if got != tt.want {
				t.Fatalf("normalizePRState(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

func TestStatusName(t *testing.T) {
	tests := []struct {
		name   string
		status CommitStatus
		want   string
	}{
		{"name present", CommitStatus{Name: "build", Key: "build-key"}, "build"},
		{"name takes precedence over key", CommitStatus{Name: "build", Key: "other"}, "build"},
		{"name empty falls back to key", CommitStatus{Name: "", Key: "build-key"}, "build-key"},
		{"name whitespace-only falls back to key", CommitStatus{Name: "   ", Key: "build-key"}, "build-key"},
		{"name trimmed when returned", CommitStatus{Name: "  build  ", Key: ""}, "build"},
		{"key trimmed when used as fallback", CommitStatus{Name: "", Key: "  build-key  "}, "build-key"},
		{"both empty returns empty", CommitStatus{}, ""},
		{"both whitespace-only returns empty", CommitStatus{Name: "  ", Key: "  "}, ""},
		{"neither name nor key set but state populated returns empty", CommitStatus{State: "SUCCESSFUL"}, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := statusName(tt.status)
			if got != tt.want {
				t.Fatalf("statusName(%#v) = %q, want %q", tt.status, got, tt.want)
			}
		})
	}
}

func TestStatusProviderID(t *testing.T) {
	t.Parallel()

	if got := statusProviderID(CommitStatus{Key: "build-1", URL: "https://bitbucket.org/ws/repo/addon/pipelines/home#!/results/pipeline-1"}); got != "bitbucket-status:build-1" {
		t.Fatalf("statusProviderID() = %q, want status key identity", got)
	}
	if got := statusProviderID(CommitStatus{URL: "https://bitbucket.org/ws/repo/addon/pipelines/home#!/results/42"}); got != "bitbucket-pipeline-build:42" {
		t.Fatalf("statusProviderID() = %q, want pipeline identity fallback", got)
	}
}

func TestFailedPipelineBuildNumberTargetsPreservesExactSameNamedSelection(t *testing.T) {
	t.Parallel()

	statuses := []CommitStatus{
		{Name: "test", Key: "test-linux", State: "FAILED", URL: "https://bitbucket.org/ws/repo/addon/pipelines/home#!/results/41"},
		{Name: "test", Key: "test-macos", State: "FAILED", URL: "https://bitbucket.org/ws/repo/addon/pipelines/home#!/results/42"},
	}
	got, err := failedPipelineBuildNumberTargets(statuses, []scm.CheckTarget{{Name: "test", ProviderID: "bitbucket-status:test-macos"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("targets = %v, want one exact pipeline", got)
	}
	if _, ok := got["42"]; !ok {
		t.Fatalf("targets = %v, want build 42", got)
	}
}

func TestFailedPipelineBuildNumberTargetsFailsClosedWhenSelectionCannotResolve(t *testing.T) {
	t.Parallel()

	statuses := []CommitStatus{{Name: "test", Key: "test-linux", State: "FAILED", URL: "https://bitbucket.org/ws/repo/addon/pipelines/home#!/results/41"}}
	got, err := failedPipelineBuildNumberTargets(statuses, []scm.CheckTarget{{Name: "test", ProviderID: "bitbucket-status:missing"}})
	if err == nil || got != nil {
		t.Fatalf("failedPipelineBuildNumberTargets() = (%v, %v), want no targets and an error", got, err)
	}
}

func TestStatusBucket(t *testing.T) {
	tests := []struct {
		name  string
		state string
		want  scm.CheckBucket
	}{
		{"successful canonical", "SUCCESSFUL", scm.CheckBucketPass},
		{"success alias", "SUCCESS", scm.CheckBucketPass},
		{"successful lowercase", "successful", scm.CheckBucketPass},
		{"successful mixed case", "Successful", scm.CheckBucketPass},
		{"successful with whitespace", "  SUCCESSFUL  ", scm.CheckBucketPass},

		{"failed canonical", "FAILED", scm.CheckBucketFail},
		{"failure alias", "FAILURE", scm.CheckBucketFail},
		{"error alias", "ERROR", scm.CheckBucketFail},
		{"failed lowercase", "failed", scm.CheckBucketFail},

		{"stopped maps to cancel", "STOPPED", scm.CheckBucketCancel},
		{"stopped lowercase", "stopped", scm.CheckBucketCancel},

		{"inprogress no underscore", "INPROGRESS", scm.CheckBucketPending},
		{"in_progress with underscore", "IN_PROGRESS", scm.CheckBucketPending},
		{"pending", "PENDING", scm.CheckBucketPending},
		{"inprogress lowercase", "inprogress", scm.CheckBucketPending},

		{"unknown returns empty", "UNKNOWN", ""},
		{"empty returns empty", "", ""},
		{"whitespace-only returns empty", "   ", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := statusBucket(tt.state)
			if got != tt.want {
				t.Fatalf("statusBucket(%q) = %q, want %q", tt.state, got, tt.want)
			}
		})
	}
}

func TestPipelineBuildNumberFromStatusURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		raw  string
		want string
	}{
		{"https://bitbucket.org/ws/repo/pipelines/results/123", "123"},
		{"https://bitbucket.org/ws/repo/addon/pipelines/home#!/results/456", "456"},
		{"https://bitbucket.org/ws/repo/pipelines/results/not-a-number", ""},
		{"https://bitbucket.org/ws/repo/pipelines", ""},
	}
	for _, tt := range tests {
		if got := pipelineBuildNumberFromStatusURL(tt.raw); got != tt.want {
			t.Fatalf("pipelineBuildNumberFromStatusURL(%q) = %q, want %q", tt.raw, got, tt.want)
		}
	}
}
