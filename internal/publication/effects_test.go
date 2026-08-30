package publication

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

var (
	errSimulatedPortError        = errors.New("simulated port error after effect attempt")
	errSimulatedObservationError = errors.New("simulated observation transport error")
)

type fakePushPort struct {
	publishCalls     int
	observeCalls     int
	remoteHead       string
	errorAfterCall   bool
	applyBeforeError bool
	observeErr       error
	requests         []PushEffectRequest
}

func (f *fakePushPort) PublishExact(_ context.Context, request PushEffectRequest) error {
	f.publishCalls++
	f.requests = append(f.requests, request)
	if !f.errorAfterCall || f.applyBeforeError {
		f.remoteHead = request.CommitSHA
	}
	if f.errorAfterCall {
		return errSimulatedPortError
	}
	return nil
}

func (f *fakePushPort) ObserveExact(_ context.Context, _ PushEffectRequest) (PushObservation, error) {
	f.observeCalls++
	if f.observeErr != nil {
		return PushObservation{}, f.observeErr
	}
	return PushObservation{RemoteHeadSHA: f.remoteHead}, nil
}

type fakePRPort struct {
	createCalls       int
	findCalls         int
	errorAfterCall    bool
	createdMatchCount int
	lastCreate        PREffectRequest
	matches           []PRObservation
	findErr           error
}

func (f *fakePRPort) CreateExact(_ context.Context, request PREffectRequest) error {
	f.createCalls++
	f.lastCreate = request
	count := f.createdMatchCount
	if count == 0 && !f.errorAfterCall {
		count = 1
	}
	for i := 0; i < count; i++ {
		f.matches = append(f.matches, PRObservation{
			RepositoryID: request.RepositoryID,
			BaseRef:      request.BaseRef,
			HeadRef:      request.HeadRef,
			HeadSHA:      request.CommitSHA,
			Marker:       request.Marker,
			DraftSHA256:  request.DraftSHA256,
			Number:       fmt.Sprintf("%d", i+1),
		})
	}
	if f.errorAfterCall {
		return errSimulatedPortError
	}
	return nil
}

func (f *fakePRPort) FindExact(_ context.Context, _ PRReconcileQuery) ([]PRObservation, error) {
	f.findCalls++
	if f.findErr != nil {
		return nil, f.findErr
	}
	return append([]PRObservation(nil), f.matches...), nil
}

type fakeCIPort struct {
	observeCalls int
	queries      []CIQuery
	observation  CIObservation
	observations []CIObservation
}

func (f *fakeCIPort) ObserveExact(_ context.Context, query CIQuery) (CIObservation, error) {
	index := f.observeCalls
	f.observeCalls++
	f.queries = append(f.queries, query)
	if index < len(f.observations) {
		return f.observations[index], nil
	}
	return f.observation, nil
}

func goAuthorization(challenge EffectChallenge) Authorization {
	return Authorization{
		Decision:       DecisionGo,
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
		DecisionDigest: challenge.DecisionDigest,
	}
}

func preparePush(t *testing.T, fixture *publicationFixture) EffectChallenge {
	t.Helper()
	startPublication(t, fixture)
	result := completeDefenseThroughLint(t, fixture)
	if result.Status != StatusReadyForPush {
		t.Fatalf("status before push gate = %q, want %q", result.Status, StatusReadyForPush)
	}
	challenge, err := fixture.manager.PreparePush(context.Background(), fixture.parsed.PublicationID)
	if err != nil {
		t.Fatalf("prepare push: %v", err)
	}
	return challenge
}

func passPush(t *testing.T, fixture *publicationFixture) EffectChallenge {
	t.Helper()
	challenge := preparePush(t, fixture)
	if _, err := fixture.manager.Authorize(context.Background(), goAuthorization(challenge)); err != nil {
		t.Fatalf("authorize push: %v", err)
	}
	result, err := fixture.manager.ExecutePush(context.Background(), fixture.parsed.PublicationID)
	if err != nil {
		t.Fatalf("execute push: %v", err)
	}
	if result.Status != StatusReadyForPR {
		t.Fatalf("status after push = %q, want %q", result.Status, StatusReadyForPR)
	}
	return challenge
}

func preparePR(t *testing.T, fixture *publicationFixture) EffectChallenge {
	t.Helper()
	passPush(t, fixture)
	challenge, err := fixture.manager.PreparePR(
		context.Background(),
		fixture.parsed.PublicationID,
		[]byte("Factory publication candidate\n\nEvidence is bound below.\n"),
	)
	if err != nil {
		t.Fatalf("prepare PR: %v", err)
	}
	return challenge
}

