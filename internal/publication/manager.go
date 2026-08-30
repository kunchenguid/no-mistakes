package publication

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const (
	publicationDriftErrorPrefix  = "publication drift: "
	publicationDeniedErrorPrefix = db.PublicationDenialErrorPrefix
	observationUnavailableJSON   = `{"outcome":"UNKNOWN","reason":"observation_transport_error"}`
)

type StepOutcome string

const (
	StepOutcomePass        StepOutcome = "PASS"
	StepOutcomeFail        StepOutcome = "FAIL"
	StepOutcomeError       StepOutcome = "ERROR"
	StepOutcomeSkipped     StepOutcome = "SKIPPED"
	StepOutcomePartial     StepOutcome = "PARTIAL"
	StepOutcomeNotExecuted StepOutcome = "NOT_EXECUTED"
)

type ManagerDeps struct {
	DB        *db.DB
	Candidate CandidateGuardPort
	Push      PushPort
	PR        PRPort
	CI        CIPort
}

// Manager is the publication guard and effect service used by the existing
// pipeline Executor. It deliberately owns no scheduler, loop, retry policy, or
// resume loop; ordered step execution remains the Executor's responsibility.
type Manager struct {
	db        *db.DB
	candidate CandidateGuardPort
	push      PushPort
	pr        PRPort
	ci        CIPort

	mu     sync.Mutex
	before map[stepGuardKey]CandidateSnapshot
}

type stepGuardKey struct {
	publicationID string
	step          types.StepName
}

func NewManager(deps ManagerDeps) (*Manager, error) {
	if deps.DB == nil {
		return nil, fmt.Errorf("publication manager database is required")
	}
	if deps.Candidate == nil {
		return nil, fmt.Errorf("publication candidate port is required")
	}
	if deps.Push == nil {
		return nil, fmt.Errorf("publication push port is required")
	}
	if deps.PR == nil {
		return nil, fmt.Errorf("publication PR port is required")
	}
	if deps.CI == nil {
		return nil, fmt.Errorf("publication CI port is required")
	}
	return &Manager{
		db:        deps.DB,
		candidate: deps.Candidate,
		push:      deps.Push,
		pr:        deps.PR,
		ci:        deps.CI,
		before:    make(map[stepGuardKey]CandidateSnapshot),
	}, nil
}

// PublicationStepPlan returns the one product step order. It is a fresh slice
// so a caller cannot mutate the normative order held by types.AllSteps.
func PublicationStepPlan() []types.StepName {
	return append([]types.StepName(nil), types.AllSteps()...)
}

func (m *Manager) Start(_ context.Context, parsed ParsedRequest) (Result, error) {
	repo, err := m.db.GetRepo(parsed.Request.Candidate.RepositoryID)
	if err != nil {
		return Result{}, fmt.Errorf("load publication repository before admission: %w", err)
	}
	if repo == nil {
		return Result{}, fmt.Errorf("publication repository %s is not registered", parsed.Request.Candidate.RepositoryID)
	}
	if strings.TrimSpace(repo.ForkURL) != "" {
		return Result{}, fmt.Errorf("factory publication v1 does not support fork routing")
	}
	registeredIdentity, err := canonicalEffectRemoteIdentity(repo.UpstreamURL)
	if err != nil {
		return Result{}, fmt.Errorf("resolve registered publication remote: %w", err)
	}
	boundIdentity, err := canonicalEffectRemoteIdentity(parsed.Request.Scopes.Push.RemoteIdentity)
	if err != nil || boundIdentity != registeredIdentity {
		return Result{}, fmt.Errorf("publication remote identity does not match the registered repository")
	}
	publication, run, created, err := m.db.CreateOrGetPublication(db.CreatePublicationInput{
		PublicationID:    parsed.PublicationID,
		CanonicalRequest: append([]byte(nil), parsed.CanonicalBytes...),
		RepoID:           parsed.Request.Candidate.RepositoryID,
		CandidateRef:     parsed.Request.Candidate.HeadRef,
		BaseRef:          parsed.Request.Candidate.BaseRef,
		HeadSHA:          parsed.Request.Candidate.CommitSHA,
		BaseSHA:          parsed.Request.Candidate.BaseSHA,
		TreeSHA:          parsed.Request.Candidate.TreeSHA,
	})
	if err != nil {
		return Result{}, err
	}
	if created || run.Status == types.RunPending {
		if err := m.db.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
			return Result{}, fmt.Errorf("start publication run: %w", err)
		}
		run.Status = types.RunRunning
	}
	return m.publicResultFor(publication, run)
}

// Status projects the durable publication state without changing the Run,
// step results, or external effects.
func (m *Manager) Status(_ context.Context, publicationID string) (Result, error) {
	publication, run, err := m.loadPublicationRun(publicationID)
	if err != nil {
		return Result{}, err
	}
	return m.publicResultFor(publication, run)
}

