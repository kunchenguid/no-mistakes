package publication

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func pushResultChallenge(publicationID, headSHA string) EffectChallenge {
	challenge := EffectChallenge{
		PublicationID:  publicationID,
		Kind:           EffectPush,
		Attempt:        1,
		CommitSHA:      headSHA,
		RemoteIdentity: "github.com/example/project",
		DestinationRef: "refs/heads/codex/factory-publication-v1",
		HeadRef:        "refs/heads/codex/factory-publication-v1",
		EffectDigest:   strings.Repeat("e", 64),
	}
	digest, err := decisionDigest(challenge)
	if err != nil {
		panic(err)
	}
	challenge.DecisionDigest = digest
	denyDigest, err := decisionDigestFor(challenge, DecisionDeny)
	if err != nil {
		panic(err)
	}
	challenge.DenyDecisionDigest = denyDigest
	return challenge
}

func prResultChallenge(publicationID, headSHA string) EffectChallenge {
	marker := "<!-- no-mistakes-factory-publication-v1:" + publicationID + ":" + headSHA + " -->"
	draft := "Inspectable exact PR draft\n\n" + marker + "\n"
	challenge := EffectChallenge{
		PublicationID:  publicationID,
		Kind:           EffectPR,
		Attempt:        1,
		CommitSHA:      headSHA,
		RemoteIdentity: "github.com/example/project",
		DestinationRef: "refs/heads/codex/factory-publication-v1",
		BaseRef:        "refs/heads/main",
		HeadRef:        "refs/heads/codex/factory-publication-v1",
		Marker:         marker,
		PreparedDraft:  draft,
		DraftSHA256:    sha256Hex([]byte(draft)),
		EffectDigest:   strings.Repeat("e", 64),
	}
	digest, err := decisionDigest(challenge)
	if err != nil {
		panic(err)
	}
	challenge.DecisionDigest = digest
	denyDigest, err := decisionDigestFor(challenge, DecisionDeny)
	if err != nil {
		panic(err)
	}
	challenge.DenyDecisionDigest = denyDigest
	return challenge
}

func TestParseResultRequiresExactOwnerChallengeOnlyAtOwnerGates(t *testing.T) {
	publicationID := strings.Repeat("a", 64)
	headSHA := strings.Repeat("b", 40)
	runID := "factory-run-1"

	for name, result := range map[string]Result{
		"push": {
			Protocol: ProtocolV1, PublicationID: publicationID, RunID: runID, HeadSHA: headSHA,
			Status: StatusReadyForPush, Challenge: pointerChallenge(pushResultChallenge(publicationID, headSHA)),
		},
		"pr": {
			Protocol: ProtocolV1, PublicationID: publicationID, RunID: runID, HeadSHA: headSHA,
			Status: StatusReadyForPR, Challenge: pointerChallenge(prResultChallenge(publicationID, headSHA)),
		},
	} {
		t.Run(name, func(t *testing.T) {
			raw, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			parsed, err := ParseResult(raw)
			if err != nil {
				t.Fatalf("parse exact gate result: %v", err)
			}
			if !reflect.DeepEqual(parsed, result) {
				t.Fatalf("parsed result = %#v, want %#v", parsed, result)
			}
		})
	}

	for name, result := range map[string]Result{
		"push without challenge": {
			Protocol: ProtocolV1, PublicationID: publicationID, RunID: runID, HeadSHA: headSHA,
			Status: StatusReadyForPush,
		},
		"pr without challenge": {
			Protocol: ProtocolV1, PublicationID: publicationID, RunID: runID, HeadSHA: headSHA,
			Status: StatusReadyForPR,
		},
		"challenge outside owner gate": {
			Protocol: ProtocolV1, PublicationID: publicationID, RunID: runID, HeadSHA: headSHA,
			Status: StatusChecking, Challenge: pointerChallenge(pushResultChallenge(publicationID, headSHA)),
		},
	} {
		t.Run(name, func(t *testing.T) {
			raw, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := ParseResult(raw); err == nil {
				t.Fatalf("accepted invalid challenge/result combination: %s", raw)
			}
		})
	}
}