func passPR(t *testing.T, fixture *publicationFixture) EffectChallenge {
	t.Helper()
	challenge := preparePR(t, fixture)
	if _, err := fixture.manager.Authorize(context.Background(), goAuthorization(challenge)); err != nil {
		t.Fatalf("authorize PR: %v", err)
	}
	result, err := fixture.manager.ExecutePR(context.Background(), fixture.parsed.PublicationID)
	if err != nil {
		t.Fatalf("execute PR: %v", err)
	}
	if result.Status != StatusCIObserving {
		t.Fatalf("status after PR = %q, want %q", result.Status, StatusCIObserving)
	}
	return challenge
}

// simulatePushProcessLoss starts the durable effect and invokes the port
// directly, then deliberately omits Manager reconciliation. This models a
// process disappearing after the external call more accurately than returning
// a normal port error to a still-running Manager.
func simulatePushProcessLoss(t *testing.T, fixture *publicationFixture) {
	t.Helper()
	publication, _, err := fixture.manager.loadPublicationRun(fixture.parsed.PublicationID)
	if err != nil {
		t.Fatalf("load publication before simulated Push process loss: %v", err)
	}
	effect, err := fixture.db.GetPublicationEffect(fixture.parsed.PublicationID, db.PublicationEffectPush)
	if err != nil || effect == nil || effect.DecisionDigest == nil {
		t.Fatalf("load authorized Push effect: %#v, %v", effect, err)
	}
	started, err := fixture.db.BeginPublicationEffect(db.BeginPublicationEffectInput{
		PublicationID:  fixture.parsed.PublicationID,
		Kind:           db.PublicationEffectPush,
		Binding:        effect.Binding,
		DecisionDigest: *effect.DecisionDigest,
	})
	if err != nil {
		t.Fatalf("begin Push before simulated process loss: %v", err)
	}
	if err := fixture.push.PublishExact(context.Background(), pushRequest(publication, started)); !errorsIs(err, errSimulatedPortError) {
		t.Fatalf("direct Push port error = %v, want simulated process loss", err)
	}
}

func simulatePRProcessLoss(t *testing.T, fixture *publicationFixture) {
	t.Helper()
	publication, _, err := fixture.manager.loadPublicationRun(fixture.parsed.PublicationID)
	if err != nil {
		t.Fatalf("load publication before simulated PR process loss: %v", err)
	}
	effect, err := fixture.db.GetPublicationEffect(fixture.parsed.PublicationID, db.PublicationEffectPR)
	if err != nil || effect == nil || effect.DecisionDigest == nil {
		t.Fatalf("load authorized PR effect: %#v, %v", effect, err)
	}
	started, err := fixture.db.BeginPublicationEffect(db.BeginPublicationEffectInput{
		PublicationID:  fixture.parsed.PublicationID,
		Kind:           db.PublicationEffectPR,
		Binding:        effect.Binding,
		DecisionDigest: *effect.DecisionDigest,
	})
	if err != nil {
		t.Fatalf("begin PR before simulated process loss: %v", err)
	}
	draft := append([]byte(nil), started.PreparedPayload...)
	if err := fixture.pr.CreateExact(context.Background(), prRequest(publication, started, draft)); !errorsIs(err, errSimulatedPortError) {
		t.Fatalf("direct PR port error = %v, want simulated process loss", err)
	}
}

func TestPushPortCallCountStaysZeroUntilExactDurableSingleUseGo(t *testing.T) {
	fixture := newPublicationFixture(t, "push-gate")
	challenge := preparePush(t, fixture)

	if _, err := fixture.manager.ExecutePush(context.Background(), fixture.parsed.PublicationID); err == nil {
		t.Fatal("push without Owner GO succeeded")
	}
	if fixture.push.publishCalls != 0 {
		t.Fatalf("push port called %d times before Owner GO", fixture.push.publishCalls)
	}

	if _, err := fixture.manager.Authorize(context.Background(), goAuthorization(challenge)); err != nil {
		t.Fatalf("persist exact Owner GO: %v", err)
	}
	managerAfterRestart := fixture.restartManager(t)
	result, err := managerAfterRestart.ExecutePush(context.Background(), fixture.parsed.PublicationID)
	if err != nil {
		t.Fatalf("execute durably authorized push after restart: %v", err)
	}
	if result.Status != StatusReadyForPR {
		t.Fatalf("status = %q, want %q", result.Status, StatusReadyForPR)
	}
	if fixture.push.publishCalls != 1 {
		t.Fatalf("push calls = %d, want exactly one", fixture.push.publishCalls)
	}

	if _, err := managerAfterRestart.ExecutePush(context.Background(), fixture.parsed.PublicationID); err != nil {
		t.Fatalf("reconcile already observed push: %v", err)
	}
	if fixture.push.publishCalls != 1 {
		t.Fatalf("single-use GO replayed push: %d calls", fixture.push.publishCalls)
	}
	request := fixture.push.requests[0]
	if request.CommitSHA != testCommitA || request.DestinationRef != challenge.DestinationRef || request.RemoteIdentity != challenge.RemoteIdentity {
		t.Fatalf("push request lost exact binding: %+v", request)
	}
}