// ValidateIntent proves that the durable publication row still contains one
// canonical request bound to this exact Run and candidate. It is read-only:
// the Executor owns the Intent step's lifecycle transition.
func (m *Manager) ValidateIntent(_ context.Context, publicationID string) error {
	publication, run, err := m.loadPublicationRun(publicationID)
	if err != nil {
		return err
	}
	parsed, err := ParseRequest(publication.CanonicalRequest)
	if err != nil {
		return fmt.Errorf("parse stored publication request: %w", err)
	}
	request := parsed.Request
	if parsed.PublicationID != publication.PublicationID ||
		request.Candidate.RepositoryID != publication.RepoID ||
		request.Candidate.HeadRef != publication.CandidateRef ||
		request.Candidate.BaseRef != publication.BaseRef ||
		request.Candidate.BaseSHA != publication.BaseSHA ||
		request.Candidate.CommitSHA != publication.HeadSHA ||
		request.Candidate.TreeSHA != publication.TreeSHA ||
		run.RepoID != publication.RepoID ||
		run.Branch != publication.CandidateRef ||
		run.HeadSHA != publication.HeadSHA ||
		run.BaseSHA != publication.BaseSHA {
		return fmt.Errorf("stored publication request no longer matches its exact Run and candidate binding")
	}
	return nil
}

// BeforeDefense records the exact pre-step candidate observation without
// changing Run or step-result lifecycle state. The Executor starts and ends
// the step row; this service only owns tamper evidence.
func (m *Manager) BeforeDefense(ctx context.Context, publicationID string, step types.StepName) error {
	if !candidateGuardedStep(step) {
		return nil
	}
	publication, run, err := m.loadPublicationRun(publicationID)
	if err != nil {
		return err
	}
	if run.Status.Terminal() {
		return fmt.Errorf("publication is terminal with run status %s", run.Status)
	}
	snapshot, err := m.candidate.Inspect(ctx, publicationID, step)
	if err != nil {
		return publicationDriftError(fmt.Errorf("inspect candidate before %s: %w", step, err))
	}
	if err := validateCandidateSnapshot(snapshot, CandidateBinding{
		CommitSHA: publication.HeadSHA,
		TreeSHA:   publication.TreeSHA,
	}); err != nil {
		return publicationDriftError(err)
	}
	key := stepGuardKey{publicationID: publicationID, step: step}
	m.mu.Lock()
	if _, exists := m.before[key]; exists {
		m.mu.Unlock()
		return fmt.Errorf("publication defense guard for %s is already active", step)
	}
	m.before[key] = snapshot
	m.mu.Unlock()
	return nil
}

// AfterDefense compares the post-step candidate observation with both the
// exact request binding and the pre-step snapshot. It never writes Run or
// step-result status; its caller returns any error to the Executor.
func (m *Manager) AfterDefense(ctx context.Context, publicationID string, step types.StepName, outcome StepOutcome) error {
	if !candidateGuardedStep(step) {
		if outcome != StepOutcomePass {
			return fmt.Errorf("publication step %s returned %q", step, outcome)
		}
		return nil
	}
	publication, _, err := m.loadPublicationRun(publicationID)
	if err != nil {
		return err
	}
	before, ok := m.takeBefore(publicationID, step)
	if !ok {
		return fmt.Errorf("publication step %s has no candidate preflight snapshot", step)
	}
	after, err := m.candidate.Inspect(ctx, publicationID, step)
	if err != nil {
		return publicationDriftError(fmt.Errorf("inspect candidate after %s: %w", step, err))
	}
	binding := CandidateBinding{CommitSHA: publication.HeadSHA, TreeSHA: publication.TreeSHA}
	if err := validateCandidateSnapshot(after, binding); err != nil {
		return publicationDriftError(err)
	}
	if err := compareCandidateSnapshots(before, after); err != nil {
		return publicationDriftError(err)
	}
	if outcome != StepOutcomePass {
		return fmt.Errorf("publication step %s returned %q", step, outcome)
	}
	return nil
}

func (m *Manager) BeginStep(ctx context.Context, publicationID string, step types.StepName) error {
	publication, run, err := m.loadPublicationRun(publicationID)
	if err != nil {
		return err
	}
	if run.Status.Terminal() {
		return fmt.Errorf("publication is terminal with run status %s", run.Status)
	}
	stepResult, err := m.stepResult(run.ID, step)
	if err != nil {
		return err
	}
	if stepResult.Status != types.StepStatusPending {
		return fmt.Errorf("publication step %s is %s, want pending", step, stepResult.Status)
	}

	if guardErr := m.BeforeDefense(ctx, publicationID, step); guardErr != nil {
		status, failure := publicationFailure(guardErr)
		_, terminalErr := m.failStep(publication, run, stepResult, status, failure)
		if terminalErr != nil {
			return terminalErr
		}
		return guardErr
	}

	if err := m.db.StartStep(stepResult.ID); err != nil {
		m.deleteBefore(publicationID, step)
		return fmt.Errorf("start publication step %s: %w", step, err)
	}
	return nil
}

func (m *Manager) CompleteStep(ctx context.Context, publicationID string, step types.StepName, outcome StepOutcome) (Result, error) {
	publication, run, err := m.loadPublicationRun(publicationID)
	if err != nil {
		return Result{}, err
	}
	if run.Status.Terminal() {
		return m.resultFor(publication, run)
	}
	stepResult, err := m.stepResult(run.ID, step)
	if err != nil {
		return Result{}, err
	}
	if stepResult.Status != types.StepStatusRunning {
		return Result{}, fmt.Errorf("publication step %s is %s, want running", step, stepResult.Status)
	}

	if guardErr := m.AfterDefense(ctx, publicationID, step, outcome); guardErr != nil {
		status, failure := publicationFailure(guardErr)
		return m.failStep(publication, run, stepResult, status, failure)
	}
	if step == types.StepReview {
		if err := m.db.CompleteReviewStep(stepResult.ID, run.ID, publication.HeadSHA, 0, 0, ""); err != nil {
			return Result{}, fmt.Errorf("complete publication review: %w", err)
		}
	} else if err := m.db.CompleteStep(stepResult.ID, 0, 0, ""); err != nil {
		return Result{}, fmt.Errorf("complete publication step %s: %w", step, err)
	}
	return m.resultFor(publication, run)
}

