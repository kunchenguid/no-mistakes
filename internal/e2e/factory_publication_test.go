//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/publication"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const factoryPublicationE2EAPIEnv = "NM_E2E_FACTORY_PUBLICATION_API"

// TestFactoryPublicationOfflineUnconfinedCoreJourney verifies composition,
// ordering, and exact-effect contracts through the tagged test seam. It is not
// evidence of a production confinement boundary.
func TestFactoryPublicationOfflineUnconfinedCoreJourney(t *testing.T) {
	stub := newFactoryPublicationGitHubStub(t)
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: factoryPublicationScenario(t)})
	h.NMBin = buildFactoryPublicationBinary(t)
	h.daemonOwn.NMBin = h.NMBin
	t.Setenv(factoryPublicationE2EAPIEnv, stub.server.URL)

	remoteIdentity := "https://github.com/example/project.git"
	pushLog := filepath.Join(t.TempDir(), "pushes.log")
	installFactoryPublicationGitWrapper(t, h, remoteIdentity)
	installFactoryPublicationReceiveLog(t, h.UpstreamDir, pushLog)

	if out, err := h.Run("init"); err != nil {
		t.Fatalf("initialize real no-mistakes daemon: %v\n%s", err, out)
	}
	database, err := db.Open(paths.WithRoot(h.NMHome).DB())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.UpdateRepoMetadata(h.repoID(), remoteIdentity, "main"); err != nil {
		database.Close()
		t.Fatalf("bind registered publication route: %v", err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	baseSHA := h.WorktreeRefSHA("refs/heads/main")
	branch := "factory/e2e"
	contractPath := ".agent/work-contract.json"
	contractRaw := []byte("{\"goal\":\"offline product publication\",\"dod\":[\"exact H\",\"all CI pass\"]}\n")
	h.CommitChange(branch, contractPath, string(contractRaw), "add factory work contract")
	headSHA := h.CommitChange(branch, "factory-feature.txt", "factory publication e2e\n", "add factory publication candidate")
	treeSHA := h.WorktreeRefSHA(headSHA + "^{tree}")
	stub.setHead(headSHA)

	publisher := factoryPublicationPublisherBinding(t, h.NMBin)
	request := publication.Request{
		Protocol: publication.ProtocolV1,
		Factory: publication.FactoryBinding{
			RunID:                "factory-e2e-offline-product",
			TerminalT10Sequence:  10,
			RunStatePrefixSHA256: sha256String("factory-e2e-run-state"),
			PlanBindingSHA256:    sha256String("factory-e2e-plan"),
		},
		WorkContract: publication.WorkContractBinding{Path: contractPath, SHA256: sha256Bytes(contractRaw)},
		BuildIntent: publication.BuildIntentProjection{
			Summary:            "publish the exact offline E2E candidate",
			AcceptanceCriteria: []string{"all nine steps pass once", "push and PR require exact single-use GO", "CI is non-empty all-pass at H"},
		},
		Candidate: publication.CandidateBinding{
			RepositoryID: h.repoID(), HeadRef: "refs/heads/" + branch, BaseRef: "refs/heads/main",
			BaseSHA: baseSHA, CommitSHA: headSHA, TreeSHA: treeSHA,
		},
		Publisher: publisher,
		Scopes: publication.PublicationScopes{
			Push: publication.PushScope{Mode: publication.PushModeExactCommit, RemoteIdentity: remoteIdentity, DestinationRef: "refs/heads/" + branch},
			PR:   publication.PRScope{Mode: publication.PRModeCreateOrUpdateExactHead, BaseRef: "refs/heads/main", HeadRef: "refs/heads/" + branch},
			CI:   publication.CIScope{Mode: publication.CIModeObserveExactHead},
		},
	}
	requestPath := writeFactoryPublicationJSON(t, request)
	parsed, err := publication.ParseRequest(mustReadFile(t, requestPath))
	if err != nil {
		t.Fatalf("canonical E2E request: %v", err)
	}
	queryPath := writeFactoryPublicationJSON(t, publication.StatusQuery{Protocol: publication.ProtocolV1, PublicationID: parsed.PublicationID})

	sourceBefore := captureFactoryPublicationSource(t, h)
	start, _, stderr, startErr := runFactoryPublicationCLI(t, h, "start", requestPath)
	if startErr == nil || start.Status != publication.StatusChecking {
		t.Fatalf("publication start = status %s err %v stderr %q, want CHECKING/nonzero", start.Status, startErr, stderr)
	}

	pushGate := waitForFactoryPublicationStatus(t, h, queryPath, publication.StatusReadyForPush, 90*time.Second)
	if pushGate.Challenge == nil || pushGate.Challenge.Kind != publication.EffectPush {
		t.Fatalf("Push gate has no exact challenge: %#v", pushGate)
	}
	assertFactoryPublicationRemoteRef(t, h.UpstreamDir, request.Scopes.Push.DestinationRef, "")
	if got := factoryPublicationPushes(t, pushLog); len(got) != 0 {
		t.Fatalf("Push effects before exact GO = %v, want zero", got)
	}
	if got := stub.prCreateCount(); got != 0 {
		t.Fatalf("PR effects before Push GO = %d, want zero", got)
	}

	pushAuthorization := factoryPublicationAuthorization(*pushGate.Challenge)
	pushAuthorizationPath := writeFactoryPublicationJSON(t, pushAuthorization)
	if result, _, stderr, err := runFactoryPublicationCLI(t, h, "authorize", pushAuthorizationPath); err == nil {
		t.Fatalf("Push authorization unexpectedly returned success status %s stderr %q", result.Status, stderr)
	}

	prGate := waitForFactoryPublicationStatus(t, h, queryPath, publication.StatusReadyForPR, 30*time.Second)
	if prGate.Challenge == nil || prGate.Challenge.Kind != publication.EffectPR ||
		prGate.Challenge.PreparedDraft == "" || sha256Bytes([]byte(prGate.Challenge.PreparedDraft)) != prGate.Challenge.DraftSHA256 {
		t.Fatalf("PR gate has no exact inspectable draft challenge: %#v", prGate)
	}
	assertFactoryPublicationRemoteRef(t, h.UpstreamDir, request.Scopes.Push.DestinationRef, headSHA)
	pushes := factoryPublicationPushes(t, pushLog)
	if len(pushes) != 1 || pushes[0].newSHA != headSHA || pushes[0].ref != request.Scopes.Push.DestinationRef {
		t.Fatalf("exact Push effects = %#v, want one Push of H to destination", pushes)
	}
	if got := stub.prCreateCount(); got != 0 {
		t.Fatalf("PR effects before second exact GO = %d, want zero", got)
	}
	if _, _, _, err := runFactoryPublicationCLI(t, h, "authorize", pushAuthorizationPath); err == nil {
		t.Fatal("consumed Push GO was reusable")
	}

	prAuthorization := factoryPublicationAuthorization(*prGate.Challenge)
	prAuthorizationPath := writeFactoryPublicationJSON(t, prAuthorization)
	if result, _, stderr, err := runFactoryPublicationCLI(t, h, "authorize", prAuthorizationPath); err == nil {
		t.Fatalf("PR authorization unexpectedly returned success status %s stderr %q", result.Status, stderr)
	}

	stub.waitForPendingCI(t, 30*time.Second)
	observing, _, stderr, observingErr := runFactoryPublicationCLI(t, h, "status", queryPath)
	if observingErr == nil || observing.Status != publication.StatusCIObserving {
		t.Fatalf("pending CI status = %s err %v stderr %q, want CI_OBSERVING/nonzero", observing.Status, observingErr, stderr)
	}
	if got := stub.prCreateCount(); got != 1 {
		t.Fatalf("authorized PR effects = %d, want exactly one", got)
	}
	created := stub.createdPR()
	if created.Head != branch || created.Base != "main" || created.Body != prGate.Challenge.PreparedDraft {
		t.Fatalf("created PR is not exact H-bound draft: %#v", created)
	}
	if got := factoryPublicationPushes(t, pushLog); len(got) != 1 {
		t.Fatalf("Push replayed while CI pending: %#v", got)
	}

	stub.passCI()
	ready := waitForFactoryPublicationStatus(t, h, queryPath, publication.StatusReady, 30*time.Second)
	if ready.HeadSHA != headSHA {
		t.Fatalf("READY head = %s, want H %s", ready.HeadSHA, headSHA)
	}
	if _, _, _, err := runFactoryPublicationCLI(t, h, "authorize", prAuthorizationPath); err == nil {
		t.Fatal("consumed PR GO was reusable")
	}

	assertFactoryPublicationDatabase(t, h, ready.RunID, parsed.PublicationID)
	if after := captureFactoryPublicationSource(t, h); !reflect.DeepEqual(after, sourceBefore) {
		t.Fatalf("publication changed registered source repo:\nbefore %#v\nafter  %#v", sourceBefore, after)
	}
	if got := factoryPublicationPushes(t, pushLog); len(got) != 1 {
		t.Fatalf("final Push effects = %#v, want exactly one", got)
	}
	if got := stub.prCreateCount(); got != 1 {
		t.Fatalf("final PR effects = %d, want exactly one", got)
	}
	if _, err := os.Stat(paths.WithRoot(h.NMHome).TelemetryGateFile()); !os.IsNotExist(err) {
		t.Fatalf("publication machine path wrote telemetry gate state: %v", err)
	}
}

func factoryPublicationScenario(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "factory-publication-scenario.yaml")
	content := `actions:
  - match: "Review the code changes and return structured findings"
    text: "review clean"
    structured:
      findings: []
      summary: "review clean"
      risk_level: low
      risk_rationale: "exact candidate is clean"
  - match: "Inspect the exact candidate and run only the smallest existing tests"
    text: "targeted validation passed"
    structured:
      findings: []
      summary: "targeted validation passed"
      tested:
        - "offline product acceptance check"
      testing_summary: "offline product acceptance check passed"
      artifacts: []
  - match: "Inspect the change and identify documentation facts it made stale"
    text: "documentation and lint clean"
    structured:
      findings: []
      summary: "documentation and lint clean"
  - text: "no issues found"
    structured:
      findings: []
      summary: "no issues found"
      risk_level: low
      risk_rationale: "no risks detected"
      tested:
        - "offline product acceptance check"
      testing_summary: "offline product acceptance check passed"
      artifacts: []
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

type factoryPublicationPR struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	Base  string `json:"base"`
	Head  string `json:"head"`
	Draft bool   `json:"draft"`
}

type factoryPublicationGitHubStub struct {
	server *httptest.Server
	mu     sync.Mutex
	head   string
	pr     factoryPublicationPR
	posts  int
	checks int
	pass   bool
}

func newFactoryPublicationGitHubStub(t *testing.T) *factoryPublicationGitHubStub {
	t.Helper()
	stub := &factoryPublicationGitHubStub{}
	stub.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		stub.serve(t, w, request)
	}))
	t.Cleanup(stub.server.Close)
	return stub
}

func (s *factoryPublicationGitHubStub) serve(t *testing.T, w http.ResponseWriter, request *http.Request) {
	body, err := io.ReadAll(request.Body)
	if err != nil {
		t.Errorf("read fake GitHub request: %v", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if request.Header.Get("Authorization") != "" {
		t.Errorf("offline GitHub stub received an authorization token")
	}
	switch {
	case request.Method == http.MethodPost && request.URL.Path == "/repos/example/project/pulls":
		s.posts++
		if err := json.Unmarshal(body, &s.pr); err != nil {
			t.Errorf("decode fake PR payload: %v", err)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"number":17}`))
	case request.Method == http.MethodGet && request.URL.Path == "/repos/example/project/pulls":
		if s.posts == 0 {
			_, _ = w.Write([]byte("[]"))
			return
		}
		_ = json.NewEncoder(w).Encode([]any{map[string]any{
			"number": 17, "body": s.pr.Body,
			"base": map[string]any{"ref": s.pr.Base, "repo": map[string]any{"full_name": "example/project"}},
			"head": map[string]any{"ref": s.pr.Head, "sha": s.head, "repo": map[string]any{"full_name": "example/project"}},
		}})
	case request.Method == http.MethodGet && request.URL.Path == "/repos/example/project/commits/"+s.head+"/check-runs":
		s.checks++
		status, conclusion := "in_progress", ""
		if s.pass {
			status, conclusion = "completed", "success"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"total_count": 1,
			"check_runs":  []any{map[string]any{"name": "offline-ci", "head_sha": s.head, "status": status, "conclusion": conclusion}},
		})
	case request.Method == http.MethodGet && request.URL.Path == "/repos/example/project/commits/"+s.head+"/status":
		_ = json.NewEncoder(w).Encode(map[string]any{"sha": s.head, "total_count": 0, "statuses": []any{}})
	default:
		t.Errorf("unexpected fake GitHub request: %s %s", request.Method, request.URL.String())
		http.Error(w, `{"message":"unexpected"}`, http.StatusNotFound)
	}
}