func TestEffectAuthorizationDigestCannotTransfer(t *testing.T) {
	tests := map[string]func(*Authorization){
		"publication":     func(a *Authorization) { a.PublicationID = "another-publication" },
		"kind":            func(a *Authorization) { a.Kind = EffectPR },
		"attempt":         func(a *Authorization) { a.Attempt++ },
		"candidate":       func(a *Authorization) { a.CommitSHA = testCommitB },
		"remote":          func(a *Authorization) { a.RemoteIdentity = "github.com/attacker/project" },
		"destination":     func(a *Authorization) { a.DestinationRef = "refs/heads/other" },
		"effect_digest":   func(a *Authorization) { a.EffectDigest = hashText("different effect") },
		"decision_digest": func(a *Authorization) { a.DecisionDigest = hashText("different decision") },
	}

	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			fixture := newPublicationFixture(t, "binding-"+name)
			challenge := preparePush(t, fixture)
			authorization := goAuthorization(challenge)
			mutate(&authorization)
			if _, err := fixture.manager.Authorize(context.Background(), authorization); err == nil {
				t.Fatalf("transferred %s authorization was accepted", name)
			}
			if _, err := fixture.manager.ExecutePush(context.Background(), fixture.parsed.PublicationID); err == nil {
				t.Fatal("rejected authorization still enabled push")
			}
			if fixture.push.publishCalls != 0 {
				t.Fatalf("push called %d times for transferred authorization", fixture.push.publishCalls)
			}
		})
	}
}

func TestPushCrashRecoveryReconcilesAndNeverBlindlyReplays(t *testing.T) {
	t.Run("crash before effect uses the durable unused GO once", func(t *testing.T) {
		fixture := newPublicationFixture(t, "push-crash-before")
		challenge := preparePush(t, fixture)
		if _, err := fixture.manager.Authorize(context.Background(), goAuthorization(challenge)); err != nil {
			t.Fatalf("authorize push: %v", err)
		}

		managerAfterCrash := fixture.restartManager(t)
		if _, err := managerAfterCrash.ExecutePush(context.Background(), fixture.parsed.PublicationID); err != nil {
			t.Fatalf("execute unused durable GO after restart: %v", err)
		}
		if fixture.push.publishCalls != 1 {
			t.Fatalf("push calls = %d, want one", fixture.push.publishCalls)
		}
	})

	t.Run("effect applied before crash is observed without replay", func(t *testing.T) {
		fixture := newPublicationFixture(t, "push-crash-after")
		fixture.push.errorAfterCall = true
		fixture.push.applyBeforeError = true
		challenge := preparePush(t, fixture)
		if _, err := fixture.manager.Authorize(context.Background(), goAuthorization(challenge)); err != nil {
			t.Fatalf("authorize push: %v", err)
		}
		simulatePushProcessLoss(t, fixture)

		fixture.push.errorAfterCall = false
		result, err := fixture.restartManager(t).RecoverEffect(context.Background(), fixture.parsed.PublicationID, EffectPush)
		if err != nil {
			t.Fatalf("recover applied push: %v", err)
		}
		if result.Status != StatusReadyForPR {
			t.Fatalf("status = %q, want %q", result.Status, StatusReadyForPR)
		}
		if fixture.push.publishCalls != 1 {
			t.Fatalf("recovery replayed applied push: %d calls", fixture.push.publishCalls)
		}
	})

	t.Run("unobserved possible effect becomes EFFECT_UNKNOWN", func(t *testing.T) {
		fixture := newPublicationFixture(t, "push-crash-unknown")
		fixture.push.errorAfterCall = true
		fixture.push.applyBeforeError = false
		challenge := preparePush(t, fixture)
		if _, err := fixture.manager.Authorize(context.Background(), goAuthorization(challenge)); err != nil {
			t.Fatalf("authorize push: %v", err)
		}
		simulatePushProcessLoss(t, fixture)

		fixture.push.errorAfterCall = false
		result, err := fixture.restartManager(t).RecoverEffect(context.Background(), fixture.parsed.PublicationID, EffectPush)
		if err != nil {
			t.Fatalf("recover ambiguous push: %v", err)
		}
		if result.Status != StatusEffectUnknown {
			t.Fatalf("status = %q, want %q", result.Status, StatusEffectUnknown)
		}
		if fixture.push.publishCalls != 1 {
			t.Fatalf("ambiguous recovery blindly replayed push: %d calls", fixture.push.publishCalls)
		}
	})
}

