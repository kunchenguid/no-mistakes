package steps

import (
	"fmt"
	"log/slog"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/reviewqa"
)

// Bounds on the published review conversation. The PR body competes for a
// host-specific budget (Azure DevOps caps a whole description at 4000
// characters), and the conversation is a RECORD, not a gate: losing its tail
// to a tight budget is acceptable, losing the Pipeline attestation is not. So
// it is rendered inside the Pipeline section as an ordinary `### ` group, which
// the existing budget logic can drop whole, and it is bounded here as well so
// a long conversation cannot be the reason a body needs truncating.
const (
	maxPublishedConversationEntries = 12
	maxPublishedConversationChars   = 240
)

// buildReviewConversationSection renders the questions this run's reviewer
// asked, the answers it received, and who gave them.
//
// The answered half comes from the durable per-branch store rather than the
// run's evidence files, because that is the record that survives: the evidence
// directory is reaped, and a question answered in an earlier run of the same
// branch is still part of this change's conversation. Withdrawn questions come
// from the run's own conversation files, since a withdrawal is never persisted
// (it settles nothing and needs no answer) - they are listed so a reader can
// tell "the reviewer asked and then answered it itself" from "nobody asked".
//
// An unanswered question can still appear, and must: the review step never
// completes on its own while one is open, but a human may approve the gate over
// it, and that is exactly the case a reader of the PR needs to see. It is
// listed as unanswered rather than quietly omitted.
func buildReviewConversationSection(sctx *pipeline.StepContext) string {
	// Keyed on the setting, not on this run's evidence directory: a repository
	// that turned the conversation off must publish the body it published
	// before the feature existed, even where an earlier run left answers in the
	// branch store.
	if !reviewConversationEnabled(sctx) {
		return ""
	}
	answered := publishedRunAnswers(sctx)
	conv := publishedRunConversation(sctx)
	withdrawn := conv.Withdrawn()
	unanswered := conv.Open()
	if len(answered) == 0 && len(withdrawn) == 0 && len(unanswered) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("### Review conversation\n\n")
	shown := 0
	omitted := 0
	for _, a := range answered {
		if shown >= maxPublishedConversationEntries {
			omitted++
			continue
		}
		shown++
		fmt.Fprintf(&b, "- **Q:** %s\n", publishedConversationText(a.Question))
		who := publishedConversationText(a.AnsweredBy)
		if who == "" {
			who = "unattributed"
		}
		fmt.Fprintf(&b, "  **A** (%s)**:** %s\n", who, publishedConversationText(a.Answer))
	}
	// Unanswered before withdrawn: a question someone approved past is the
	// most consequential line in this section, so a length bound must not be
	// what drops it.
	for _, q := range unanswered {
		if shown >= maxPublishedConversationEntries {
			omitted++
			continue
		}
		shown++
		fmt.Fprintf(&b, "- **Q:** %s\n  **Unanswered:** the review gate was resolved with this question still open.\n", publishedConversationText(q.Question.Question))
	}
	for _, q := range withdrawn {
		if shown >= maxPublishedConversationEntries {
			omitted++
			continue
		}
		shown++
		fmt.Fprintf(&b, "- **Q:** %s\n  **Withdrawn by the reviewer:** %s\n", publishedConversationText(q.Question.Question), publishedConversationText(q.Reason))
	}
	if omitted > 0 {
		fmt.Fprintf(&b, "\n%d further review question(s) omitted for length.\n", omitted)
	}
	// The conversation quotes agent and human text, so it can carry a foreign
	// attestation marker; verify.py binds the FIRST marker in the raw body, and
	// this section is appended after the real one.
	return neutralizeAttestationMarkers(strings.TrimRight(b.String(), "\n"))
}

func publishedRunAnswers(sctx *pipeline.StepContext) []db.ReviewAnswer {
	if sctx == nil || sctx.DB == nil || sctx.Repo == nil || sctx.Run == nil {
		return nil
	}
	branch := strings.TrimSpace(sctx.Run.Branch)
	if sctx.Repo.ID == "" || branch == "" {
		return nil
	}
	answers, _, err := sctx.DB.GetBranchReviewAnswers(sctx.Repo.ID, branch, db.MaxBranchReviewAnswers)
	if err != nil {
		// A branch with nothing settled returns no rows and no error, so a
		// failure here silently drops the answered half of the published
		// conversation. Logged at ERROR rather than as a degradation.
		slog.Error("failed to read the review conversation; the PR body will omit every answered question", "run_id", sctx.Run.ID, "error", err)
		return nil
	}
	// GetBranchReviewAnswers is most-recent-first so its bound is a recency
	// window; publish oldest first so the section reads as a conversation.
	ordered := make([]db.ReviewAnswer, 0, len(answers))
	for i := len(answers) - 1; i >= 0; i-- {
		ordered = append(ordered, answers[i])
	}
	return ordered
}

// publishedRunConversation reads this run's own conversation files, which is
// where a withdrawal and an unanswered question live. Neither is persisted in
// the branch store: a withdrawal settles nothing and needs no answer, and an
// unanswered question has no answer to record.
func publishedRunConversation(sctx *pipeline.StepContext) reviewqa.Conversation {
	dir := reviewConversationDir(sctx)
	if dir == "" {
		return reviewqa.Conversation{}
	}
	conv, err := reviewqa.Load(dir)
	if err != nil {
		return reviewqa.Conversation{}
	}
	return conv
}

// publishedConversationText flattens and bounds one quoted line so a long
// question or answer cannot dominate the body, and so a newline cannot break
// out of the list item it belongs to.
// It counts RUNES, not bytes, and so does its disclosure: a question or answer
// is ordinary prose, so a byte cut falls inside a multi-byte rune on nothing
// more exotic than a typographic quote or an em dash and publishes invalid
// UTF-8. This is the same shape as internal/cli's truncate.
func publishedConversationText(s string) string {
	s = sanitizePromptText(s)
	runes := []rune(s)
	if len(runes) <= maxPublishedConversationChars {
		return s
	}
	return string(runes[:maxPublishedConversationChars]) + fmt.Sprintf("… (truncated, %d chars total)", len(runes))
}
