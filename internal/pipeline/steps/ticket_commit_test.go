package steps

import (
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func stepContextForBranch(branch, pattern string) *pipeline.StepContext {
	return &pipeline.StepContext{
		Run:    &db.Run{Branch: branch},
		Config: &config.Config{TicketPrefixPattern: pattern},
	}
}

func TestDeterministicFixCommitMessage(t *testing.T) {
	t.Parallel()

	t.Run("ticket leads subject with step trace", func(t *testing.T) {
		t.Parallel()
		got := deterministicFixCommitMessage(stepContextForBranch("WEB-12345-readme", `WEB-\d+`), types.StepDocument, "drop stale key")
		want := "WEB-12345: drop stale key [no-mistakes/document]"
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})

	t.Run("no ticket keeps conventional subject", func(t *testing.T) {
		t.Parallel()
		got := deterministicFixCommitMessage(stepContextForBranch("docs/readme-refresh", `WEB-\d+`), types.StepDocument, "drop stale key")
		want := "no-mistakes(document): drop stale key"
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})
}

func TestFixedFixCommitMessage(t *testing.T) {
	t.Parallel()

	t.Run("ticket leads subject", func(t *testing.T) {
		t.Parallel()
		got := fixedFixCommitMessage(stepContextForBranch("WEB-7-x", `WEB-\d+`), "apply CI fixes")
		want := "WEB-7: apply CI fixes [no-mistakes]"
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})

	t.Run("no ticket keeps default subject", func(t *testing.T) {
		t.Parallel()
		got := fixedFixCommitMessage(stepContextForBranch("feature/x", `WEB-\d+`), "apply CI fixes")
		want := "no-mistakes: apply CI fixes"
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})
}