func TestPushPortErrorReconcilesBeforeReturningAndNeverReplays(t *testing.T) {
	for _, test := range []struct {
		name              string
		applyBeforeError  bool
		wantStatus        ResultStatus
		wantRemoteHeadSHA string
	}{
		{
			name:              "exact effect happened",
			applyBeforeError:  true,
			wantStatus:        StatusReadyForPR,
			wantRemoteHeadSHA: testCommitA,
		},
		{
			name:             "effect did not happen",
			applyBeforeError: false,
			wantStatus:       StatusEffectUnknown,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newPublicationFixture(t, "push-port-error-"+test.name)
			fixture.push.errorAfterCall = true
			fixture.push.applyBeforeError = test.applyBeforeError
			challenge := preparePush(t, fixture)
			if _, err := fixture.manager.Authorize(context.Background(), goAuthorization(challenge)); err != nil {
				t.Fatalf("authorize push: %v", err)
			}

			result, err := fixture.manager.ExecutePush(context.Background(), fixture.parsed.PublicationID)
			if err != nil {
				t.Fatalf("execute Push must reconcile the uncertain port error before returning: %v", err)
			}
			if result.Status != test.wantStatus {
				t.Fatalf("status = %q, want %q", result.Status, test.wantStatus)
			}
			if fixture.push.publishCalls != 1 || fixture.push.observeCalls != 1 {
				t.Fatalf("Push calls publish=%d observe=%d, want exactly 1/1", fixture.push.publishCalls, fixture.push.observeCalls)
			}
			if fixture.push.remoteHead != test.wantRemoteHeadSHA {
				t.Fatalf("remote head = %q, want %q", fixture.push.remoteHead, test.wantRemoteHeadSHA)
			}
			run, err := fixture.db.GetRun(result.RunID)
			if err != nil {
				t.Fatalf("read publication Run: %v", err)
			}
			if run == nil || run.Status.Terminal() {
				t.Fatalf("port error prematurely terminalized Run: %#v", run)
			}

			reconciled, err := fixture.restartManager(t).RecoverEffect(context.Background(), fixture.parsed.PublicationID, EffectPush)
			if err != nil {
				t.Fatalf("repeat durable reconciliation: %v", err)
			}
			if reconciled.Status != test.wantStatus {
				t.Fatalf("reconciled status = %q, want %q", reconciled.Status, test.wantStatus)
			}
			if fixture.push.publishCalls != 1 || fixture.push.observeCalls != 1 {
				t.Fatalf("durable result replayed/re-observed Push: publish=%d observe=%d", fixture.push.publishCalls, fixture.push.observeCalls)
			}
		})
	}
}