func (m *Manager) PreparePush(_ context.Context, publicationID string) (EffectChallenge, error) {
	publication, run, err := m.loadPublicationRun(publicationID)
	if err != nil {
		return EffectChallenge{}, err
	}
	result, err := m.resultFor(publication, run)
	if err != nil {
		return EffectChallenge{}, err
	}
	if result.Status != StatusReadyForPush {
		return EffectChallenge{}, fmt.Errorf("publication status is %s, want %s", result.Status, StatusReadyForPush)
	}
	parsed, err := ParseRequest(publication.CanonicalRequest)
	if err != nil {
		return EffectChallenge{}, fmt.Errorf("parse stored publication request: %w", err)
	}
	binding := db.PublicationEffectBinding{
		CandidateSHA:   publication.HeadSHA,
		RemoteIdentity: parsed.Request.Scopes.Push.RemoteIdentity,
		DestinationRef: parsed.Request.Scopes.Push.DestinationRef,
		HeadRef:        parsed.Request.Candidate.HeadRef,
	}
	binding.EffectDigest, err = effectDigest(binding)
	if err != nil {
		return EffectChallenge{}, err
	}
	effect, err := m.db.PlanPublicationEffect(db.PlanPublicationEffectInput{
		PublicationID: publicationID,
		Kind:          db.PublicationEffectPush,
		Binding:       binding,
	})
	if err != nil {
		return EffectChallenge{}, err
	}
	return challengeFor(publication, effect)
}

func (m *Manager) PreparePR(_ context.Context, publicationID string, body []byte) (EffectChallenge, error) {
	publication, run, err := m.loadPublicationRun(publicationID)
	if err != nil {
		return EffectChallenge{}, err
	}
	result, err := m.resultFor(publication, run)
	if err != nil {
		return EffectChallenge{}, err
	}
	if result.Status != StatusReadyForPR {
		return EffectChallenge{}, fmt.Errorf("publication status is %s, want %s", result.Status, StatusReadyForPR)
	}
	parsed, err := ParseRequest(publication.CanonicalRequest)
	if err != nil {
		return EffectChallenge{}, fmt.Errorf("parse stored publication request: %w", err)
	}
	marker := reconciliationMarker(publication)
	draft, err := finalizedPRDraft(body, marker)
	if err != nil {
		return EffectChallenge{}, err
	}
	draftSum := sha256Hex(draft)
	binding := db.PublicationEffectBinding{
		CandidateSHA:   publication.HeadSHA,
		RemoteIdentity: parsed.Request.Scopes.Push.RemoteIdentity,
		DestinationRef: parsed.Request.Scopes.Push.DestinationRef,
		BaseRef:        parsed.Request.Scopes.PR.BaseRef,
		HeadRef:        parsed.Request.Scopes.PR.HeadRef,
		DraftDigest:    draftSum,
	}
	binding.EffectDigest, err = effectDigest(binding)
	if err != nil {
		return EffectChallenge{}, err
	}
	effect, err := m.db.PlanPublicationEffect(db.PlanPublicationEffectInput{
		PublicationID:   publicationID,
		Kind:            db.PublicationEffectPR,
		Binding:         binding,
		PreparedPayload: draft,
	})
	if err != nil {
		return EffectChallenge{}, err
	}
	return challengeFor(publication, effect)
}

func (m *Manager) Authorize(_ context.Context, authorization Authorization) (Result, error) {
	publication, run, err := m.loadPublicationRun(authorization.PublicationID)
	if err != nil {
		return Result{}, err
	}
	if run.Status.Terminal() && authorization.Decision != DecisionDeny {
		return Result{}, fmt.Errorf("terminal publication Run cannot authorize an external effect")
	}
	effect, dbKind, err := m.effect(authorization.PublicationID, authorization.Kind)
	if err != nil {
		return Result{}, err
	}
	challenge, err := challengeFor(publication, effect)
	if err != nil {
		return Result{}, err
	}
	if !authorizationMatches(challenge, authorization) {
		return Result{}, fmt.Errorf("publication authorization does not match the exact effect challenge")
	}
	if authorization.Decision == DecisionDeny {
		if _, err := m.db.DenyPublicationEffect(db.DenyPublicationEffectInput{
			PublicationID:  authorization.PublicationID,
			Kind:           dbKind,
			Binding:        effect.Binding,
			DecisionDigest: authorization.DecisionDigest,
		}); err != nil {
			return Result{}, err
		}
		updatedRun, err := m.db.GetRun(run.ID)
		if err != nil {
			return Result{}, fmt.Errorf("reload denied publication Run: %w", err)
		}
		if updatedRun == nil {
			return Result{}, fmt.Errorf("denied publication Run disappeared")
		}
		return m.publicResultFor(publication, updatedRun)
	}
	if _, err := m.db.AuthorizePublicationEffect(db.AuthorizePublicationEffectInput{
		PublicationID:  authorization.PublicationID,
		Kind:           dbKind,
		Binding:        effect.Binding,
		DecisionDigest: authorization.DecisionDigest,
	}); err != nil {
		return Result{}, err
	}
	return m.publicResultFor(publication, run)
}