func (s *factoryPublicationGitHubStub) setHead(head string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.head = head
}

func (s *factoryPublicationGitHubStub) prCreateCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.posts
}

func (s *factoryPublicationGitHubStub) createdPR() factoryPublicationPR {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pr
}

func (s *factoryPublicationGitHubStub) passCI() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pass = true
}

func (s *factoryPublicationGitHubStub) waitForPendingCI(t *testing.T, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		checks, pass := s.checks, s.pass
		s.mu.Unlock()
		if checks > 0 && !pass {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("publication never observed pending exact-H CI")
}

func buildFactoryPublicationBinary(t *testing.T) string {
	t.Helper()
	root, err := findRepoRoot()
	if err != nil {
		t.Fatal(err)
	}
	headCommand := exec.Command("git", "rev-parse", "HEAD")
	headCommand.Dir = root
	headRaw, err := headCommand.Output()
	if err != nil {
		t.Fatalf("resolve build SHA: %v", err)
	}
	head := strings.TrimSpace(string(headRaw))
	if len(head) != 40 {
		t.Fatalf("build SHA = %q, want full commit", head)
	}
	output := filepath.Join(t.TempDir(), executableName("no-mistakes"))
	ldflags := "-X github.com/kunchenguid/no-mistakes/internal/buildinfo.Commit=" + head
	command := exec.Command("go", "build", "-tags", "factorypublicatione2e", "-ldflags", ldflags, "-o", output, "./cmd/no-mistakes")
	command.Dir = root
	if built, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build factory-publication E2E binary: %v\n%s", err, built)
	}
	return output
}