func TestPushObservationErrorAfterPortErrorDurablyBecomesEffectUnknown(t *testing.T) {
	fixture := newPublicationFixture(t, "push-observation-error")
	fixture.push.errorAfterCall = true
	fixture.push.applyBeforeError = true
	fixture.push.observeErr = errSimulatedObservationError
	challenge := preparePush(t, fixture)
	if _, err := fixture.manager.Authorize(context.Background(), goAuthorization(challenge)); err != nil {
		t.Fatalf("authorize Push: %v", err)
	}

	result, err := fixture.manager.ExecutePush(context.Background(), fixture.parsed.PublicationID)
	if err != nil {
		t.Fatalf("observation transport error must become durable EFFECT_UNKNOWN: %v", err)
	}
	if result.Status != StatusEffectUnknown {
		t.Fatalf("status = %q, want %q", result.Status, StatusEffectUnknown)
	}
	if fixture.push.publishCalls != 1 || fixture.push.observeCalls != 1 {
		t.Fatalf("Push calls publish=%d observe=%d, want exactly 1/1", fixture.push.publishCalls, fixture.push.observeCalls)
	}
	effect, err := fixture.db.GetPublicationEffect(fixture.parsed.PublicationID, db.PublicationEffectPush)
	if err != nil || effect == nil || effect.State != db.PublicationEffectUnknown {
		t.Fatalf("durable Push effect = %#v, %v; want unknown", effect, err)
	}
	run, err := fixture.db.GetRun(result.RunID)
	if err != nil {
		t.Fatalf("read publication Run: %v", err)
	}
	if run == nil || run.Status.Terminal() {
		t.Fatalf("observation error prematurely terminalized Run: %#v", run)
	}

	reconciled, err := fixture.restartManager(t).RecoverEffect(context.Background(), fixture.parsed.PublicationID, EffectPush)
	if err != nil {
		t.Fatalf("recover durable unknown Push: %v", err)
	}
	if reconciled.Status != StatusEffectUnknown {
		t.Fatalf("reconciled status = %q, want %q", reconciled.Status, StatusEffectUnknown)
	}
	if fixture.push.publishCalls != 1 || fixture.push.observeCalls != 1 {
		t.Fatalf("restart retried uncertain Push: publish=%d observe=%d", fixture.push.publishCalls, fixture.push.observeCalls)
	}
}

func TestPRDraftHasOneExactMarkerAndRawByteDigest(t *testing.T) {
	fixture := newPublicationFixture(t, "pr-draft")
	challenge := preparePR(t, fixture)
	if _, err := fixture.manager.Authorize(context.Background(), goAuthorization(challenge)); err != nil {
		t.Fatalf("authorize PR: %v", err)
	}
	if _, err := fixture.manager.ExecutePR(context.Background(), fixture.parsed.PublicationID); err != nil {
		t.Fatalf("execute PR: %v", err)
	}

	draft := fixture.pr.lastCreate.Draft
	if challenge.Marker == "" {
		t.Fatal("PR challenge has no reconciliation marker")
	}
	if count := bytes.Count(draft, []byte(challenge.Marker)); count != 1 {
		t.Fatalf("live reconciliation marker count = %d, want exactly one", count)
	}
	sum := sha256.Sum256(draft)
	if got := hex.EncodeToString(sum[:]); got != challenge.DraftSHA256 {
		t.Fatalf("draft digest = %q, want raw-byte SHA-256 %q", challenge.DraftSHA256, got)
	}
	if fixture.pr.lastCreate.CommitSHA != testCommitA {
		t.Fatalf("PR head = %q, want exact candidate %q", fixture.pr.lastCreate.CommitSHA, testCommitA)
	}
}

func TestPRCrashReconciliationRequiresExactlyOneExactMatch(t *testing.T) {
	for _, matchCount := range []int{0, 1, 2} {
		t.Run(fmt.Sprintf("matches_%d", matchCount), func(t *testing.T) {
			fixture := newPublicationFixture(t, fmt.Sprintf("pr-reconcile-%d", matchCount))
			challenge := preparePR(t, fixture)
			fixture.pr.errorAfterCall = true
			fixture.pr.createdMatchCount = matchCount
			if _, err := fixture.manager.Authorize(context.Background(), goAuthorization(challenge)); err != nil {
				t.Fatalf("authorize PR: %v", err)
			}
			simulatePRProcessLoss(t, fixture)

			fixture.pr.errorAfterCall = false
			result, err := fixture.restartManager(t).RecoverEffect(context.Background(), fixture.parsed.PublicationID, EffectPR)
			if err != nil {
				t.Fatalf("recover PR: %v", err)
			}
			want := StatusEffectUnknown
			if matchCount == 1 {
				want = StatusCIObserving
			}
			if result.Status != want {
				t.Fatalf("status = %q, want %q", result.Status, want)
			}
			if fixture.pr.createCalls != 1 {
				t.Fatalf("PR recovery blindly created again: %d calls", fixture.pr.createCalls)
			}
		})
	}
}

