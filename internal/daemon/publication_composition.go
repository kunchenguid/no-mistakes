package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	pipelinesteps "github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/publication"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type publicationRuntimeCandidatePort interface {
	publication.CandidatePort
	CheckUpToDate(context.Context, string, publication.CandidateStepView) error
}

type publicationExactConfigLoader func(context.Context, *paths.Paths, *config.GlobalConfig, *db.Repo, []byte) (*exactPublicationConfig, error)
type publicationAgentFactory func(context.Context, *exactPublicationConfig) (agent.Agent, error)

var errPublicationConfinementUnavailable = agent.ErrPublicationConfinementUnavailable

// publicationDefenseBoundary is deliberately unexported. N0 has no production
// implementation: implementations exist only in _test.go or the dedicated
// offline-E2E build-tag file. Possessing this marker does not claim isolation.
type publicationDefenseBoundary interface {
	publicationDefenseBoundary()
}

// publicationCompositionOptions exposes only technical ports and test seams.
// Product semantics, ordering, resume and retry remain owned by Manager and
// the existing pipeline Executor.
type publicationCompositionOptions struct {
	Paths        *paths.Paths
	DB           *db.DB
	Runs         *RunManager
	GlobalConfig *config.GlobalConfig
	Identity     publication.PublisherBinding

	// TestOnlyUnconfinedDefenseBoundary enables core-composition tests without
	// pretending to provide process, filesystem, credential, or network
	// confinement. Production must leave this nil and fail closed.
	TestOnlyUnconfinedDefenseBoundary publicationDefenseBoundary
	ProductionBoundary                *agent.PublicationCodexBoundaryV1

	Candidate publicationRuntimeCandidatePort
	Push      publication.PushPort
	PR        publication.PRPort
	CI        publication.CIPort

	LoadConfig publicationExactConfigLoader
	NewAgent   publicationAgentFactory
}

type publicationComposition struct {
	runtime       *publicationRuntime
	manager       *publication.Manager
	identity      publication.PublisherBinding
	boundary      *agent.PublicationCodexBoundaryV1
	commandRunner pipeline.PublicationCommandRunner
	agentFactory  publicationAgentFactory
}

var daemonExecutablePath = os.Executable

func validatePublicationCodexProfile(cfg *config.Config) error {
	if cfg == nil || cfg.Agent != types.AgentCodex || len(cfg.Agents) != 1 || cfg.Agents[0] != types.AgentCodex {
		return fmt.Errorf("%w: publication requires exactly one Codex agent and no fallback", agent.ErrPublicationConfinementUnavailable)
	}
	if len(cfg.AgentArgsFor(types.AgentCodex)) != 0 {
		return fmt.Errorf("%w: publication forbids raw Codex execution overrides", agent.ErrPublicationConfinementUnavailable)
	}
	if cfg.AgentPathFor(types.AgentCodex) != "codex" || !cfg.AgentProfileFor(types.AgentCodex).IsZero() {
		return fmt.Errorf("%w: publication forbids alternate Codex paths and model/runtime profile overrides", agent.ErrPublicationConfinementUnavailable)
	}
	return nil
}

type productionPublicationCompositionOverride func(
	*paths.Paths,
	*db.DB,
	*RunManager,
	*config.GlobalConfig,
	publication.PublisherBinding,
) (*publicationComposition, bool, error)

// The normal build leaves this nil. A build-tagged offline E2E adapter may
// inject an explicitly unconfined test boundary and technical ports while
// exercising the same core runtime, Manager, Executor, and IPC handlers. It is
// not a production-confinement seam.
var overrideProductionPublicationComposition productionPublicationCompositionOverride

func currentDaemonPublicationIdentity() (publication.PublisherBinding, error) {
	executable, err := daemonExecutablePath()
	if err != nil {
		return publication.PublisherBinding{}, fmt.Errorf("resolve daemon executable: %w", err)
	}
	binding, err := publication.CurrentPublisherBinding(executable)
	if err != nil {
		return publication.PublisherBinding{}, fmt.Errorf("bind daemon publisher identity: %w", err)
	}
	return binding, nil
}

