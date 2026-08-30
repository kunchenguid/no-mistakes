package steps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/publication"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// FactoryPublicationManager is the guard and external-effect surface used by
// the publication step adapter. It deliberately excludes Manager's legacy
// BeginStep/CompleteStep methods: the existing Executor is the sole owner of
// step_results lifecycle transitions.
type FactoryPublicationManager interface {
	ValidateIntent(ctx context.Context, publicationID string) error
	BeforeDefense(ctx context.Context, publicationID string, step types.StepName) error
	AfterDefense(ctx context.Context, publicationID string, step types.StepName, outcome publication.StepOutcome) error
	PreparePush(ctx context.Context, publicationID string) (publication.EffectChallenge, error)
	PreparePR(ctx context.Context, publicationID string, draft []byte) (publication.EffectChallenge, error)
	WaitForAuthorization(ctx context.Context, challenge publication.EffectChallenge) error
	ExecutePush(ctx context.Context, publicationID string) (publication.Result, error)
	ExecutePR(ctx context.Context, publicationID string) (publication.Result, error)
	ObserveCI(ctx context.Context, publicationID string) (publication.Result, error)
}

// FactoryPublicationCandidatePort owns one fresh candidate view per guarded
// step. Manager observes the same view through its CandidateGuardPort.
type FactoryPublicationCandidatePort interface {
	PrepareStep(ctx context.Context, publicationID string, step types.StepName) (publication.CandidateStepView, error)
	DisposeStep(ctx context.Context, publicationID string, step types.StepName) error
}

// FactoryPublicationFreshnessPort performs the protected profile's read-only
// replacement for the ordinary, mutating Rebase step.
type FactoryPublicationFreshnessPort interface {
	CheckUpToDate(ctx context.Context, publicationID string, view publication.CandidateStepView) error
}

type FactoryPublicationStepAdapterOptions struct {
	PublicationID string
	Manager       FactoryPublicationManager
	Candidate     FactoryPublicationCandidatePort
	Freshness     FactoryPublicationFreshnessPort
	RenderPRDraft func(context.Context, string) ([]byte, error)
	CommandRunner pipeline.PublicationCommandRunner
}

type factoryPublicationStepAdapter struct {
	publicationID string
	manager       FactoryPublicationManager
	candidate     FactoryPublicationCandidatePort
	freshness     FactoryPublicationFreshnessPort
	renderPRDraft func(context.Context, string) ([]byte, error)
	commandRunner pipeline.PublicationCommandRunner
}

func NewFactoryPublicationStepAdapter(options FactoryPublicationStepAdapterOptions) (pipeline.PublicationStepAdapter, error) {
	if strings.TrimSpace(options.PublicationID) == "" {
		return nil, fmt.Errorf("publication ID is required")
	}
	if options.Manager == nil {
		return nil, fmt.Errorf("publication manager is required")
	}
	if options.Candidate == nil {
		return nil, fmt.Errorf("publication candidate port is required")
	}
	if options.Freshness == nil {
		return nil, fmt.Errorf("publication freshness port is required")
	}
	if options.RenderPRDraft == nil {
		return nil, fmt.Errorf("publication PR draft renderer is required")
	}
	return &factoryPublicationStepAdapter{
		publicationID: strings.TrimSpace(options.PublicationID),
		manager:       options.Manager,
		candidate:     options.Candidate,
		freshness:     options.Freshness,
		renderPRDraft: options.RenderPRDraft,
		commandRunner: options.CommandRunner,
	}, nil
}

func (a *factoryPublicationStepAdapter) ExecutePublicationStep(sctx *pipeline.StepContext, step pipeline.Step) (*pipeline.StepOutcome, error) {
	if sctx == nil || sctx.Ctx == nil || sctx.Run == nil {
		return nil, fmt.Errorf("publication step context and run are required")
	}
	if step == nil {
		return nil, fmt.Errorf("publication step is required")
	}

	switch step.Name() {
	case types.StepIntent:
		if err := a.manager.ValidateIntent(sctx.Ctx, a.publicationID); err != nil {
			return nil, err
		}
		return publicationPassOutcome(step.Name(), sctx.Run.HeadSHA), nil
	case types.StepRebase:
		return a.executeGuarded(sctx, step.Name(), func(_ *pipeline.StepContext, view publication.CandidateStepView) (*pipeline.StepOutcome, error) {
			if err := a.freshness.CheckUpToDate(sctx.Ctx, a.publicationID, view); err != nil {
				return nil, err
			}
			return &pipeline.StepOutcome{}, nil
		})
	case types.StepReview, types.StepTest, types.StepDocument, types.StepLint:
		return a.executeGuarded(sctx, step.Name(), func(candidateContext *pipeline.StepContext, _ publication.CandidateStepView) (*pipeline.StepOutcome, error) {
			return step.Execute(candidateContext)
		})
	case types.StepPush:
		return a.executePush(sctx)
	case types.StepPR:
		return a.executePR(sctx)
	case types.StepCI:
		return a.observeCI(sctx)
	default:
		return nil, fmt.Errorf("unsupported factory publication step %s", step.Name())
	}
}

