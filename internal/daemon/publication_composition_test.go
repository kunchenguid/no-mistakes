package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/buildinfo"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/publication"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type compositionCandidatePort struct{}

func (compositionCandidatePort) PrepareStep(context.Context, string, types.StepName) (publication.CandidateStepView, error) {
	return publication.CandidateStepView{}, nil
}
func (compositionCandidatePort) DisposeStep(context.Context, string, types.StepName) error {
	return nil
}
func (compositionCandidatePort) Inspect(context.Context, string, types.StepName) (publication.CandidateSnapshot, error) {
	return publication.CandidateSnapshot{}, nil
}
func (compositionCandidatePort) CheckUpToDate(context.Context, string, publication.CandidateStepView) error {
	return nil
}

type compositionAgent struct{ closed atomic.Bool }

type admissionControlFake struct{ startCalls atomic.Int32 }

func (f *admissionControlFake) Start(_ context.Context, request publication.ParsedRequest) (publication.Result, error) {
	f.startCalls.Add(1)
	return validPublicationRPCResult(request.PublicationID, publication.StatusChecking), nil
}
func (*admissionControlFake) Authorize(_ context.Context, authorization publication.Authorization) (publication.Result, error) {
	return validPublicationRPCResult(authorization.PublicationID, publication.StatusChecking), nil
}
func (*admissionControlFake) Status(_ context.Context, publicationID string) (publication.Result, error) {
	return validPublicationRPCResult(publicationID, publication.StatusChecking), nil
}
func (*admissionControlFake) RecoverEffect(_ context.Context, publicationID string, _ publication.EffectKind) (publication.Result, error) {
	return validPublicationRPCResult(publicationID, publication.StatusChecking), nil
}

type unitTestOnlyUnconfinedPublicationDefenseBoundary struct{}

func (unitTestOnlyUnconfinedPublicationDefenseBoundary) publicationDefenseBoundary() {}

func (*compositionAgent) Name() string { return "composition-test" }
func (*compositionAgent) Run(context.Context, agent.RunOpts) (*agent.Result, error) {
	return &agent.Result{}, nil
}
func (a *compositionAgent) Close() error { a.closed.Store(true); return nil }

func newDaemonPublicationBoundary(t *testing.T) *agent.PublicationCodexBoundaryV1 {
	t.Helper()
	dir := t.TempDir()
	write := func(name, body string) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		return path
	}
	native := write("native", `if [ "$1" = "--version" ]; then printf 'codex-cli 0.150.1\n'; exit 0; fi; exit 0`)
	boundary, err := agent.NewPublicationCodexBoundaryV1(context.Background(), agent.PublicationCodexBoundaryOptions{
		LogicalEntryPath: write("logical", "exit 97"), NativeCodexPath: native,
		SandboxHelperPath: write("helper", `exec "$@"`), BubblewrapPath: write("bwrap", `while [ "$#" -gt 0 ] && [ "$1" != "--" ]; do shift; done; shift; exec "$@"`),
		PermissionProfile: []byte("publication-v1"), ProbeFixedArgs: []string{"sandbox"},
		ExecFixedArgs: []string{"exec"}, CommandFixedArgs: []string{"sandbox"},
		BootstrapDir: filepath.Join(dir, "bootstrap"), ConfigHomeDir: filepath.Join(dir, "config-home"),
		SentinelExecutablePath: write("sentinel", `sleep "$1"`),
	})
	if err != nil {
		t.Fatalf("construct publication boundary: %v", err)
	}
	return boundary
}