func factoryPublicationPublisherBinding(t *testing.T, executable string) publication.PublisherBinding {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(executable)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(resolved)
	if err != nil {
		t.Fatal(err)
	}
	root, err := findRepoRoot()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command("git", "rev-parse", "HEAD")
	command.Dir = root
	head, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	return publication.PublisherBinding{
		ExecutablePath: resolved, ExecutableSHA256: sha256Bytes(raw),
		BuildSHA: strings.TrimSpace(string(head)), Protocol: publication.ProtocolV1,
	}
}

func installFactoryPublicationGitWrapper(t *testing.T, h *Harness, remoteIdentity string) {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	wrapper := filepath.Join(h.BinDir, executableName("git"))
	script := "#!/bin/sh\n" +
		"real=" + shellQuote(realGit) + "\n" +
		"identity=" + shellQuote(remoteIdentity) + "\n" +
		"remote=" + shellQuote(h.UpstreamDir) + "\n" +
		`if [ "$1" = "ls-remote" ] && [ "$2" = "--get-url" ] && [ "$3" = "$identity" ]; then printf '%s\n' "$identity"; exit 0; fi` + "\n" +
		`if [ "$1" = "ls-remote" ] && [ "$2" = "--refs" ] && [ "$3" = "$identity" ]; then exec "$real" "$1" "$2" "$remote" "$4"; fi` + "\n" +
		`if [ "$1" = "push" ] && [ "$2" = "--no-verify" ] && [ "$3" = "$identity" ]; then exec "$real" "$1" "$2" "$remote" "$4"; fi` + "\n" +
		`exec "$real" "$@"` + "\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

func installFactoryPublicationReceiveLog(t *testing.T, bareRemote, logPath string) {
	t.Helper()
	hook := filepath.Join(bareRemote, "hooks", "pre-receive")
	script := "#!/bin/sh\nwhile read old new ref; do printf '%s %s %s\\n' \"$old\" \"$new\" \"$ref\" >> " + shellQuote(logPath) + "; done\n"
	if err := os.WriteFile(hook, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

type factoryPublicationPush struct{ oldSHA, newSHA, ref string }

func factoryPublicationPushes(t *testing.T, path string) []factoryPublicationPush {
	t.Helper()
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var pushes []factoryPublicationPush
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 {
			t.Fatalf("malformed pre-receive record %q", line)
		}
		pushes = append(pushes, factoryPublicationPush{oldSHA: fields[0], newSHA: fields[1], ref: fields[2]})
	}
	return pushes
}

func assertFactoryPublicationRemoteRef(t *testing.T, bareRemote, ref, want string) {
	t.Helper()
	command := exec.Command("git", "--git-dir", bareRemote, "rev-parse", "--verify", ref)
	raw, err := command.Output()
	if want == "" {
		if err == nil {
			t.Fatalf("remote ref %s exists before authorization at %s", ref, strings.TrimSpace(string(raw)))
		}
		return
	}
	if err != nil || strings.TrimSpace(string(raw)) != want {
		t.Fatalf("remote ref %s = %q, %v; want %s", ref, raw, err, want)
	}
}

func factoryPublicationAuthorization(challenge publication.EffectChallenge) publication.AuthorizationEnvelope {
	return publication.AuthorizationEnvelope{
		Protocol: publication.ProtocolV1, Decision: publication.DecisionGo,
		PublicationID: challenge.PublicationID, Kind: challenge.Kind, Attempt: challenge.Attempt,
		CommitSHA: challenge.CommitSHA, RemoteIdentity: challenge.RemoteIdentity,
		DestinationRef: challenge.DestinationRef, BaseRef: challenge.BaseRef, HeadRef: challenge.HeadRef,
		DraftSHA256: challenge.DraftSHA256, EffectDigest: challenge.EffectDigest, DecisionDigest: challenge.DecisionDigest,
	}
}

func runFactoryPublicationCLI(t *testing.T, h *Harness, verb, requestPath string) (publication.Result, string, string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, h.NMBin, "publication", verb, "--request", requestPath)
	command.Dir = h.WorkDir
	command.Env = os.Environ()
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	h.syncDaemonOwnership()
	if ctx.Err() != nil {
		t.Fatalf("publication %s timed out: %v\n%s", verb, ctx.Err(), stderr.String())
	}
	var result publication.Result
	if raw := bytes.TrimSpace(stdout.Bytes()); len(raw) > 0 {
		parsed, parseErr := publication.ParseResult(raw)
		if parseErr != nil {
			t.Fatalf("publication %s returned invalid stdout %q: %v; stderr=%q", verb, raw, parseErr, stderr.String())
		}
		result = parsed
	}
	return result, stdout.String(), stderr.String(), err
}

func waitForFactoryPublicationStatus(t *testing.T, h *Harness, queryPath string, want publication.ResultStatus, timeout time.Duration) publication.Result {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last publication.Result
	var lastErr error
	for time.Now().Before(deadline) {
		result, _, stderr, err := runFactoryPublicationCLI(t, h, "status", queryPath)
		last, lastErr = result, err
		if result.Status == want {
			if want == publication.StatusReady && err != nil {
				t.Fatalf("READY returned nonzero: %v stderr=%q", err, stderr)
			}
			if want != publication.StatusReady && err == nil {
				t.Fatalf("nonterminal %s returned exit zero", want)
			}
			return result
		}
		switch result.Status {
		case publication.StatusFailed, publication.StatusDrift, publication.StatusDenied, publication.StatusEffectUnknown:
			h.dumpDebugState()
			t.Fatalf("publication became terminal %s while waiting for %s; stderr=%q", result.Status, want, stderr)
		}
		time.Sleep(50 * time.Millisecond)
	}
	h.dumpDebugState()
	t.Fatalf("publication did not reach %s; last=%#v err=%v", want, last, lastErr)
	return publication.Result{}
}

type factoryPublicationSourceState struct {
	Head, Status, Refs, Config string
}

func captureFactoryPublicationSource(t *testing.T, h *Harness) factoryPublicationSourceState {
	t.Helper()
	git := func(args ...string) string {
		raw, err := h.runGit(context.Background(), h.WorkDir, args...)
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, raw)
		}
		return string(raw)
	}
	return factoryPublicationSourceState{
		Head: git("rev-parse", "HEAD"), Status: git("status", "--porcelain=v1", "--untracked-files=all"),
		Refs: git("for-each-ref", "--format=%(refname) %(objectname)"), Config: git("config", "--local", "--null", "--list"),
	}
}