func (a *factoryPublicationStepAdapter) executeGuarded(
	sctx *pipeline.StepContext,
	step types.StepName,
	execute func(*pipeline.StepContext, publication.CandidateStepView) (*pipeline.StepOutcome, error),
) (*pipeline.StepOutcome, error) {
	view, err := a.candidate.PrepareStep(sctx.Ctx, a.publicationID, step)
	if err != nil {
		return nil, fmt.Errorf("prepare publication candidate for %s: %w", step, err)
	}
	dispose := func() error {
		if err := a.candidate.DisposeStep(sctx.Ctx, a.publicationID, step); err != nil {
			return fmt.Errorf("dispose publication candidate for %s: %w", step, err)
		}
		return nil
	}
	if err := validateCandidateStepView(view); err != nil {
		return nil, errors.Join(err, dispose())
	}
	if err := a.manager.BeforeDefense(sctx.Ctx, a.publicationID, step); err != nil {
		return nil, errors.Join(err, dispose())
	}

	candidateContext, err := candidateStepContext(sctx, view, a.commandRunner)
	var outcome *pipeline.StepOutcome
	if err == nil {
		outcome, err = execute(candidateContext, view)
	}
	guardOutcome, classificationErr := classifyPublicationStepOutcome(outcome, err)
	err = errors.Join(err, classificationErr)
	if guardOutcome == publication.StepOutcomePass && candidateContext.Run.HeadSHA != sctx.Run.HeadSHA {
		guardOutcome = publication.StepOutcomeError
		err = fmt.Errorf("publication step %s changed its exact-H run binding", step)
	}
	if guardOutcome == publication.StepOutcomePass && outcome.ReviewApprovedHeadSHA != "" &&
		(step != types.StepReview || outcome.ReviewApprovedHeadSHA != sctx.Run.HeadSHA) {
		guardOutcome = publication.StepOutcomeError
		err = fmt.Errorf("publication step %s returned a review binding other than exact H", step)
	}
	if guardOutcome != publication.StepOutcomePass && err == nil {
		err = fmt.Errorf("publication step %s returned non-pass outcome %s", step, guardOutcome)
	}
	afterErr := a.manager.AfterDefense(sctx.Ctx, a.publicationID, step, guardOutcome)
	returnOutcome := publicationPassOutcome(step, sctx.Run.HeadSHA)
	if errors.Is(err, agent.ErrPublicationConfinementCleanupUncertain) || errors.Is(afterErr, agent.ErrPublicationConfinementCleanupUncertain) {
		return nil, errors.Join(err, afterErr)
	}
	if combined := errors.Join(err, afterErr, dispose()); combined != nil {
		return nil, combined
	}
	return returnOutcome, nil
}

func (a *factoryPublicationStepAdapter) executePush(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	challenge, err := a.manager.PreparePush(sctx.Ctx, a.publicationID)
	if err != nil {
		return nil, err
	}
	if err := validateEffectChallenge(challenge, a.publicationID, publication.EffectPush, sctx.Run.HeadSHA); err != nil {
		return nil, err
	}
	if err := a.manager.WaitForAuthorization(sctx.Ctx, challenge); err != nil {
		return nil, err
	}
	result, err := a.manager.ExecutePush(sctx.Ctx, a.publicationID)
	if err != nil {
		return nil, err
	}
	if err := validatePublicationResult(result, a.publicationID, sctx.Run.HeadSHA, publication.StatusReadyForPR); err != nil {
		return nil, err
	}
	return publicationPassOutcome(types.StepPush, sctx.Run.HeadSHA), nil
}