func TestPublicationCompositionFailsClosedWithoutDefenseBoundaryBeforePortsOrAgent(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	runs := NewRunManager(database, p, func() []pipeline.Step { return runtimeSteps() })
	var agentCalls atomic.Int32

	composition, err := newPublicationComposition(publicationCompositionOptions{
		Paths: p, DB: database, Runs: runs, GlobalConfig: config.DefaultGlobalConfig(), Identity: validCompositionIdentity(),
		PR: &runtimePRPort{}, CI: runtimeCIPort{},
		NewAgent: func(context.Context, *exactPublicationConfig) (agent.Agent, error) {
			agentCalls.Add(1)
			return &compositionAgent{}, nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "confinement_unavailable") {
		t.Fatalf("composition without a defense boundary = %#v, %v; want confinement_unavailable", composition, err)
	}
	if composition != nil || runs.publicationRecovery != nil {
		t.Fatalf("unconfined composition installed runtime/recovery: composition=%#v recovery=%#v", composition, runs.publicationRecovery)
	}
	if agentCalls.Load() != 0 {
		t.Fatalf("unconfined composition constructed %d agents, want zero", agentCalls.Load())
	}
	if _, err := os.Lstat(p.PublicationCandidatesDir()); !os.IsNotExist(err) {
		t.Fatalf("unconfined composition constructed candidate port/root: %v", err)
	}
}

func TestPublicationCompositionAcceptsOnlyOneExactColdCodexProfile(t *testing.T) {
	exact := &config.Config{Agent: types.AgentCodex, Agents: []types.AgentName{types.AgentCodex}}
	if err := validatePublicationCodexProfile(exact); err != nil {
		t.Fatalf("exact single Codex profile rejected: %v", err)
	}

	for name, cfg := range map[string]*config.Config{
		"auto":     {Agent: types.AgentAuto, Agents: []types.AgentName{types.AgentAuto}},
		"other":    {Agent: types.AgentClaude, Agents: []types.AgentName{types.AgentClaude}},
		"fallback": {Agent: types.AgentCodex, Agents: []types.AgentName{types.AgentCodex, types.AgentClaude}},
		"raw execution override": {
			Agent: types.AgentCodex, Agents: []types.AgentName{types.AgentCodex},
			AgentArgsOverride: map[string][]string{"codex": {"--sandbox", "danger-full-access"}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := validatePublicationCodexProfile(cfg); !errors.Is(err, agent.ErrPublicationConfinementUnavailable) {
				t.Fatalf("profile %#v error=%v, want confinement_unavailable", cfg, err)
			}
		})
	}
}

func TestPublicationAdmissionChecksExactConfigAndBoundaryBeforeDurableStart(t *testing.T) {
	fixture := newExactPublicationConfigFixture(t, "<absent>", "<absent>")
	database, err := db.Open(fixture.paths.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if _, err := database.InsertRepoWithID(fixture.repo.ID, fixture.repo.WorkingPath, fixture.repo.UpstreamURL, fixture.repo.DefaultBranch); err != nil {
		t.Fatal(err)
	}
	request, err := publication.ParseRequest(fixture.raw)
	if err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name       string
		cfg        *config.Config
		loadErr    error
		returnNil  bool
		wantErr    bool
		wantStarts int32
	}{
		{name: "invalid fallback profile refuses before Start", cfg: &config.Config{Agent: types.AgentCodex, Agents: []types.AgentName{types.AgentCodex, types.AgentClaude}}, wantErr: true},
		{name: "alternate executable refuses before Start", cfg: &config.Config{Agent: types.AgentCodex, Agents: []types.AgentName{types.AgentCodex}, AgentPathOverride: map[string]string{"codex": "/tmp/foreign-codex"}}, wantErr: true},
		{name: "raw argv override refuses before Start", cfg: &config.Config{Agent: types.AgentCodex, Agents: []types.AgentName{types.AgentCodex}, AgentArgsOverride: map[string][]string{"codex": {"--sandbox", "danger-full-access"}}}, wantErr: true},
		{name: "model profile refuses before Start", cfg: &config.Config{Agent: types.AgentCodex, Agents: []types.AgentName{types.AgentCodex}, AgentConfig: map[string]agentcfg.Profile{"codex": {Model: "foreign-model"}}}, wantErr: true},
		{name: "loader error refuses before Start", loadErr: errors.New("exact config unavailable"), wantErr: true},
		{name: "nil exact config refuses before Start", returnNil: true, wantErr: true},
		{name: "exact cold Codex reaches Start", cfg: &config.Config{Agent: types.AgentCodex, Agents: []types.AgentName{types.AgentCodex}}, wantStarts: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			inner := &admissionControlFake{}
			control := &publicationAdmissionControl{
				inner: inner, database: database, paths: fixture.paths, global: config.DefaultGlobalConfig(),
				boundary: newDaemonPublicationBoundary(t),
				loadConfig: func(context.Context, *paths.Paths, *config.GlobalConfig, *db.Repo, []byte) (*exactPublicationConfig, error) {
					if test.loadErr != nil {
						return nil, test.loadErr
					}
					if test.returnNil {
						return nil, nil
					}
					return &exactPublicationConfig{Config: test.cfg}, nil
				},
			}
			_, err := control.Start(context.Background(), request)
			if test.wantErr {
				if !errors.Is(err, agent.ErrPublicationConfinementUnavailable) {
					t.Fatalf("admission error=%v, want confinement_unavailable", err)
				}
			} else if err != nil {
				t.Fatalf("exact admission: %v", err)
			}
			if got := inner.startCalls.Load(); got != test.wantStarts {
				t.Fatalf("durable Start calls=%d, want %d", got, test.wantStarts)
			}
		})
	}
	if admitted, err := database.GetPublication(request.PublicationID); err != nil || admitted != nil {
		t.Fatalf("admission decorator itself wrote publication state: row=%#v err=%v", admitted, err)
	}
}

func TestPublicationCoreCompositionWithExplicitUnconfinedTestBoundaryConstructsPrivateCandidateRoot(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	runs := NewRunManager(database, p, func() []pipeline.Step { return runtimeSteps() })
	identity := validCompositionIdentity()
	var configCalls atomic.Int32
	var agentCalls atomic.Int32

	composition, err := newPublicationComposition(publicationCompositionOptions{
		Paths: p, DB: database, Runs: runs, GlobalConfig: config.DefaultGlobalConfig(), Identity: identity,
		TestOnlyUnconfinedDefenseBoundary: unitTestOnlyUnconfinedPublicationDefenseBoundary{},
		PR:                                &runtimePRPort{}, CI: runtimeCIPort{},
		LoadConfig: func(context.Context, *paths.Paths, *config.GlobalConfig, *db.Repo, []byte) (*exactPublicationConfig, error) {
			configCalls.Add(1)
			return nil, nil
		},
		NewAgent: func(context.Context, *exactPublicationConfig) (agent.Agent, error) {
			agentCalls.Add(1)
			return &compositionAgent{}, nil
		},
	})
	if err != nil {
		t.Fatalf("compose publication runtime: %v", err)
	}
	if composition.runtime == nil || runs.publicationRecovery != composition.runtime {
		t.Fatal("composition did not register runtime recovery before startup")
	}
	if configCalls.Load() != 0 || agentCalls.Load() != 0 {
		t.Fatalf("construction reached lazy config/agent/auth work: config=%d agent=%d", configCalls.Load(), agentCalls.Load())
	}
	root := p.PublicationCandidatesDir()
	info, err := os.Lstat(root)
	if err != nil {
		t.Fatalf("inspect candidate root: %v", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o700 {
		t.Fatalf("candidate root mode/type = %s, want private real directory", info.Mode())
	}
	if filepath.Clean(root) == filepath.Clean(p.WorktreesDir()) || filepath.Clean(root) == filepath.Clean(p.ReposDir()) {
		t.Fatalf("candidate root %q overlaps existing managed roots", root)
	}
}

func TestPublicationCoreCompositionUsesOneExactBoundaryForAgentCommandsAndAdmission(t *testing.T) {
	fixture := newExactPublicationConfigFixture(t, "<absent>", "<absent>")
	database, err := db.Open(fixture.paths.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	runs := NewRunManager(database, fixture.paths, func() []pipeline.Step { return runtimeSteps() })
	boundary := newDaemonPublicationBoundary(t)
	exact := &exactPublicationConfig{Config: &config.Config{Agent: types.AgentCodex, Agents: []types.AgentName{types.AgentCodex}}}
	composition, err := newPublicationComposition(publicationCompositionOptions{
		Paths: fixture.paths, DB: database, Runs: runs, GlobalConfig: config.DefaultGlobalConfig(), Identity: validCompositionIdentity(),
		ProductionBoundary: boundary, Candidate: compositionCandidatePort{}, Push: &runtimePushPort{}, PR: &runtimePRPort{}, CI: runtimeCIPort{},
		LoadConfig: func(context.Context, *paths.Paths, *config.GlobalConfig, *db.Repo, []byte) (*exactPublicationConfig, error) {
			return exact, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if composition.boundary != boundary {
		t.Fatal("composition did not retain its one exact production boundary")
	}
	runner, ok := composition.commandRunner.(publicationCodexCommandRunner)
	if !ok || runner.boundary != boundary {
		t.Fatalf("configured-command runner does not use the exact production boundary: %#v", composition.commandRunner)
	}
	control, ok := composition.runtime.manager.(*publicationAdmissionControl)
	if !ok || control.boundary != boundary {
		t.Fatalf("admission control does not use the exact production boundary: %#v", composition.runtime.manager)
	}
	protected, err := composition.agentFactory(context.Background(), exact)
	if err != nil {
		t.Fatal(err)
	}
	defer protected.Close()
	if !agent.IsPublicationConfinementAgent(protected) || agent.IsFallbackAgent(protected) || agent.SupportsSessionResume(protected) {
		t.Fatalf("agent factory did not produce the exact cold no-fallback boundary agent: %#v", protected)
	}
}

func TestPublicationCoreCompositionWithExplicitUnconfinedTestBoundaryUsesExactConfigAndClosesAgent(t *testing.T) {
	fixture := newExactPublicationConfigFixture(t,
		"allow_repo_commands: false\ncommands:\n  lint: trusted-lint\n",
		"allow_repo_commands: true\ncommands:\n  lint: pushed-lint\nignore_patterns:\n  - pushed-only\n",
	)
	database, err := db.Open(fixture.paths.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	repo, err := database.InsertRepoWithID(fixture.repo.ID, fixture.repo.WorkingPath, fixture.repo.UpstreamURL, fixture.repo.DefaultBranch)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := publication.ParseRequest(fixture.raw)
	if err != nil {
		t.Fatal(err)
	}
	publicationRow, run, _, err := database.CreateOrGetPublication(db.CreatePublicationInput{
		PublicationID: parsed.PublicationID, CanonicalRequest: parsed.CanonicalBytes,
		RepoID: repo.ID, CandidateRef: parsed.Request.Candidate.HeadRef, BaseRef: parsed.Request.Candidate.BaseRef,
		HeadSHA: parsed.Request.Candidate.CommitSHA, BaseSHA: parsed.Request.Candidate.BaseSHA, TreeSHA: parsed.Request.Candidate.TreeSHA,
	})
	if err != nil {
		t.Fatal(err)
	}
	runs := NewRunManager(database, fixture.paths, func() []pipeline.Step { return runtimeSteps() })
	global := config.DefaultGlobalConfig()
	var loaded *exactPublicationConfig
	createdAgent := &compositionAgent{}
	composition, err := newPublicationComposition(publicationCompositionOptions{
		Paths: fixture.paths, DB: database, Runs: runs, GlobalConfig: global, Identity: parsed.Request.Publisher,
		TestOnlyUnconfinedDefenseBoundary: unitTestOnlyUnconfinedPublicationDefenseBoundary{},
		Candidate:                         compositionCandidatePort{}, Push: &runtimePushPort{}, PR: &runtimePRPort{}, CI: runtimeCIPort{},
		LoadConfig: func(ctx context.Context, p *paths.Paths, cfg *config.GlobalConfig, repo *db.Repo, raw []byte) (*exactPublicationConfig, error) {
			var err error
			loaded, err = loadExactPublicationConfig(ctx, p, cfg, repo, raw)
			return loaded, err
		},
		NewAgent: func(_ context.Context, exact *exactPublicationConfig) (agent.Agent, error) {
			if exact != loaded {
				t.Fatal("agent factory did not receive the exact loaded projection")
			}
			return createdAgent, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := composition.runtime.factory(context.Background(), publicationRow.PublicationID, run, repo)
	if err != nil {
		t.Fatalf("compose exact publication executor: %v", err)
	}
	if plan == nil || plan.Executor == nil || plan.WorkDir != repo.WorkingPath || plan.Cleanup == nil {
		t.Fatalf("incomplete publication executor plan: %#v", plan)
	}
	if loaded == nil || loaded.Config == nil || loaded.Config.Commands.Lint != "trusted-lint" ||
		loaded.Config.TrustedConfigSHA != parsed.Request.Candidate.BaseSHA {
		t.Fatalf("executor did not use exact trusted config: %#v", loaded)
	}
	plan.Cleanup()
	if !createdAgent.closed.Load() {
		t.Fatal("publication executor cleanup did not close its agent")
	}
}

func TestPublicationCoreCompositionWithExplicitUnconfinedTestBoundaryRegistersHandlers(t *testing.T) {
	shortRoot, err := os.MkdirTemp("/tmp", "nm-pub-handlers-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(shortRoot) })
	p := paths.WithRoot(shortRoot)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	runs := NewRunManager(database, p, func() []pipeline.Step { return runtimeSteps() })
	composition, err := newPublicationComposition(publicationCompositionOptions{
		Paths: p, DB: database, Runs: runs, GlobalConfig: config.DefaultGlobalConfig(), Identity: validCompositionIdentity(),
		TestOnlyUnconfinedDefenseBoundary: unitTestOnlyUnconfinedPublicationDefenseBoundary{},
		Candidate:                         compositionCandidatePort{}, Push: &runtimePushPort{}, PR: &runtimePRPort{}, CI: runtimeCIPort{},
	})
	if err != nil {
		t.Fatal(err)
	}
	server := ipc.NewServer()
	registerHandlers(server, runs, database, func() {})
	composition.registerHandlers(server, func(context.Context) error { return nil })
	if err := server.Listen(p.Socket()); err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.ServeReady() }()
	defer func() {
		server.Close()
		<-serveDone
	}()
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var health ipc.HealthResult
	if err := client.Call(ipc.MethodHealth, &ipc.HealthParams{}, &health); err != nil || health.Status != "ok" {
		t.Fatalf("generic handler missing: health=%#v err=%v", health, err)
	}
	want := publicationIPCIdentity(composition.identity)
	var handshake ipc.PublicationHandshakeResult
	if err := client.Call(ipc.MethodPublicationHandshake, &ipc.PublicationHandshakeParams{Identity: want}, &handshake); err != nil {
		t.Fatalf("publication handler missing: %v", err)
	}
	if handshake.Identity != want {
		t.Fatalf("handshake identity=%#v want=%#v", handshake.Identity, want)
	}
	foreign := want
	foreign.BuildSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if err := client.Call(ipc.MethodPublicationHandshake, &ipc.PublicationHandshakeParams{Identity: foreign}, &handshake); err == nil {
		t.Fatal("handler accepted a foreign daemon/CLI identity")
	}
}

func TestRunWithOptionsKeepsOrdinaryDaemonAvailableWhenConfinementIsUnavailable(t *testing.T) {
	p, database := startTestDaemonWithSteps(t, func() []pipeline.Step { return runtimeSteps() })
	binding, err := currentDaemonPublicationIdentity()
	if err != nil {
		t.Fatal(err)
	}
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	want := publicationIPCIdentity(binding)

	var health ipc.HealthResult
	if err := client.Call(ipc.MethodHealth, &ipc.HealthParams{}, &health); err != nil || health.Status != "ok" {
		t.Fatalf("ordinary daemon health unavailable: health=%#v err=%v", health, err)
	}
	var ordinaryRuns ipc.GetRunsResult
	if err := client.Call(ipc.MethodGetRuns, &ipc.GetRunsParams{RepoID: "ordinary-repo"}, &ordinaryRuns); err != nil {
		t.Fatalf("ordinary daemon handler unavailable: %v", err)
	}

	request, parsed := publicationRPCRequest(t, want)
	for _, test := range []struct {
		name   string
		method string
		params any
	}{
		{name: "handshake", method: ipc.MethodPublicationHandshake, params: &ipc.PublicationHandshakeParams{Identity: want}},
		{name: "start", method: ipc.MethodPublicationStart, params: &ipc.PublicationStartParams{Request: request}},
		{name: "authorize", method: ipc.MethodPublicationAuthorize, params: &ipc.PublicationAuthorizeParams{Authorization: publicationRPCAuthorization(parsed.PublicationID)}},
	} {
		if err := client.Call(test.method, test.params, &json.RawMessage{}); err == nil || !strings.Contains(err.Error(), "confinement_unavailable") {
			t.Errorf("publication %s without confinement error = %v, want confinement_unavailable", test.name, err)
		}
	}

	admitted, err := database.GetPublication(parsed.PublicationID)
	if err != nil {
		t.Fatal(err)
	}
	if admitted != nil {
		t.Fatalf("unconfined publication profile admitted durable state: %#v", admitted)
	}
	for _, kind := range []db.PublicationEffectKind{db.PublicationEffectPush, db.PublicationEffectPR, db.PublicationEffectCI} {
		effect, err := database.GetPublicationEffect(parsed.PublicationID, kind)
		if err != nil {
			t.Fatal(err)
		}
		if effect != nil {
			t.Fatalf("unconfined publication profile persisted %s effect: %#v", kind, effect)
		}
	}
	if _, err := os.Lstat(p.PublicationCandidatesDir()); !os.IsNotExist(err) {
		t.Fatalf("unconfined publication profile constructed candidate root: %v", err)
	}
	agentOutput, err := os.ReadFile(p.ManagedServerLog())
	if err != nil {
		t.Fatal(err)
	}
	if len(bytes.TrimSpace(agentOutput)) != 0 {
		t.Fatalf("unconfined publication profile launched an agent/command: %q", agentOutput)
	}
}

func TestRunWithOptionsKeepsOrdinaryDaemonAvailableWhenPublisherIdentityIsUnavailable(t *testing.T) {
	originalCommit := buildinfo.Commit
	buildinfo.Commit = "unknown"
	t.Cleanup(func() { buildinfo.Commit = originalCommit })

	p, database := startTestDaemonWithSteps(t, func() []pipeline.Step { return runtimeSteps() })
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	var health ipc.HealthResult
	if err := client.Call(ipc.MethodHealth, &ipc.HealthParams{}, &health); err != nil || health.Status != "ok" {
		t.Fatalf("ordinary daemon health unavailable: health=%#v err=%v", health, err)
	}
	var runs ipc.GetRunsResult
	if err := client.Call(ipc.MethodGetRuns, &ipc.GetRunsParams{RepoID: "ordinary-repo"}, &runs); err != nil {
		t.Fatalf("ordinary daemon handler unavailable: %v", err)
	}

	foreignIdentity := publicationRPCIdentity()
	if err := client.Call(ipc.MethodPublicationHandshake, &ipc.PublicationHandshakeParams{Identity: foreignIdentity}, &ipc.PublicationHandshakeResult{}); err == nil {
		t.Fatal("publication handshake succeeded without a provable daemon publisher identity")
	}
	request, parsed := publicationRPCRequest(t, foreignIdentity)
	if err := client.Call(ipc.MethodPublicationStart, &ipc.PublicationStartParams{Request: request}, &json.RawMessage{}); err == nil {
		t.Fatal("publication start succeeded without a provable daemon publisher identity")
	}
	if err := client.Call(ipc.MethodPublicationAuthorize, &ipc.PublicationAuthorizeParams{Authorization: publicationRPCAuthorization(parsed.PublicationID)}, &json.RawMessage{}); err == nil {
		t.Fatal("publication authorization succeeded without a provable daemon publisher identity")
	}

	admitted, err := database.GetPublication(parsed.PublicationID)
	if err != nil {
		t.Fatal(err)
	}
	if admitted != nil {
		t.Fatalf("unavailable publication profile admitted durable state: %#v", admitted)
	}
	for _, kind := range []db.PublicationEffectKind{db.PublicationEffectPush, db.PublicationEffectPR, db.PublicationEffectCI} {
		effect, err := database.GetPublicationEffect(parsed.PublicationID, kind)
		if err != nil {
			t.Fatal(err)
		}
		if effect != nil {
			t.Fatalf("unavailable publication profile persisted %s effect: %#v", kind, effect)
		}
	}
}

func validCompositionIdentity() publication.PublisherBinding {
	return publication.PublisherBinding{
		ExecutablePath: "/opt/pinned/no-mistakes", ExecutableSHA256: "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee",
		BuildSHA: "ffffffffffffffffffffffffffffffffffffffff", Protocol: publication.ProtocolV1,
	}
}