func assertFactoryPublicationDatabase(t *testing.T, h *Harness, runID, publicationID string) {
	t.Helper()
	database, err := db.Open(paths.WithRoot(h.NMHome).DB())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	run, err := database.GetRun(runID)
	if err != nil || run == nil || run.Status != types.RunCompleted || run.Kind != types.RunKindFactoryPublicationV1 {
		t.Fatalf("final publication run = %#v, %v", run, err)
	}
	steps, err := database.GetStepsByRun(runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != len(types.AllSteps()) {
		t.Fatalf("publication steps = %d, want %d", len(steps), len(types.AllSteps()))
	}
	for index, step := range steps {
		if step.StepName != types.AllSteps()[index] || step.Status != types.StepStatusCompleted || step.ExitCode == nil || *step.ExitCode != 0 ||
			step.CIFixAttempts != 0 || step.Error != nil {
			t.Errorf("step %d is not one clean execution: %#v", index+1, step)
		}
		rounds, err := database.GetRoundsByStep(step.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(rounds) != 1 || rounds[0].Round != 1 || rounds[0].Trigger != "initial" || rounds[0].IsFixRound() {
			t.Errorf("%s rounds = %#v, want one initial non-fix round", step.StepName, rounds)
		}
	}
	for _, kind := range []db.PublicationEffectKind{db.PublicationEffectPush, db.PublicationEffectPR, db.PublicationEffectCI} {
		effect, err := database.GetPublicationEffect(publicationID, kind)
		if err != nil || effect == nil || effect.State != db.PublicationEffectObserved || effect.EffectStartedAt == nil || effect.ObservedAt == nil {
			t.Errorf("%s effect = %#v, %v; want one observed effect", kind, effect, err)
		}
		if kind != db.PublicationEffectCI && (effect.DecisionDigest == nil || effect.DecisionConsumedAt == nil) {
			t.Errorf("%s effect did not consume one exact Owner decision: %#v", kind, effect)
		}
	}
}

func writeFactoryPublicationJSON(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "request.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func sha256Bytes(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func sha256String(value string) string { return sha256Bytes([]byte(value)) }