func TestParseResultRejectsChallengeBindingDrift(t *testing.T) {
	publicationID := strings.Repeat("a", 64)
	headSHA := strings.Repeat("b", 40)
	base := Result{
		Protocol: ProtocolV1, PublicationID: publicationID, RunID: "factory-run-1", HeadSHA: headSHA,
		Status: StatusReadyForPush, Challenge: pointerChallenge(pushResultChallenge(publicationID, headSHA)),
	}
	mutations := map[string]func(*Result){
		"publication":   func(result *Result) { result.Challenge.PublicationID = strings.Repeat("c", 64) },
		"kind":          func(result *Result) { result.Challenge.Kind = EffectPR },
		"attempt":       func(result *Result) { result.Challenge.Attempt = 2 },
		"head":          func(result *Result) { result.Challenge.CommitSHA = strings.Repeat("c", 40) },
		"remote":        func(result *Result) { result.Challenge.RemoteIdentity = "" },
		"destination":   func(result *Result) { result.Challenge.DestinationRef = "refs/heads/other" },
		"effect":        func(result *Result) { result.Challenge.EffectDigest = strings.Repeat("f", 64) },
		"decision":      func(result *Result) { result.Challenge.DecisionDigest = strings.Repeat("f", 64) },
		"deny decision": func(result *Result) { result.Challenge.DenyDecisionDigest = strings.Repeat("f", 64) },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			result := base
			challenge := *base.Challenge
			result.Challenge = &challenge
			mutate(&result)
			raw, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := ParseResult(raw); err == nil {
				t.Fatalf("accepted challenge drift: %s", raw)
			}
		})
	}
}

func TestParseResultRejectsOpenOrNoncanonicalNestedChallenge(t *testing.T) {
	publicationID := strings.Repeat("a", 64)
	headSHA := strings.Repeat("b", 40)
	result := Result{
		Protocol: ProtocolV1, PublicationID: publicationID, RunID: "factory-run-1", HeadSHA: headSHA,
		Status: StatusReadyForPush, Challenge: pointerChallenge(pushResultChallenge(publicationID, headSHA)),
	}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	mutants := map[string][]byte{
		"duplicate":    []byte(strings.Replace(string(raw), `"challenge":{"publication_id":`, `"challenge":{"publication_id":"`+publicationID+`","publication_id":`, 1)),
		"unknown":      []byte(strings.Replace(string(raw), `"challenge":{`, `"challenge":{"approved":true,`, 1)),
		"noncanonical": []byte(strings.Replace(string(raw), `"challenge":{`, `"challenge":{ `, 1)),
	}
	for name, mutant := range mutants {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseResult(mutant); err == nil {
				t.Fatalf("accepted open/noncanonical nested challenge: %s", mutant)
			}
		})
	}
}

func TestManagerStatusPublishesChallengeOnlyAfterExactEffectIsPrepared(t *testing.T) {
	fixture := newPublicationFixture(t, "public-result-challenge")
	startPublication(t, fixture)
	result := completeDefenseThroughLint(t, fixture)
	if result.Status != StatusReadyForPush {
		t.Fatalf("internal lint result status = %s, want %s", result.Status, StatusReadyForPush)
	}

	publicBefore, err := fixture.manager.Status(context.Background(), fixture.parsed.PublicationID)
	if err != nil {
		t.Fatal(err)
	}
	if publicBefore.Status != StatusChecking || publicBefore.Challenge != nil {
		t.Fatalf("unprepared public result = %#v, want CHECKING without challenge", publicBefore)
	}

	pushChallenge, err := fixture.manager.PreparePush(context.Background(), fixture.parsed.PublicationID)
	if err != nil {
		t.Fatal(err)
	}
	publicPush, err := fixture.manager.Status(context.Background(), fixture.parsed.PublicationID)
	if err != nil {
		t.Fatal(err)
	}
	if publicPush.Status != StatusReadyForPush || publicPush.Challenge == nil || *publicPush.Challenge != pushChallenge {
		t.Fatalf("prepared Push result = %#v, want exact challenge %#v", publicPush, pushChallenge)
	}

	authorizedPush, err := fixture.manager.Authorize(context.Background(), goAuthorization(pushChallenge))
	if err != nil {
		t.Fatal(err)
	}
	if authorizedPush.Status != StatusChecking || authorizedPush.Challenge != nil {
		t.Fatalf("authorized Push still exposed an Owner gate: %#v", authorizedPush)
	}
	if _, err := fixture.manager.ExecutePush(context.Background(), fixture.parsed.PublicationID); err != nil {
		t.Fatal(err)
	}
	publicBeforePR, err := fixture.manager.Status(context.Background(), fixture.parsed.PublicationID)
	if err != nil {
		t.Fatal(err)
	}
	if publicBeforePR.Status != StatusChecking || publicBeforePR.Challenge != nil {
		t.Fatalf("unprepared PR public result = %#v, want CHECKING without challenge", publicBeforePR)
	}

	prChallenge, err := fixture.manager.PreparePR(context.Background(), fixture.parsed.PublicationID, []byte("exact draft\n"))
	if err != nil {
		t.Fatal(err)
	}
	publicPR, err := fixture.manager.Status(context.Background(), fixture.parsed.PublicationID)
	if err != nil {
		t.Fatal(err)
	}
	if publicPR.Status != StatusReadyForPR || publicPR.Challenge == nil || *publicPR.Challenge != prChallenge {
		t.Fatalf("prepared PR result = %#v, want exact challenge %#v", publicPR, prChallenge)
	}
	authorizedPR, err := fixture.manager.Authorize(context.Background(), goAuthorization(prChallenge))
	if err != nil {
		t.Fatal(err)
	}
	if authorizedPR.Status != StatusChecking || authorizedPR.Challenge != nil {
		t.Fatalf("authorized PR still exposed an Owner gate: %#v", authorizedPR)
	}
}