func (a *factoryPublicationStepAdapter) executePR(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	draft, err := a.renderPRDraft(sctx.Ctx, a.publicationID)
	if err != nil {
		return nil, fmt.Errorf("render publication PR draft: %w", err)
	}
	challenge, err := a.manager.PreparePR(sctx.Ctx, a.publicationID, draft)
	if err != nil {
		return nil, err
	}
	if err := validateEffectChallenge(challenge, a.publicationID, publication.EffectPR, sctx.Run.HeadSHA); err != nil {
		return nil, err
	}
	if err := a.manager.WaitForAuthorization(sctx.Ctx, challenge); err != nil {
		return nil, err
	}
	result, err := a.manager.ExecutePR(sctx.Ctx, a.publicationID)
	if err != nil {
		return nil, err
	}
	if err := validatePublicationResult(result, a.publicationID, sctx.Run.HeadSHA, publication.StatusCIObserving); err != nil {
		return nil, err
	}
	return publicationPassOutcome(types.StepPR, sctx.Run.HeadSHA), nil
}

func (a *factoryPublicationStepAdapter) observeCI(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	timeout := config.DefaultCITimeout
	if sctx.Config != nil && sctx.Config.CITimeout > 0 {
		timeout = sctx.Config.CITimeout
	}
	ctx, cancel := context.WithTimeout(sctx.Ctx, timeout)
	defer cancel()

	for {
		result, err := a.manager.ObserveCI(ctx, a.publicationID)
		if err != nil {
			return nil, err
		}
		if err := validatePublicationResultBinding(result, a.publicationID, sctx.Run.HeadSHA); err != nil {
			return nil, err
		}
		switch result.Status {
		case publication.StatusReady:
			return publicationPassOutcome(types.StepCI, sctx.Run.HeadSHA), nil
		case publication.StatusCIObserving:
			timer := time.NewTimer(50 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, fmt.Errorf("wait for exact-H CI: %w", ctx.Err())
			case <-timer.C:
			}
		default:
			return nil, fmt.Errorf("publication CI returned closed non-ready status %s", result.Status)
		}
	}
}

func candidateStepContext(source *pipeline.StepContext, view publication.CandidateStepView, runner pipeline.PublicationCommandRunner) (*pipeline.StepContext, error) {
	evidenceDir := filepath.Join(view.ScratchDir, "evidence")
	if err := os.MkdirAll(evidenceDir, 0o700); err != nil {
		return nil, fmt.Errorf("create publication evidence directory: %w", err)
	}
	if err := os.Chmod(evidenceDir, 0o700); err != nil {
		return nil, fmt.Errorf("protect publication evidence directory: %w", err)
	}

	copy := *source
	copy.WorkDir = view.WorktreeDir
	copy.GateDir = ""
	copy.EvidenceDir = evidenceDir
	copy.Env = withPublicationScratchEnv(source.Env, view.ScratchDir)
	copy.PublicationDefense = true
	copy.PublicationScratchDir = view.ScratchDir
	if source.Repo != nil {
		copy.PublicationSourceDir = source.Repo.WorkingPath
	}
	if runner != nil {
		copy.PublicationCommandRunner = runner
	}
	if source.Run != nil {
		runCopy := *source.Run
		copy.Run = &runCopy
	}
	if source.Repo != nil {
		repoCopy := *source.Repo
		repoCopy.WorkingPath = view.WorktreeDir
		copy.Repo = &repoCopy
	}
	return &copy, nil
}

const publicationDefensePromptContract = `Publication defense mode is read-only. Inspect and report only.
- These rules override any earlier fix, retry, or action language in the step prompt.
- Do not create, edit, delete, stage, commit, or fix files in the candidate.
- Do not request or perform an auto-fix, retry, rebase, or restart.
- Report the current exact candidate as PASS or with structured findings; later publication policy decides the closed result.`

func rejectPublicationDefenseFixState(sctx *pipeline.StepContext, step types.StepName) error {
	if sctx != nil && sctx.PublicationDefense && (sctx.Fixing || sctx.SkipFixExecution) {
		return fmt.Errorf("publication defense step %s forbids fix execution", step)
	}
	return nil
}

func publicationDefensePromptSection(sctx *pipeline.StepContext) string {
	if sctx == nil || !sctx.PublicationDefense {
		return ""
	}
	return "\n\n" + publicationDefensePromptContract
}

func validateCandidateStepView(view publication.CandidateStepView) error {
	if strings.TrimSpace(view.WorktreeDir) == "" || strings.TrimSpace(view.ScratchDir) == "" {
		return fmt.Errorf("publication candidate worktree and scratch directories are required")
	}
	worktree, err := filepath.Abs(view.WorktreeDir)
	if err != nil {
		return fmt.Errorf("resolve publication candidate worktree: %w", err)
	}
	scratch, err := filepath.Abs(view.ScratchDir)
	if err != nil {
		return fmt.Errorf("resolve publication candidate scratch: %w", err)
	}
	if pathsOverlapForPublication(worktree, scratch) {
		return fmt.Errorf("publication candidate worktree and scratch directories must be disjoint")
	}
	return nil
}

