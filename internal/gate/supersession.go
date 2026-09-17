package gate

import (
	"context"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
)

// SupersessionSource is the durable, run-recorded evidence source for
// superseded-submission reconciliation. A zero value (nil DB) supplies no
// evidence, which leaves every caller on the ordinary containment path.
//
// It is deliberately a run-record reader, not a caller assertion: the guard
// resolves the head a run actually submitted from the database, so no caller can
// claim ownership of a private head on its own behalf.
type SupersessionSource struct {
	DB     *db.DB
	RepoID string
	Branch string
}

// SupersessionContinuation is the recorded shape one condition away from
// recorded supersession: a terminal run that submitted exactly this private head
// and already returned custody, whose verified final head carries commits made
// after its reviewed head. That continuation is deliberately NOT evidence - the
// exception accepts only a verified head equal to the reviewed head - so it never
// authorizes a replacement. It exists so the refusal can name this exact
// condition and the operator step that answers it, instead of the generic at-risk
// wording.
//
// It is a diagnosis, never proof: the reviewed head it names must still survive
// in the head being submitted, so the condition reported is precisely the missing
// continuation and not a discarded accepted result.
type SupersessionContinuation struct {
	RunID        string
	ReviewedHead string
	VerifiedHead string
}

// evidenceFor reports recorded proof that privateHead is superseded by a result
// this repository's own pipeline accepted, and that the accepted result survives
// in liveHead. It is called only after liveHead has been staged into the gate, so
// the containment question is answerable there. A nil evidence result means no
// proof, and the caller's refusal stands; a non-nil continuation is diagnosis for
// that refusal, never proof.
//
// The evidence is exactly one terminal run on this branch that
//
//   - submitted privateHead (SubmittedHeadSHA equals it, never an abbreviated or
//     otherwise-recorded head),
//   - already ended its ownership of the branch (CustodyReturnedAt), so no
//     in-flight work is being discarded,
//   - recorded a verified final head (TerminalHeadVerifiedAt) that is its own
//     reviewed head (HeadSHA equals ReviewApprovedHeadSHA), which is what makes
//     the replacement a reviewed decision rather than an inference, and
//   - whose reviewed head is contained in liveHead, so the accepted result
//     survives the replacement.
//
// Any missing or mismatched field yields no evidence, and the caller's refusal
// stands. An active run never produces evidence: its ownership is not this
// path's to settle.
func (s SupersessionSource) evidenceFor(ctx context.Context, gateDir, privateHead, liveHead string) (*SupersessionEvidence, *SupersessionContinuation) {
	if s.DB == nil || strings.TrimSpace(s.RepoID) == "" || strings.TrimSpace(s.Branch) == "" {
		return nil, nil
	}
	privateHead = strings.TrimSpace(privateHead)
	liveHead = strings.TrimSpace(liveHead)
	if privateHead == "" || liveHead == "" {
		return nil, nil
	}
	if objectType, err := git.Run(ctx, gateDir, "cat-file", "-t", privateHead); err != nil || objectType != "commit" {
		return nil, nil
	}
	runs, err := s.DB.GetRunsByRepo(s.RepoID)
	if err != nil {
		return nil, nil
	}
	var continuation *SupersessionContinuation
	for _, run := range runs {
		if run.Branch != s.Branch || !run.Status.Terminal() || run.CustodyReturnedAt == nil {
			continue
		}
		if run.SubmittedHeadSHA == nil || strings.TrimSpace(*run.SubmittedHeadSHA) != privateHead {
			continue
		}
		if run.TerminalHeadVerifiedAt == nil || run.ReviewApprovedHeadSHA == nil {
			continue
		}
		verified := strings.TrimSpace(run.HeadSHA)
		reviewed := strings.TrimSpace(*run.ReviewApprovedHeadSHA)
		if verified == "" || reviewed == "" {
			continue
		}
		if verified != reviewed {
			if continuation == nil && acceptedSurvivesIn(ctx, gateDir, reviewed, liveHead) {
				continuation = &SupersessionContinuation{
					RunID: run.ID, ReviewedHead: reviewed, VerifiedHead: verified,
				}
			}
			continue
		}
		if !acceptedSurvivesIn(ctx, gateDir, verified, liveHead) {
			continue
		}
		return &SupersessionEvidence{RunID: run.ID, SubmittedHead: privateHead, AcceptedHead: verified}, nil
	}
	return nil, continuation
}

// acceptedSurvivesIn reports whether the run's accepted head is still contained
// in the head about to be submitted. Containment rather than equality: a live
// head carrying later commits on top of the accepted result still preserves it.
func acceptedSurvivesIn(ctx context.Context, gateDir, accepted, liveHead string) bool {
	if accepted == liveHead {
		return true
	}
	_, err := git.Run(ctx, gateDir, "merge-base", "--is-ancestor", accepted, liveHead)
	return err == nil
}