func TestPublicChallengeProducesClosedGOAndDENYAuthorizationEnvelopes(t *testing.T) {
	publicationID := strings.Repeat("a", 64)
	headSHA := strings.Repeat("b", 40)
	for name, challenge := range map[string]EffectChallenge{
		"push": pushResultChallenge(publicationID, headSHA),
		"pr":   prResultChallenge(publicationID, headSHA),
	} {
		for _, decision := range []Decision{DecisionGo, DecisionDeny} {
			t.Run(name+"-"+string(decision), func(t *testing.T) {
				digest := challenge.DecisionDigest
				if decision == DecisionDeny {
					digest = challenge.DenyDecisionDigest
				}
				envelope := AuthorizationEnvelope{
					Protocol:       ProtocolV1,
					Decision:       decision,
					PublicationID:  challenge.PublicationID,
					Kind:           challenge.Kind,
					Attempt:        challenge.Attempt,
					CommitSHA:      challenge.CommitSHA,
					RemoteIdentity: challenge.RemoteIdentity,
					DestinationRef: challenge.DestinationRef,
					BaseRef:        challenge.BaseRef,
					HeadRef:        challenge.HeadRef,
					DraftSHA256:    challenge.DraftSHA256,
					EffectDigest:   challenge.EffectDigest,
					DecisionDigest: digest,
				}
				raw, err := json.Marshal(envelope)
				if err != nil {
					t.Fatal(err)
				}
				parsed, err := ParseAuthorization(raw)
				if err != nil {
					t.Fatalf("parse %s authorization derived from public challenge: %v", decision, err)
				}
				if !authorizationMatches(challenge, parsed.Authorization()) {
					t.Fatalf("%s authorization no longer matches exact public challenge", decision)
				}
			})
		}
	}
	if pushResultChallenge(publicationID, headSHA).DecisionDigest == pushResultChallenge(publicationID, headSHA).DenyDecisionDigest {
		t.Fatal("GO and DENY decision digests are transferable")
	}
}

func TestManagerDENYRequiresAndConsumesTheExactDecisionSpecificChallenge(t *testing.T) {
	fixture := newPublicationFixture(t, "exact-deny")
	challenge := preparePush(t, fixture)
	deny := goAuthorization(challenge)
	deny.Decision = DecisionDeny
	deny.DecisionDigest = challenge.DenyDecisionDigest

	for name, mutate := range map[string]func(*Authorization){
		"go digest":   func(value *Authorization) { value.DecisionDigest = challenge.DecisionDigest },
		"head":        func(value *Authorization) { value.CommitSHA = testCommitB },
		"destination": func(value *Authorization) { value.DestinationRef = "refs/heads/other" },
		"effect":      func(value *Authorization) { value.EffectDigest = hashText("foreign") },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := deny
			mutate(&candidate)
			if _, err := fixture.manager.Authorize(context.Background(), candidate); err == nil {
				t.Fatalf("accepted mismatched DENY %s", name)
			}
			status, err := fixture.manager.Status(context.Background(), fixture.parsed.PublicationID)
			if err != nil {
				t.Fatal(err)
			}
			if status.Status != StatusReadyForPush || status.Challenge == nil {
				t.Fatalf("mismatched DENY changed durable state: %#v", status)
			}
			if fixture.push.publishCalls != 0 {
				t.Fatalf("mismatched DENY reached Push port %d times", fixture.push.publishCalls)
			}
		})
	}

	result, err := fixture.manager.Authorize(context.Background(), deny)
	if err != nil {
		t.Fatalf("persist exact DENY: %v", err)
	}
	if result.Status != StatusDenied || result.Challenge != nil || result.ExitCode() == 0 {
		t.Fatalf("exact DENY result = %#v", result)
	}
	if fixture.push.publishCalls != 0 {
		t.Fatalf("exact DENY reached Push port %d times", fixture.push.publishCalls)
	}

	result, err = fixture.restartManager(t).Authorize(context.Background(), deny)
	if err != nil {
		t.Fatalf("reconcile exact DENY after restart: %v", err)
	}
	if result.Status != StatusDenied || fixture.push.publishCalls != 0 {
		t.Fatalf("DENY retry = %#v, push calls=%d", result, fixture.push.publishCalls)
	}

	goAfterDeny := goAuthorization(challenge)
	if _, err := fixture.restartManager(t).Authorize(context.Background(), goAfterDeny); err == nil {
		t.Fatal("DENY transferred to GO after restart")
	}
}