func newProductionPublicationComposition(
	p *paths.Paths,
	database *db.DB,
	runs *RunManager,
	global *config.GlobalConfig,
	identity publication.PublisherBinding,
) (*publicationComposition, error) {
	if overrideProductionPublicationComposition != nil {
		composition, handled, err := overrideProductionPublicationComposition(p, database, runs, global, identity)
		if handled {
			return composition, err
		}
	}
	boundary, err := agent.DiscoverProductionPublicationCodexBoundary(context.Background(), filepath.Join(p.Root(), "publication-codex-bootstrap"))
	if err != nil {
		return nil, err
	}
	if err := probeProductionPublicationBoundary(context.Background(), p, identity, boundary); err != nil {
		return nil, err
	}
	sessions, err := publication.NewForgePublicationSessionResolver(publication.ForgePublicationSessionResolverOptions{
		DB: database, Profiles: global.ForgeProfiles,
	})
	if err != nil {
		return nil, fmt.Errorf("compose publication provider session resolver: %w", err)
	}
	push, err := publication.NewGitPushPort(publication.GitPushPortOptions{DB: database, EnvironmentResolver: sessions})
	if err != nil {
		return nil, fmt.Errorf("compose publication Push port: %w", err)
	}
	github, err := publication.NewGitHubV1RoutingPort(publication.GitHubV1RoutingPortOptions{DB: database, Sessions: sessions})
	if err != nil {
		return nil, fmt.Errorf("compose publication GitHub routing ports: %w", err)
	}
	return newPublicationComposition(publicationCompositionOptions{
		Paths: p, DB: database, Runs: runs, GlobalConfig: global, Identity: identity,
		ProductionBoundary: boundary, Push: push, PR: github, CI: github,
	})
}

func newPublicationComposition(options publicationCompositionOptions) (*publicationComposition, error) {
	if (options.TestOnlyUnconfinedDefenseBoundary == nil) == (options.ProductionBoundary == nil) {
		return nil, fmt.Errorf("%w: publication defense execution requires an explicit boundary", errPublicationConfinementUnavailable)
	}
	if options.Paths == nil || options.DB == nil || options.Runs == nil || options.GlobalConfig == nil {
		return nil, fmt.Errorf("publication composition requires paths, database, RunManager, and startup global config")
	}
	if !validPublicationRPCIdentity(publicationIPCIdentity(options.Identity)) {
		return nil, fmt.Errorf("publication composition identity is invalid")
	}

	candidate := options.Candidate
	if candidate == nil {
		created, err := publication.NewGitCandidatePort(publication.GitCandidatePortOptions{
			DB: options.DB, Root: options.Paths.PublicationCandidatesDir(),
		})
		if err != nil {
			return nil, fmt.Errorf("compose publication candidate port: %w", err)
		}
		candidate = created
	}
	push := options.Push
	if push == nil {
		created, err := publication.NewGitPushPort(publication.GitPushPortOptions{DB: options.DB})
		if err != nil {
			return nil, fmt.Errorf("compose publication Push port: %w", err)
		}
		push = created
	}
	if options.PR == nil || options.CI == nil {
		return nil, fmt.Errorf("publication composition requires PR and CI routing ports")
	}

	manager, err := publication.NewManager(publication.ManagerDeps{
		DB: options.DB, Candidate: candidate, Push: push, PR: options.PR, CI: options.CI,
	})
	if err != nil {
		return nil, fmt.Errorf("compose publication manager: %w", err)
	}
	loadConfig := options.LoadConfig
	if loadConfig == nil {
		loadConfig = loadExactPublicationConfig
	}
	newAgent := options.NewAgent
	if newAgent == nil {
		newAgent = func(ctx context.Context, exact *exactPublicationConfig) (agent.Agent, error) {
			if exact == nil || exact.Config == nil {
				return nil, fmt.Errorf("exact publication config is unavailable")
			}
			if options.ProductionBoundary != nil {
				if err := validatePublicationCodexProfile(exact.Config); err != nil {
					return nil, err
				}
				return agent.NewPublicationCodexAgent(options.ProductionBoundary, nil)
			}
			return newPipelineAgent(ctx, exact.Config, options.Paths.EvidenceRoot(exact.Config.Test.Evidence.LocalRoot), exec.LookPath, exact.Environment)
		}
	}
	var commandRunner pipeline.PublicationCommandRunner
	if options.ProductionBoundary != nil {
		commandRunner = publicationCodexCommandRunner{boundary: options.ProductionBoundary}
	}

	executorFactory := func(ctx context.Context, publicationID string, run *db.Run, repo *db.Repo) (*publicationExecutorPlan, error) {
		if ctx == nil || publicationID == "" || run == nil || repo == nil {
			return nil, fmt.Errorf("publication executor composition is incomplete")
		}
		publicationRow, err := options.DB.GetPublication(publicationID)
		if err != nil {
			return nil, fmt.Errorf("load publication for executor composition: %w", err)
		}
		if publicationRow == nil || publicationRow.RunID != run.ID || publicationRow.RepoID != repo.ID ||
			publicationRow.HeadSHA != run.HeadSHA || run.Kind != types.RunKindFactoryPublicationV1 {
			return nil, fmt.Errorf("publication executor inputs do not match their durable binding")
		}
		exact, err := loadConfig(ctx, options.Paths, options.GlobalConfig, repo, publicationRow.CanonicalRequest)
		if err != nil {
			return nil, fmt.Errorf("load exact publication config: %w", err)
		}
		if exact == nil || exact.Config == nil {
			return nil, fmt.Errorf("exact publication config loader returned no config")
		}
		ag, err := newAgent(ctx, exact)
		if err != nil {
			return nil, fmt.Errorf("compose publication agent: %w", err)
		}
		if ag == nil {
			return nil, fmt.Errorf("publication agent factory returned no agent")
		}
		closeAgent := func() { _ = ag.Close() }

		adapter, err := pipelinesteps.NewFactoryPublicationStepAdapter(pipelinesteps.FactoryPublicationStepAdapterOptions{
			PublicationID: publicationID,
			Manager:       manager, Candidate: candidate, Freshness: candidate,
			RenderPRDraft: manager.RenderPRDraft,
			CommandRunner: commandRunner,
		})
		if err != nil {
			closeAgent()
			return nil, fmt.Errorf("compose publication step adapter: %w", err)
		}
		execSteps := options.Runs.steps()
		executor := pipeline.NewExecutor(options.DB, options.Paths, exact.Config, ag, execSteps, options.Runs.broadcast)
		executor.SetForgeContext(exact.Forge)
		executor.SetPublicationStepAdapter(adapter)
		return &publicationExecutorPlan{Executor: executor, WorkDir: repo.WorkingPath, Cleanup: closeAgent}, nil
	}

	var runtimeControl publicationControl = manager
	if options.ProductionBoundary != nil {
		runtimeControl = &publicationAdmissionControl{
			inner: manager, database: options.DB, paths: options.Paths, global: options.GlobalConfig,
			loadConfig: loadConfig, boundary: options.ProductionBoundary,
		}
	}
	runtime, err := newPublicationRuntime(publicationRuntimeOptions{
		DB: options.DB, Runs: options.Runs, Manager: runtimeControl, Identity: options.Identity,
		ExecutorFactory: executorFactory,
	})
	if err != nil {
		return nil, fmt.Errorf("compose publication runtime: %w", err)
	}
	return &publicationComposition{
		runtime: runtime, manager: manager, identity: options.Identity,
		boundary: options.ProductionBoundary, commandRunner: commandRunner, agentFactory: newAgent,
	}, nil
}