const publicationAuthorizationPollInterval = 50 * time.Millisecond

// WaitForAuthorization waits only for the durable decision bound to challenge.
// It performs no effect and owns no workflow transition; an external
// publication authorize command is the sole writer that can release it.
func (m *Manager) WaitForAuthorization(ctx context.Context, challenge EffectChallenge) error {
	if challenge.Kind != EffectPush && challenge.Kind != EffectPR {
		return fmt.Errorf("unknown publication effect kind %q", challenge.Kind)
	}
	ticker := time.NewTicker(publicationAuthorizationPollInterval)
	defer ticker.Stop()
	for {
		publication, run, err := m.loadPublicationRun(challenge.PublicationID)
		if err != nil {
			return err
		}
		if run.Status.Terminal() {
			if run.Error != nil {
				return fmt.Errorf("publication authorization ended: %s", *run.Error)
			}
			return fmt.Errorf("publication authorization ended with run status %s", run.Status)
		}
		effect, _, err := m.effect(challenge.PublicationID, challenge.Kind)
		if err != nil {
			return err
		}
		current, err := challengeFor(publication, effect)
		if err != nil {
			return err
		}
		if !sameEffectChallenge(current, challenge) {
			return fmt.Errorf("publication effect challenge changed while awaiting authorization")
		}
		if effect.DecisionDigest != nil {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (m *Manager) ExecutePush(ctx context.Context, publicationID string) (Result, error) {
	publication, run, err := m.loadPublicationRun(publicationID)
	if err != nil {
		return Result{}, err
	}
	if run.Status.Terminal() {
		return Result{}, fmt.Errorf("terminal publication Run cannot execute Push")
	}
	effect, err := m.db.GetPublicationEffect(publicationID, db.PublicationEffectPush)
	if err != nil {
		return Result{}, err
	}
	if effect == nil {
		return Result{}, fmt.Errorf("push effect is not prepared")
	}
	if effect.State == db.PublicationEffectObserved {
		return m.resultFor(publication, run)
	}
	if effect.DecisionDigest == nil {
		return Result{}, db.ErrPublicationAuthorizationRequired
	}
	if _, err := m.db.BeginPublicationEffect(db.BeginPublicationEffectInput{
		PublicationID:  publicationID,
		Kind:           db.PublicationEffectPush,
		Binding:        effect.Binding,
		DecisionDigest: *effect.DecisionDigest,
	}); err != nil {
		return Result{}, err
	}
	request := pushRequest(publication, effect)
	if err := m.push.PublishExact(ctx, request); err != nil {
		// BeginPublicationEffect durably consumed the single-use decision before
		// the port call. A returned error cannot prove whether the external
		// effect happened, so observe the exact binding now instead of failing
		// the Run or allowing a later caller to replay the Push.
		return m.reconcilePush(ctx, publication, run, effect)
	}
	return m.reconcilePush(ctx, publication, run, effect)
}

func (m *Manager) ExecutePR(ctx context.Context, publicationID string) (Result, error) {
	publication, run, err := m.loadPublicationRun(publicationID)
	if err != nil {
		return Result{}, err
	}
	if run.Status.Terminal() {
		return Result{}, fmt.Errorf("terminal publication Run cannot execute PR")
	}
	effect, err := m.db.GetPublicationEffect(publicationID, db.PublicationEffectPR)
	if err != nil {
		return Result{}, err
	}
	if effect == nil {
		return Result{}, fmt.Errorf("PR effect is not prepared")
	}
	if effect.State == db.PublicationEffectObserved {
		return m.resultFor(publication, run)
	}
	if effect.DecisionDigest == nil {
		return Result{}, db.ErrPublicationAuthorizationRequired
	}
	draft := append([]byte(nil), effect.PreparedPayload...)
	if len(draft) == 0 || sha256Hex(draft) != effect.Binding.DraftDigest {
		return Result{}, fmt.Errorf("exact prepared PR draft is unavailable")
	}
	if _, err := m.db.BeginPublicationEffect(db.BeginPublicationEffectInput{
		PublicationID:  publicationID,
		Kind:           db.PublicationEffectPR,
		Binding:        effect.Binding,
		DecisionDigest: *effect.DecisionDigest,
	}); err != nil {
		return Result{}, err
	}
	request := prRequest(publication, effect, draft)
	if err := m.pr.CreateExact(ctx, request); err != nil {
		// The provider may have accepted the exact PR before returning an error.
		// Reconcile the durable binding immediately; zero or multiple matches
		// become EFFECT_UNKNOWN and the consumed decision is never replayed.
		return m.reconcilePR(ctx, publication, run, effect)
	}
	return m.reconcilePR(ctx, publication, run, effect)
}

func (m *Manager) RecoverEffect(ctx context.Context, publicationID string, kind EffectKind) (Result, error) {
	publication, run, err := m.loadPublicationRun(publicationID)
	if err != nil {
		return Result{}, err
	}
	effect, _, err := m.effect(publicationID, kind)
	if err != nil {
		return Result{}, err
	}
	switch effect.State {
	case db.PublicationEffectObserved, db.PublicationEffectUnknown, db.PublicationEffectFailed:
		return m.resultFor(publication, run)
	}
	if effect.EffectStartedAt == nil {
		return m.resultFor(publication, run)
	}
	switch kind {
	case EffectPush:
		return m.reconcilePush(ctx, publication, run, effect)
	case EffectPR:
		return m.reconcilePR(ctx, publication, run, effect)
	default:
		return Result{}, fmt.Errorf("unknown publication effect kind %q", kind)
	}
}

func (m *Manager) ObserveCI(ctx context.Context, publicationID string) (Result, error) {
	publication, run, err := m.loadPublicationRun(publicationID)
	if err != nil {
		return Result{}, err
	}
	result, err := m.resultFor(publication, run)
	if err != nil {
		return Result{}, err
	}
	if result.Status != StatusCIObserving {
		return Result{}, fmt.Errorf("publication status is %s, want %s", result.Status, StatusCIObserving)
	}
	parsed, err := ParseRequest(publication.CanonicalRequest)
	if err != nil {
		return Result{}, fmt.Errorf("parse stored publication request: %w", err)
	}
	binding := db.PublicationEffectBinding{
		CandidateSHA:   publication.HeadSHA,
		RemoteIdentity: parsed.Request.Scopes.Push.RemoteIdentity,
		DestinationRef: parsed.Request.Scopes.Push.DestinationRef,
		BaseRef:        parsed.Request.Scopes.PR.BaseRef,
		HeadRef:        parsed.Request.Scopes.PR.HeadRef,
	}
	binding.EffectDigest, err = effectDigest(binding)
	if err != nil {
		return Result{}, err
	}
	_, err = m.db.PlanPublicationEffect(db.PlanPublicationEffectInput{
		PublicationID: publicationID,
		Kind:          db.PublicationEffectCI,
		Binding:       binding,
	})
	if err != nil {
		return Result{}, err
	}
	if _, err := m.db.BeginPublicationEffect(db.BeginPublicationEffectInput{
		PublicationID: publicationID,
		Kind:          db.PublicationEffectCI,
		Binding:       binding,
	}); err != nil {
		return Result{}, err
	}
	observation, err := m.ci.ObserveExact(ctx, CIQuery{PublicationID: publicationID, CommitSHA: publication.HeadSHA})
	if err != nil {
		return Result{}, err
	}
	observationBytes, err := json.Marshal(observation)
	if err != nil {
		return Result{}, fmt.Errorf("marshal CI observation: %w", err)
	}
	if exactCIPassed(observation, publication.HeadSHA) {
		if _, err := m.db.ConcludePublicationEffect(db.ConcludePublicationEffectInput{
			PublicationID: publicationID,
			Kind:          db.PublicationEffectCI,
			Binding:       binding,
			State:         db.PublicationEffectObserved,
			Observation:   observationBytes,
		}); err != nil {
			return Result{}, err
		}
		return m.resultFor(publication, run)
	}

	status := StatusFailed
	if observation.HeadSHA != publication.HeadSHA {
		status = StatusDrift
	} else if ciObservationPending(observation) {
		// Pending is not success, but it is still an observation that can become
		// conclusive without changing the candidate or issuing an external effect.
		return m.resultWithStatus(publication, run, StatusCIObserving), nil
	}
	if _, err := m.db.ConcludePublicationEffect(db.ConcludePublicationEffectInput{
		PublicationID: publicationID,
		Kind:          db.PublicationEffectCI,
		Binding:       binding,
		State:         db.PublicationEffectFailed,
		Observation:   observationBytes,
	}); err != nil {
		return Result{}, err
	}
	return m.resultWithStatus(publication, run, status), nil
}

func (m *Manager) reconcilePush(ctx context.Context, publication *db.Publication, run *db.Run, effect *db.PublicationEffect) (Result, error) {
	request := pushRequest(publication, effect)
	observation, err := m.push.ObserveExact(ctx, request)
	if err != nil {
		return m.concludeObservationUnknown(publication, run, effect, db.PublicationEffectPush)
	}
	observationBytes, err := json.Marshal(observation)
	if err != nil {
		return Result{}, fmt.Errorf("marshal push observation: %w", err)
	}
	state := db.PublicationEffectUnknown
	if observation.RemoteHeadSHA == publication.HeadSHA {
		state = db.PublicationEffectObserved
	}
	if _, err := m.db.ConcludePublicationEffect(db.ConcludePublicationEffectInput{
		PublicationID: publication.PublicationID,
		Kind:          db.PublicationEffectPush,
		Binding:       effect.Binding,
		State:         state,
		Observation:   observationBytes,
	}); err != nil {
		return Result{}, err
	}
	return m.resultFor(publication, run)
}

func (m *Manager) reconcilePR(ctx context.Context, publication *db.Publication, run *db.Run, effect *db.PublicationEffect) (Result, error) {
	query := prQuery(publication, effect)
	observations, err := m.pr.FindExact(ctx, query)
	if err != nil {
		return m.concludeObservationUnknown(publication, run, effect, db.PublicationEffectPR)
	}
	matches := exactPRMatches(observations, query)
	observationBytes, err := json.Marshal(observations)
	if err != nil {
		return Result{}, fmt.Errorf("marshal PR observations: %w", err)
	}
	state := db.PublicationEffectUnknown
	if len(matches) == 1 {
		state = db.PublicationEffectObserved
	}
	if _, err := m.db.ConcludePublicationEffect(db.ConcludePublicationEffectInput{
		PublicationID: publication.PublicationID,
		Kind:          db.PublicationEffectPR,
		Binding:       effect.Binding,
		State:         state,
		Observation:   observationBytes,
	}); err != nil {
		return Result{}, err
	}
	return m.resultFor(publication, run)
}

// concludeObservationUnknown closes a durably started mutating effect when its
// exact observation transport fails. The provider error is deliberately not
// persisted: it may contain credentials or unstable transport details. UNKNOWN
// is the durable fail-closed fact, and prevents both generic Run failure and a
// later replay of the already-consumed Owner decision.
func (m *Manager) concludeObservationUnknown(
	publication *db.Publication,
	run *db.Run,
	effect *db.PublicationEffect,
	kind db.PublicationEffectKind,
) (Result, error) {
	if _, err := m.db.ConcludePublicationEffect(db.ConcludePublicationEffectInput{
		PublicationID: publication.PublicationID,
		Kind:          kind,
		Binding:       effect.Binding,
		State:         db.PublicationEffectUnknown,
		Observation:   []byte(observationUnavailableJSON),
	}); err != nil {
		return Result{}, err
	}
	return m.resultFor(publication, run)
}

func (m *Manager) failStep(publication *db.Publication, run *db.Run, step *db.StepResult, status ResultStatus, failure error) (Result, error) {
	if err := m.db.FailStep(step.ID, failure.Error(), 0); err != nil {
		return Result{}, fmt.Errorf("fail publication step %s: %w", step.StepName, err)
	}
	return m.failRun(publication, run, status, failure)
}

func (m *Manager) failRun(publication *db.Publication, run *db.Run, status ResultStatus, failure error) (Result, error) {
	message := failure.Error()
	if status == StatusDrift {
		message = publicationDriftErrorPrefix + message
	} else if status == StatusDenied {
		message = publicationDeniedErrorPrefix + message
	}
	if err := m.db.UpdateRunErrorStatus(run.ID, message, types.RunFailed); err != nil {
		return Result{}, fmt.Errorf("fail publication run: %w", err)
	}
	run.Status = types.RunFailed
	run.Error = &message
	return m.resultFor(publication, run)
}

func publicationDriftError(err error) error {
	if err == nil {
		return nil
	}
	if strings.HasPrefix(err.Error(), publicationDriftErrorPrefix) {
		return err
	}
	return fmt.Errorf("%s%w", publicationDriftErrorPrefix, err)
}

func publicationFailure(err error) (ResultStatus, error) {
	if err != nil && strings.HasPrefix(err.Error(), publicationDriftErrorPrefix) {
		return StatusDrift, errors.New(strings.TrimPrefix(err.Error(), publicationDriftErrorPrefix))
	}
	return StatusFailed, err
}

func sameEffectChallenge(left, right EffectChallenge) bool {
	return left == right
}

func (m *Manager) loadPublicationRun(publicationID string) (*db.Publication, *db.Run, error) {
	publication, err := m.db.GetPublication(publicationID)
	if err != nil {
		return nil, nil, err
	}
	if publication == nil {
		return nil, nil, fmt.Errorf("publication %s not found", publicationID)
	}
	run, err := m.db.GetRun(publication.RunID)
	if err != nil {
		return nil, nil, err
	}
	if run == nil || run.Kind != db.RunKindFactoryPublicationV1 {
		return nil, nil, fmt.Errorf("publication %s has no factory-publication-v1 run", publicationID)
	}
	return publication, run, nil
}

func (m *Manager) stepResult(runID string, step types.StepName) (*db.StepResult, error) {
	steps, err := m.db.GetStepsByRun(runID)
	if err != nil {
		return nil, err
	}
	for _, candidate := range steps {
		if candidate.StepName == step {
			return candidate, nil
		}
	}
	return nil, fmt.Errorf("publication step %s is missing", step)
}

func (m *Manager) resultFor(publication *db.Publication, run *db.Run) (Result, error) {
	if run.Status == types.RunFailed {
		status := StatusFailed
		if run.Error != nil && strings.HasPrefix(*run.Error, publicationDriftErrorPrefix) {
			status = StatusDrift
		} else if run.Error != nil && strings.HasPrefix(*run.Error, publicationDeniedErrorPrefix) {
			status = StatusDenied
		} else {
			ci, err := m.db.GetPublicationEffect(publication.PublicationID, db.PublicationEffectCI)
			if err != nil {
				return Result{}, err
			}
			status = ciEffectFailureStatus(ci, publication.HeadSHA)
		}
		return m.resultWithStatus(publication, run, status), nil
	}
	effects := make(map[db.PublicationEffectKind]*db.PublicationEffect, 3)
	for _, kind := range []db.PublicationEffectKind{db.PublicationEffectCI, db.PublicationEffectPR, db.PublicationEffectPush} {
		effect, err := m.db.GetPublicationEffect(publication.PublicationID, kind)
		if err != nil {
			return Result{}, err
		}
		effects[kind] = effect
		if effect == nil {
			continue
		}
		if effect.State == db.PublicationEffectUnknown {
			return m.resultWithStatus(publication, run, StatusEffectUnknown), nil
		}
		if effect.State == db.PublicationEffectFailed {
			status := StatusFailed
			if kind == db.PublicationEffectCI {
				status = ciEffectFailureStatus(effect, publication.HeadSHA)
			}
			return m.resultWithStatus(publication, run, status), nil
		}
	}
	if effect := effects[db.PublicationEffectCI]; effect != nil && effect.State == db.PublicationEffectObserved {
		return m.resultWithStatus(publication, run, StatusReady), nil
	}
	if effect := effects[db.PublicationEffectPR]; effect != nil && effect.State == db.PublicationEffectObserved {
		return m.resultWithStatus(publication, run, StatusCIObserving), nil
	}
	if push := effects[db.PublicationEffectPush]; push != nil && push.State == db.PublicationEffectObserved {
		if pr := effects[db.PublicationEffectPR]; effectAwaitingDecision(pr) {
			return m.resultWithChallenge(publication, run, StatusReadyForPR, pr)
		}
		if pr := effects[db.PublicationEffectPR]; pr != nil && (pr.DecisionConsumedAt != nil || pr.EffectStartedAt != nil) {
			return m.resultWithStatus(publication, run, StatusChecking), nil
		}
		return m.resultWithStatus(publication, run, StatusReadyForPR), nil
	}
	steps, err := m.db.GetStepsByRun(run.ID)
	if err != nil {
		return Result{}, err
	}
	completed := make(map[types.StepName]bool, len(steps))
	for _, step := range steps {
		completed[step.StepName] = step.Status == types.StepStatusCompleted
	}
	for _, required := range []types.StepName{types.StepIntent, types.StepRebase, types.StepReview, types.StepTest, types.StepDocument, types.StepLint} {
		if !completed[required] {
			return m.resultWithStatus(publication, run, StatusChecking), nil
		}
	}
	if push := effects[db.PublicationEffectPush]; effectAwaitingDecision(push) {
		return m.resultWithChallenge(publication, run, StatusReadyForPush, push)
	}
	if push := effects[db.PublicationEffectPush]; push != nil && (push.DecisionConsumedAt != nil || push.EffectStartedAt != nil) {
		return m.resultWithStatus(publication, run, StatusChecking), nil
	}
	return m.resultWithStatus(publication, run, StatusReadyForPush), nil
}

func ciEffectFailureStatus(effect *db.PublicationEffect, candidateSHA string) ResultStatus {
	if effect == nil || effect.State != db.PublicationEffectFailed {
		return StatusFailed
	}
	var observation CIObservation
	if err := json.Unmarshal(effect.Observation, &observation); err == nil && observation.HeadSHA != candidateSHA {
		return StatusDrift
	}
	return StatusFailed
}

// publicResultFor never exposes an Owner-gate status before the exact durable
// effect challenge exists. The existing Executor may be between completing the
// preceding step and preparing the next effect; that short internal boundary
// remains CHECKING rather than publishing an unusable authorization surface.
func (m *Manager) publicResultFor(publication *db.Publication, run *db.Run) (Result, error) {
	result, err := m.resultFor(publication, run)
	if err != nil {
		return Result{}, err
	}
	if (result.Status == StatusReadyForPush || result.Status == StatusReadyForPR) && result.Challenge == nil {
		return m.resultWithStatus(publication, run, StatusChecking), nil
	}
	return result, nil
}

func effectAwaitingDecision(effect *db.PublicationEffect) bool {
	return effect != nil &&
		effect.State == db.PublicationEffectPlanned && effect.DecisionDigest == nil &&
		effect.DecisionConsumedAt == nil && effect.EffectStartedAt == nil
}

func (m *Manager) resultWithChallenge(
	publication *db.Publication,
	run *db.Run,
	status ResultStatus,
	effect *db.PublicationEffect,
) (Result, error) {
	challenge, err := challengeFor(publication, effect)
	if err != nil {
		return Result{}, err
	}
	result := m.resultWithStatus(publication, run, status)
	result.Challenge = &challenge
	return result, nil
}

func (m *Manager) resultWithStatus(publication *db.Publication, run *db.Run, status ResultStatus) Result {
	return Result{
		Protocol:      ProtocolV1,
		PublicationID: publication.PublicationID,
		RunID:         run.ID,
		HeadSHA:       publication.HeadSHA,
		Status:        status,
	}
}

func (m *Manager) effect(publicationID string, kind EffectKind) (*db.PublicationEffect, db.PublicationEffectKind, error) {
	var dbKind db.PublicationEffectKind
	switch kind {
	case EffectPush:
		dbKind = db.PublicationEffectPush
	case EffectPR:
		dbKind = db.PublicationEffectPR
	default:
		return nil, "", fmt.Errorf("unknown publication effect kind %q", kind)
	}
	effect, err := m.db.GetPublicationEffect(publicationID, dbKind)
	if err != nil {
		return nil, "", err
	}
	if effect == nil {
		return nil, "", fmt.Errorf("publication effect %s is not prepared", kind)
	}
	return effect, dbKind, nil
}

func challengeFor(publication *db.Publication, effect *db.PublicationEffect) (EffectChallenge, error) {
	if err := validateChallengeEffectBinding(publication, effect); err != nil {
		return EffectChallenge{}, err
	}
	kind := EffectKind(effect.Kind)
	challenge := EffectChallenge{
		PublicationID:  publication.PublicationID,
		Kind:           kind,
		Attempt:        1,
		CommitSHA:      effect.Binding.CandidateSHA,
		RemoteIdentity: effect.Binding.RemoteIdentity,
		DestinationRef: effect.Binding.DestinationRef,
		BaseRef:        effect.Binding.BaseRef,
		HeadRef:        effect.Binding.HeadRef,
		DraftSHA256:    effect.Binding.DraftDigest,
		EffectDigest:   effect.Binding.EffectDigest,
	}
	if kind == EffectPR {
		challenge.Marker = reconciliationMarker(publication)
		challenge.PreparedDraft = string(effect.PreparedPayload)
	}
	return BindEffectChallengeDecisions(challenge)
}

func validateChallengeEffectBinding(publication *db.Publication, effect *db.PublicationEffect) error {
	if publication == nil || effect == nil || effect.PublicationID != publication.PublicationID {
		return fmt.Errorf("publication effect is not bound to the requested publication")
	}
	parsed, err := ParseRequest(publication.CanonicalRequest)
	if err != nil {
		return fmt.Errorf("parse stored publication request for effect challenge: %w", err)
	}
	request := parsed.Request
	if parsed.PublicationID != publication.PublicationID ||
		request.Candidate.RepositoryID != publication.RepoID ||
		request.Candidate.HeadRef != publication.CandidateRef ||
		request.Candidate.BaseRef != publication.BaseRef ||
		request.Candidate.BaseSHA != publication.BaseSHA ||
		request.Candidate.CommitSHA != publication.HeadSHA ||
		request.Candidate.TreeSHA != publication.TreeSHA {
		return fmt.Errorf("stored publication request no longer matches the durable candidate binding")
	}
	binding := effect.Binding
	if binding.CandidateSHA != publication.HeadSHA {
		return fmt.Errorf("publication effect candidate does not match exact H")
	}
	digestBinding := binding
	digestBinding.EffectDigest = ""
	wantEffectDigest, err := effectDigest(digestBinding)
	if err != nil {
		return err
	}
	if binding.EffectDigest != wantEffectDigest {
		return fmt.Errorf("publication effect digest does not match its durable binding")
	}

	switch effect.Kind {
	case db.PublicationEffectPush:
		if binding.RemoteIdentity != request.Scopes.Push.RemoteIdentity ||
			binding.DestinationRef != request.Scopes.Push.DestinationRef ||
			binding.HeadRef != request.Candidate.HeadRef || binding.BaseRef != "" || binding.DraftDigest != "" ||
			len(effect.PreparedPayload) != 0 {
			return fmt.Errorf("Push effect no longer matches the canonical request scope")
		}
	case db.PublicationEffectPR:
		marker := reconciliationMarker(publication)
		if binding.RemoteIdentity != request.Scopes.Push.RemoteIdentity ||
			binding.DestinationRef != request.Scopes.Push.DestinationRef ||
			binding.BaseRef != request.Scopes.PR.BaseRef || binding.HeadRef != request.Scopes.PR.HeadRef ||
			len(effect.PreparedPayload) == 0 || len(effect.PreparedPayload) > MaxPreparedPRDraftBytes ||
			!utf8.Valid(effect.PreparedPayload) || sha256Hex(effect.PreparedPayload) != binding.DraftDigest ||
			bytes.Count(effect.PreparedPayload, []byte(marker)) != 1 {
			return fmt.Errorf("PR effect no longer matches its canonical scope and exact prepared draft")
		}
	default:
		return fmt.Errorf("publication Owner challenge cannot target effect kind %q", effect.Kind)
	}
	return nil
}

func pushRequest(publication *db.Publication, effect *db.PublicationEffect) PushEffectRequest {
	return PushEffectRequest{
		PublicationID:  publication.PublicationID,
		RepositoryID:   publication.RepoID,
		CommitSHA:      publication.HeadSHA,
		RemoteIdentity: effect.Binding.RemoteIdentity,
		DestinationRef: effect.Binding.DestinationRef,
		EffectDigest:   effect.Binding.EffectDigest,
	}
}

func prRequest(publication *db.Publication, effect *db.PublicationEffect, draft []byte) PREffectRequest {
	return PREffectRequest{
		PublicationID: publication.PublicationID,
		RepositoryID:  publication.RepoID,
		BaseRef:       effect.Binding.BaseRef,
		HeadRef:       effect.Binding.HeadRef,
		CommitSHA:     publication.HeadSHA,
		Marker:        reconciliationMarker(publication),
		Draft:         append([]byte(nil), draft...),
		DraftSHA256:   effect.Binding.DraftDigest,
		EffectDigest:  effect.Binding.EffectDigest,
	}
}

func prQuery(publication *db.Publication, effect *db.PublicationEffect) PRReconcileQuery {
	return PRReconcileQuery{
		PublicationID: publication.PublicationID,
		RepositoryID:  publication.RepoID,
		BaseRef:       effect.Binding.BaseRef,
		HeadRef:       effect.Binding.HeadRef,
		CommitSHA:     publication.HeadSHA,
		Marker:        reconciliationMarker(publication),
		DraftSHA256:   effect.Binding.DraftDigest,
	}
}

func reconciliationMarker(publication *db.Publication) string {
	return fmt.Sprintf("<!-- no-mistakes-factory-publication-v1:%s:%s -->", publication.PublicationID, publication.HeadSHA)
}

func sha256Hex(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func ciObservationPending(observation CIObservation) bool {
	if observation.HeadSHA == "" || len(observation.Checks) == 0 {
		return false
	}
	for _, check := range observation.Checks {
		if check.Status != CICheckPending {
			return false
		}
	}
	return true
}

func (m *Manager) takeBefore(publicationID string, step types.StepName) (CandidateSnapshot, bool) {
	key := stepGuardKey{publicationID: publicationID, step: step}
	m.mu.Lock()
	defer m.mu.Unlock()
	snapshot, ok := m.before[key]
	delete(m.before, key)
	return snapshot, ok
}

func (m *Manager) deleteBefore(publicationID string, step types.StepName) {
	key := stepGuardKey{publicationID: publicationID, step: step}
	m.mu.Lock()
	delete(m.before, key)
	m.mu.Unlock()
}