func TestChallengeReprovesRequestJournalAndPreparedDraftBeforePublication(t *testing.T) {
	fixture := newPublicationFixture(t, "challenge-reproof")
	preparePR(t, fixture)
	publicationRow, err := fixture.db.GetPublication(fixture.parsed.PublicationID)
	if err != nil || publicationRow == nil {
		t.Fatalf("load publication: row=%#v err=%v", publicationRow, err)
	}
	effect, err := fixture.db.GetPublicationEffect(fixture.parsed.PublicationID, db.PublicationEffectPR)
	if err != nil || effect == nil {
		t.Fatalf("load PR effect: effect=%#v err=%v", effect, err)
	}
	if err := validateChallengeEffectBinding(publicationRow, effect); err != nil {
		t.Fatalf("exact durable binding rejected: %v", err)
	}

	mutations := map[string]func(*db.Publication, *db.PublicationEffect){
		"request": func(publication *db.Publication, _ *db.PublicationEffect) {
			publication.CanonicalRequest = append([]byte(nil), publication.CanonicalRequest...)
			publication.CanonicalRequest[0] = '['
		},
		"candidate": func(_ *db.Publication, effect *db.PublicationEffect) { effect.Binding.CandidateSHA = testCommitB },
		"base": func(publication *db.Publication, _ *db.PublicationEffect) {
			publication.BaseSHA = testCommitB
		},
		"remote": func(_ *db.Publication, effect *db.PublicationEffect) {
			effect.Binding.RemoteIdentity = "github.com/attacker/project"
		},
		"effect digest": func(_ *db.Publication, effect *db.PublicationEffect) {
			effect.Binding.EffectDigest = hashText("foreign")
		},
		"draft": func(_ *db.Publication, effect *db.PublicationEffect) { effect.PreparedPayload = []byte("foreign") },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			publicationCopy := *publicationRow
			publicationCopy.CanonicalRequest = append([]byte(nil), publicationRow.CanonicalRequest...)
			effectCopy := *effect
			effectCopy.PreparedPayload = append([]byte(nil), effect.PreparedPayload...)
			mutate(&publicationCopy, &effectCopy)
			if err := validateChallengeEffectBinding(&publicationCopy, &effectCopy); err == nil {
				t.Fatalf("accepted corrupted %s binding", name)
			}
		})
	}
}

func TestTerminalPublicationCannotAuthorizeOrExecuteAnEffect(t *testing.T) {
	fixture := newPublicationFixture(t, "terminal-effect")
	challenge := preparePush(t, fixture)
	publicationRow, err := fixture.db.GetPublication(fixture.parsed.PublicationID)
	if err != nil || publicationRow == nil {
		t.Fatalf("load publication: row=%#v err=%v", publicationRow, err)
	}
	if err := fixture.db.UpdateRunErrorStatus(publicationRow.RunID, "unrelated terminal failure", types.RunFailed); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.manager.Authorize(context.Background(), goAuthorization(challenge)); err == nil {
		t.Fatal("terminal Run accepted GO")
	}
	if _, err := fixture.manager.ExecutePush(context.Background(), fixture.parsed.PublicationID); err == nil {
		t.Fatal("terminal Run executed Push")
	}
	if fixture.push.publishCalls != 0 {
		t.Fatalf("terminal Run reached Push port %d times", fixture.push.publishCalls)
	}
}

func pointerChallenge(challenge EffectChallenge) *EffectChallenge { return &challenge }