func pathsOverlapForPublication(left, right string) bool {
	within := func(path, parent string) bool {
		rel, err := filepath.Rel(parent, path)
		return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
	}
	return within(left, right) || within(right, left)
}

func withPublicationScratchEnv(existing []string, scratch string) []string {
	overrides := map[string]string{
		"TMPDIR": scratch,
		"TMP":    scratch,
		"TEMP":   scratch,
	}
	result := make([]string, 0, len(existing)+len(overrides))
	for _, entry := range existing {
		key := envKey(entry)
		if _, replace := overrides[key]; !replace {
			result = append(result, entry)
		}
	}
	for _, key := range []string{"TMPDIR", "TMP", "TEMP"} {
		result = append(result, key+"="+overrides[key])
	}
	return result
}

func classifyPublicationStepOutcome(outcome *pipeline.StepOutcome, executionErr error) (publication.StepOutcome, error) {
	if executionErr != nil {
		return publication.StepOutcomeError, nil
	}
	if outcome == nil {
		return publication.StepOutcomeNotExecuted, nil
	}
	if outcome.Skipped || outcome.SkipRemaining {
		return publication.StepOutcomeSkipped, nil
	}
	if outcome.ExitCode != 0 {
		return publication.StepOutcomeFail, nil
	}
	if outcome.NeedsApproval || outcome.AutoFixable || outcome.RestartFrom != "" ||
		strings.TrimSpace(outcome.PRURL) != "" || outcome.DurationOverrideMS != 0 {
		return publication.StepOutcomePartial, nil
	}
	if strings.TrimSpace(outcome.Findings) != "" {
		findings, err := parsePublicationDefenseFindings(outcome.Findings)
		if err != nil {
			return publication.StepOutcomePartial, err
		}
		for _, finding := range findings.Items {
			if types.NormalizeFindingSeverity(finding.Severity) != types.FindingSeverityInfo ||
				types.NormalizeFindingAction(finding.Action) != types.ActionNoOp {
				return publication.StepOutcomeFail, nil
			}
		}
	}
	return publication.StepOutcomePass, nil
}

func parsePublicationDefenseFindings(raw string) (types.Findings, error) {
	findings, err := types.ParseFindingsJSON(raw)
	if err != nil {
		return types.Findings{}, fmt.Errorf("parse publication defense findings: %w", err)
	}
	var envelope struct {
		Findings *json.RawMessage `json:"findings"`
		Items    *json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
		return types.Findings{}, fmt.Errorf("parse publication defense findings envelope: %w", err)
	}
	if envelope.Findings == nil && envelope.Items == nil {
		return types.Findings{}, fmt.Errorf("publication defense findings payload has no findings array")
	}
	for index, finding := range findings.Items {
		if !types.IsKnownFindingSeverity(finding.Severity) ||
			!types.IsKnownFindingAction(finding.Action) ||
			strings.TrimSpace(finding.Description) == "" {
			return types.Findings{}, fmt.Errorf("publication defense finding %d is malformed", index+1)
		}
	}
	return findings, nil
}

func publicationPassOutcome(step types.StepName, headSHA string) *pipeline.StepOutcome {
	outcome := &pipeline.StepOutcome{}
	if step == types.StepReview {
		outcome.ReviewApprovedHeadSHA = headSHA
	}
	return outcome
}

func validateEffectChallenge(challenge publication.EffectChallenge, publicationID string, kind publication.EffectKind, headSHA string) error {
	if challenge.PublicationID != publicationID || challenge.Kind != kind || challenge.CommitSHA != headSHA {
		return fmt.Errorf("publication %s challenge is not bound to exact publication and head", kind)
	}
	return nil
}

func validatePublicationResult(result publication.Result, publicationID, headSHA string, status publication.ResultStatus) error {
	if err := validatePublicationResultBinding(result, publicationID, headSHA); err != nil {
		return err
	}
	if result.Status != status {
		return fmt.Errorf("publication result is not exact %s at head %s", status, headSHA)
	}
	return nil
}

func validatePublicationResultBinding(result publication.Result, publicationID, headSHA string) error {
	if result.Protocol != publication.ProtocolV1 || result.PublicationID != publicationID ||
		result.HeadSHA != headSHA || strings.TrimSpace(result.RunID) == "" {
		return fmt.Errorf("publication result is not bound to exact publication and head %s", headSHA)
	}
	return nil
}