type publicationCodexCommandRunner struct {
	boundary *agent.PublicationCodexBoundaryV1
}

func (r publicationCodexCommandRunner) RunPublicationCommand(ctx context.Context, request pipeline.PublicationCommandRequest) (pipeline.PublicationCommandResult, error) {
	if r.boundary == nil {
		return pipeline.PublicationCommandResult{}, fmt.Errorf("%w: configured command boundary is unavailable", agent.ErrPublicationConfinementUnavailable)
	}
	view, err := r.boundary.BindView(request.WorkDir, request.SourceDir, request.ScratchDir)
	if err != nil {
		return pipeline.PublicationCommandResult{}, err
	}
	output, exitCode, err := view.RunConfiguredCommand(ctx, request.Command)
	return pipeline.PublicationCommandResult{Output: output, ExitCode: exitCode}, err
}

type publicationAdmissionControl struct {
	inner      publicationControl
	database   *db.DB
	paths      *paths.Paths
	global     *config.GlobalConfig
	loadConfig publicationExactConfigLoader
	boundary   *agent.PublicationCodexBoundaryV1
}

func (c *publicationAdmissionControl) Start(ctx context.Context, request publication.ParsedRequest) (publication.Result, error) {
	if c == nil || c.inner == nil || c.database == nil || c.paths == nil || c.global == nil || c.boundary == nil {
		return publication.Result{}, fmt.Errorf("%w: publication admission boundary is incomplete", agent.ErrPublicationConfinementUnavailable)
	}
	repo, err := c.database.GetRepo(request.Request.Candidate.RepositoryID)
	if err != nil || repo == nil {
		return publication.Result{}, fmt.Errorf("%w: load exact publication repository before admission", agent.ErrPublicationConfinementUnavailable)
	}
	loader := c.loadConfig
	if loader == nil {
		loader = loadExactPublicationConfig
	}
	exact, err := loader(ctx, c.paths, c.global, repo, request.CanonicalBytes)
	if err != nil {
		return publication.Result{}, fmt.Errorf("%w: exact publication config before admission: %v", agent.ErrPublicationConfinementUnavailable, err)
	}
	if exact == nil || exact.Config == nil {
		return publication.Result{}, fmt.Errorf("%w: exact publication config is unavailable", agent.ErrPublicationConfinementUnavailable)
	}
	if err := validatePublicationCodexProfile(exact.Config); err != nil {
		return publication.Result{}, err
	}
	for _, kind := range []agent.PublicationLaunchKind{agent.PublicationLaunchExec, agent.PublicationLaunchCommand} {
		if err := c.boundary.RevalidateForLaunch(kind); err != nil {
			return publication.Result{}, err
		}
	}
	return c.inner.Start(ctx, request)
}