func TestPRPortErrorReconcilesBeforeReturningAndNeverReplays(t *testing.T) {
	for _, matchCount := range []int{0, 1, 2} {
		t.Run(fmt.Sprintf("matches_%d", matchCount), func(t *testing.T) {
			fixture := newPublicationFixture(t, fmt.Sprintf("pr-port-error-%d", matchCount))
			challenge := preparePR(t, fixture)
			fixture.pr.errorAfterCall = true
			fixture.pr.createdMatchCount = matchCount
			if _, err := fixture.manager.Authorize(context.Background(), goAuthorization(challenge)); err != nil {
				t.Fatalf("authorize PR: %v", err)
			}

			result, err := fixture.manager.ExecutePR(context.Background(), fixture.parsed.PublicationID)
			if err != nil {
				t.Fatalf("execute PR must reconcile the uncertain port error before returning: %v", err)
			}
			want := StatusEffectUnknown
			if matchCount == 1 {
				want = StatusCIObserving
			}
			if result.Status != want {
				t.Fatalf("status = %q, want %q", result.Status, want)
			}
			if fixture.pr.createCalls != 1 || fixture.pr.findCalls != 1 {
				t.Fatalf("PR calls create=%d find=%d, want exactly 1/1", fixture.pr.createCalls, fixture.pr.findCalls)
			}
			run, err := fixture.db.GetRun(result.RunID)
			if err != nil {
				t.Fatalf("read publication Run: %v", err)
			}
			if run == nil || run.Status.Terminal() {
				t.Fatalf("port error prematurely terminalized Run: %#v", run)
			}

			reconciled, err := fixture.restartManager(t).RecoverEffect(context.Background(), fixture.parsed.PublicationID, EffectPR)
			if err != nil {
				t.Fatalf("repeat durable reconciliation: %v", err)
			}
			if reconciled.Status != want {
				t.Fatalf("reconciled status = %q, want %q", reconciled.Status, want)
			}
			if fixture.pr.createCalls != 1 || fixture.pr.findCalls != 1 {
				t.Fatalf("durable result replayed/re-observed PR: create=%d find=%d", fixture.pr.createCalls, fixture.pr.findCalls)
			}
		})
	}
}

func TestPRObservationErrorAfterPortErrorDurablyBecomesEffectUnknown(t *testing.T) {
	fixture := newPublicationFixture(t, "pr-observation-error")
	challenge := preparePR(t, fixture)
	fixture.pr.errorAfterCall = true
	fixture.pr.createdMatchCount = 1
	fixture.pr.findErr = errSimulatedObservationError
	if _, err := fixture.manager.Authorize(context.Background(), goAuthorization(challenge)); err != nil {
		t.Fatalf("authorize PR: %v", err)
	}

	result, err := fixture.manager.ExecutePR(context.Background(), fixture.parsed.PublicationID)
	if err != nil {
		t.Fatalf("observation transport error must become durable EFFECT_UNKNOWN: %v", err)
	}
	if result.Status != StatusEffectUnknown {
		t.Fatalf("status = %q, want %q", result.Status, StatusEffectUnknown)
	}
	if fixture.pr.createCalls != 1 || fixture.pr.findCalls != 1 {
		t.Fatalf("PR calls create=%d find=%d, want exactly 1/1", fixture.pr.createCalls, fixture.pr.findCalls)
	}
	effect, err := fixture.db.GetPublicationEffect(fixture.parsed.PublicationID, db.PublicationEffectPR)
	if err != nil || effect == nil || effect.State != db.PublicationEffectUnknown {
		t.Fatalf("durable PR effect = %#v, %v; want unknown", effect, err)
	}
	run, err := fixture.db.GetRun(result.RunID)
	if err != nil {
		t.Fatalf("read publication Run: %v", err)
	}
	if run == nil || run.Status.Terminal() {
		t.Fatalf("observation error prematurely terminalized Run: %#v", run)
	}

	reconciled, err := fixture.restartManager(t).RecoverEffect(context.Background(), fixture.parsed.PublicationID, EffectPR)
	if err != nil {
		t.Fatalf("recover durable unknown PR: %v", err)
	}
	if reconciled.Status != StatusEffectUnknown {
		t.Fatalf("reconciled status = %q, want %q", reconciled.Status, StatusEffectUnknown)
	}
	if fixture.pr.createCalls != 1 || fixture.pr.findCalls != 1 {
		t.Fatalf("restart retried uncertain PR: create=%d find=%d", fixture.pr.createCalls, fixture.pr.findCalls)
	}
}

func TestPRRecoveryRejectsEveryExactBindingMismatch(t *testing.T) {
	mutations := map[string]func(*PRObservation){
		"repository": func(o *PRObservation) { o.RepositoryID = "other-repo" },
		"base":       func(o *PRObservation) { o.BaseRef = "refs/heads/release" },
		"head_ref":   func(o *PRObservation) { o.HeadRef = "refs/heads/other" },
		"head_sha":   func(o *PRObservation) { o.HeadSHA = testCommitB },
		"marker":     func(o *PRObservation) { o.Marker = "foreign-marker" },
		"draft":      func(o *PRObservation) { o.DraftSHA256 = hashText("foreign draft") },
	}

	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			fixture := newPublicationFixture(t, "pr-mismatch-"+name)
			challenge := preparePR(t, fixture)
			fixture.pr.errorAfterCall = true
			fixture.pr.createdMatchCount = 1
			if _, err := fixture.manager.Authorize(context.Background(), goAuthorization(challenge)); err != nil {
				t.Fatalf("authorize PR: %v", err)
			}
			simulatePRProcessLoss(t, fixture)
			mutate(&fixture.pr.matches[0])

			result, err := fixture.restartManager(t).RecoverEffect(context.Background(), fixture.parsed.PublicationID, EffectPR)
			if err != nil {
				t.Fatalf("recover mismatched PR: %v", err)
			}
			if result.Status != StatusEffectUnknown {
				t.Fatalf("status = %q, want %q", result.Status, StatusEffectUnknown)
			}
			if fixture.pr.createCalls != 1 {
				t.Fatalf("mismatched recovery created another PR: %d calls", fixture.pr.createCalls)
			}
		})
	}
}

func TestCIIsReadOnlyExactHeadAndNonEmptyAllPassOnly(t *testing.T) {
	tests := map[string]struct {
		observation CIObservation
		wantReady   bool
	}{
		"one pass": {
			observation: CIObservation{HeadSHA: testCommitA, Checks: []CICheck{{Name: "test", HeadSHA: testCommitA, Status: CICheckPass}}},
			wantReady:   true,
		},
		"multiple pass": {
			observation: CIObservation{HeadSHA: testCommitA, Checks: []CICheck{{Name: "test", HeadSHA: testCommitA, Status: CICheckPass}, {Name: "lint", HeadSHA: testCommitA, Status: CICheckPass}}},
			wantReady:   true,
		},
		"empty":            {observation: CIObservation{HeadSHA: testCommitA}},
		"head drift":       {observation: CIObservation{HeadSHA: testCommitB, Checks: []CICheck{{Name: "test", HeadSHA: testCommitB, Status: CICheckPass}}}},
		"check head drift": {observation: CIObservation{HeadSHA: testCommitA, Checks: []CICheck{{Name: "test", HeadSHA: testCommitB, Status: CICheckPass}}}},
		"failed":           {observation: CIObservation{HeadSHA: testCommitA, Checks: []CICheck{{Name: "test", HeadSHA: testCommitA, Status: CICheckFail}}}},
		"pending":          {observation: CIObservation{HeadSHA: testCommitA, Checks: []CICheck{{Name: "test", HeadSHA: testCommitA, Status: CICheckPending}}}},
		"cancelled":        {observation: CIObservation{HeadSHA: testCommitA, Checks: []CICheck{{Name: "test", HeadSHA: testCommitA, Status: CICheckCancelled}}}},
		"skipped":          {observation: CIObservation{HeadSHA: testCommitA, Checks: []CICheck{{Name: "test", HeadSHA: testCommitA, Status: CICheckSkipped}}}},
		"partial":          {observation: CIObservation{HeadSHA: testCommitA, Checks: []CICheck{{Name: "test", HeadSHA: testCommitA, Status: CICheckPartial}}}},
		"unknown":          {observation: CIObservation{HeadSHA: testCommitA, Checks: []CICheck{{Name: "test", HeadSHA: testCommitA, Status: CICheckUnknown}}}},
		"malformed":        {observation: CIObservation{HeadSHA: testCommitA, Checks: []CICheck{{Name: "test", HeadSHA: testCommitA, Status: CICheckStatus("not-a-status")}}}},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			fixture := newPublicationFixture(t, "ci-"+name)
			passPR(t, fixture)
			fixture.ci.observation = test.observation

			result, err := fixture.manager.ObserveCI(context.Background(), fixture.parsed.PublicationID)
			if err != nil {
				t.Fatalf("observe CI: %v", err)
			}
			if test.wantReady {
				if result.Status != StatusReady || result.ExitCode() != 0 {
					t.Fatalf("green exact CI result = %+v, exit %d", result, result.ExitCode())
				}
			} else if result.Status == StatusReady || result.ExitCode() == 0 {
				t.Fatalf("non-green CI became READY: %+v, exit %d", result, result.ExitCode())
			}
			if fixture.ci.observeCalls != 1 {
				t.Fatalf("CI observation calls = %d, want one read-only observation", fixture.ci.observeCalls)
			}
			wantQuery := CIQuery{PublicationID: fixture.parsed.PublicationID, CommitSHA: testCommitA}
			if !reflect.DeepEqual(fixture.ci.queries[0], wantQuery) {
				t.Fatalf("CI query = %+v, want exact-H query %+v", fixture.ci.queries[0], wantQuery)
			}
		})
	}
}