func (c *publicationAdmissionControl) Authorize(ctx context.Context, authorization publication.Authorization) (publication.Result, error) {
	return c.inner.Authorize(ctx, authorization)
}

func (c *publicationAdmissionControl) Status(ctx context.Context, publicationID string) (publication.Result, error) {
	return c.inner.Status(ctx, publicationID)
}

func (c *publicationAdmissionControl) RecoverEffect(ctx context.Context, publicationID string, kind publication.EffectKind) (publication.Result, error) {
	return c.inner.RecoverEffect(ctx, publicationID, kind)
}

func probeProductionPublicationBoundary(ctx context.Context, p *paths.Paths, identity publication.PublisherBinding, boundary *agent.PublicationCodexBoundaryV1) error {
	if p == nil || boundary == nil || boundary.CanaryExecutable() == "" {
		return fmt.Errorf("%w: production capability probe is incomplete", agent.ErrPublicationConfinementUnavailable)
	}
	manifest := boundary.Manifest()
	canaryBound := false
	for _, binding := range manifest.ExecutableClosure {
		if binding.Role == agent.PublicationExecutableCanary && binding.RealPath == identity.ExecutablePath &&
			binding.RawSHA256 == identity.ExecutableSHA256 {
			canaryBound = true
			break
		}
	}
	if !canaryBound {
		return fmt.Errorf("%w: publication canary does not match exact daemon publisher bytes", agent.ErrPublicationConfinementUnavailable)
	}
	fixture, err := os.MkdirTemp(p.Root(), "publication-probe-")
	if err != nil {
		return fmt.Errorf("%w: create production capability fixture: %v", agent.ErrPublicationConfinementUnavailable, err)
	}
	retain := false
	defer func() {
		if !retain {
			_ = os.RemoveAll(fixture)
		}
	}()
	if err := os.Chmod(fixture, 0o700); err != nil {
		return err
	}
	candidate := filepath.Join(fixture, "candidate")
	scratch := filepath.Join(fixture, "scratch")
	sibling := filepath.Join(fixture, "sibling")
	source := filepath.Join(fixture, "source")
	for _, dir := range []string{candidate, scratch, sibling, source} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			return err
		}
	}
	if err := os.WriteFile(filepath.Join(candidate, "readable.txt"), []byte("candidate\n"), 0o400); err != nil {
		return err
	}
	sourceFile := filepath.Join(source, "secret")
	siblingFile := filepath.Join(sibling, "secret")
	for _, file := range []string{sourceFile, siblingFile} {
		if err := os.WriteFile(file, []byte("secret\n"), 0o600); err != nil {
			return err
		}
	}
	tcpListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("%w: open TCP negative control: %v", agent.ErrPublicationConfinementUnavailable, err)
	}
	defer tcpListener.Close()
	// The socket is deliberately under the writable scratch grant so the
	// negative proves AF_UNIX denial rather than merely a filesystem denial.
	unixPath := filepath.Join(scratch, "probe.sock")
	unixListener, err := net.Listen("unix", unixPath)
	if err != nil {
		return fmt.Errorf("%w: open Unix-socket negative control: %v", agent.ErrPublicationConfinementUnavailable, err)
	}
	defer unixListener.Close()
	probeCtx, cancelProbe := context.WithTimeout(ctx, 20*time.Second)
	defer cancelProbe()
	err = boundary.Probe(probeCtx, agent.PublicationCodexProbeOptions{
		CandidateDir: candidate, SourceDir: source, ScratchDir: scratch,
		SourceFile: sourceFile, SiblingFile: siblingFile,
		TCPAddress: tcpListener.Addr().String(), UnixSocketPath: unixPath,
	})
	if errors.Is(err, agent.ErrPublicationConfinementCleanupUncertain) {
		retain = true
	}
	if err != nil {
		return err
	}
	return nil
}

func (c *publicationComposition) registerHandlers(server *ipc.Server, guard publicationMutationGuard) {
	c.runtime.registerHandlers(server, publicationIPCIdentity(c.identity), guard)
}