func TestObserveCILeavesRunLifecycleToExecutor(t *testing.T) {
	tests := map[string]struct {
		observation CIObservation
		wantStatus  ResultStatus
	}{
		"exact pass": {
			observation: CIObservation{HeadSHA: testCommitA, Checks: []CICheck{{Name: "test", HeadSHA: testCommitA, Status: CICheckPass}}},
			wantStatus:  StatusReady,
		},
		"failed check": {
			observation: CIObservation{HeadSHA: testCommitA, Checks: []CICheck{{Name: "test", HeadSHA: testCommitA, Status: CICheckFail}}},
			wantStatus:  StatusFailed,
		},
		"head drift": {
			observation: CIObservation{HeadSHA: testCommitB, Checks: []CICheck{{Name: "test", HeadSHA: testCommitB, Status: CICheckPass}}},
			wantStatus:  StatusDrift,
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			fixture := newPublicationFixture(t, "ci-executor-authority-"+name)
			passPR(t, fixture)
			fixture.ci.observation = test.observation

			result, err := fixture.manager.ObserveCI(context.Background(), fixture.parsed.PublicationID)
			if err != nil {
				t.Fatalf("observe CI: %v", err)
			}
			if result.Status != test.wantStatus {
				t.Fatalf("CI result status = %q, want %q", result.Status, test.wantStatus)
			}
			publicationRow, err := fixture.db.GetPublication(fixture.parsed.PublicationID)
			if err != nil {
				t.Fatal(err)
			}
			run, err := fixture.db.GetRun(publicationRow.RunID)
			if err != nil {
				t.Fatal(err)
			}
			if run.Status != types.RunRunning {
				t.Fatalf("Manager terminalized Run as %s; Executor must remain sole lifecycle authority", run.Status)
			}
			projected, err := fixture.manager.Status(context.Background(), fixture.parsed.PublicationID)
			if err != nil {
				t.Fatal(err)
			}
			if projected.Status != test.wantStatus {
				t.Fatalf("durable CI projection = %q, want %q", projected.Status, test.wantStatus)
			}
		})
	}
}

func TestCIPendingObservationCanBecomeReadyAtTheSameExactHead(t *testing.T) {
	fixture := newPublicationFixture(t, "ci-pending-then-ready")
	passPR(t, fixture)
	fixture.ci.observations = []CIObservation{
		{HeadSHA: testCommitA, Checks: []CICheck{{Name: "test", HeadSHA: testCommitA, Status: CICheckPending}}},
		{HeadSHA: testCommitA, Checks: []CICheck{{Name: "test", HeadSHA: testCommitA, Status: CICheckPass}}},
	}

	pending, err := fixture.manager.ObserveCI(context.Background(), fixture.parsed.PublicationID)
	if err != nil {
		t.Fatalf("observe pending CI: %v", err)
	}
	if pending.Status != StatusCIObserving {
		t.Fatalf("pending CI status=%q, want %q", pending.Status, StatusCIObserving)
	}
	ready, err := fixture.manager.ObserveCI(context.Background(), fixture.parsed.PublicationID)
	if err != nil {
		t.Fatalf("observe ready CI after pending: %v", err)
	}
	if ready.Status != StatusReady || ready.HeadSHA != testCommitA {
		t.Fatalf("second exact-H CI observation=%+v, want READY at %s", ready, testCommitA)
	}
	if fixture.ci.observeCalls != 2 {
		t.Fatalf("CI observation calls=%d, want two read-only observations", fixture.ci.observeCalls)
	}
}

func hashText(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func errorsIs(got, want error) bool {
	return got != nil && (got == want || got.Error() == want.Error())
}
