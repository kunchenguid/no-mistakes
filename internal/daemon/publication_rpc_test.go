package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/gatecontext"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/publication"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type publicationRPCFake struct {
	mu sync.Mutex

	startCalls     []publication.ParsedRequest
	authorizeCalls []publication.Authorization
	statusCalls    []string
	created        map[string]publication.Result
	createdCount   int
	forcedResult   *publication.Result
}

func newPublicationRPCFake() *publicationRPCFake {
	return &publicationRPCFake{created: make(map[string]publication.Result)}
}

func (f *publicationRPCFake) Start(_ context.Context, request publication.ParsedRequest) (publication.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.startCalls = append(f.startCalls, request)
	if f.forcedResult != nil {
		return *f.forcedResult, nil
	}
	if result, ok := f.created[request.PublicationID]; ok {
		return result, nil
	}
	result := validPublicationRPCResult(request.PublicationID, publication.StatusChecking)
	f.created[request.PublicationID] = result
	f.createdCount++
	return result, nil
}

func (f *publicationRPCFake) Authorize(_ context.Context, authorization publication.Authorization) (publication.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.authorizeCalls = append(f.authorizeCalls, authorization)
	if f.forcedResult != nil {
		return *f.forcedResult, nil
	}
	return validPublicationRPCResult(authorization.PublicationID, publication.StatusReadyForPR), nil
}

func (f *publicationRPCFake) Status(_ context.Context, publicationID string) (publication.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statusCalls = append(f.statusCalls, publicationID)
	if f.forcedResult != nil {
		return *f.forcedResult, nil
	}
	return validPublicationRPCResult(publicationID, publication.StatusReady), nil
}

func (f *publicationRPCFake) snapshot() (starts []publication.ParsedRequest, authorizations []publication.Authorization, statuses []string, created int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]publication.ParsedRequest(nil), f.startCalls...), append([]publication.Authorization(nil), f.authorizeCalls...), append([]string(nil), f.statusCalls...), f.createdCount
}

func TestPublicationRPCHandshakeRequiresExactDaemonIdentity(t *testing.T) {
	identity := publicationRPCIdentity()
	client := startPublicationRPCServer(t, newPublicationRPCFake(), identity)
	defer client.Close()

	var result ipc.PublicationHandshakeResult
	if err := client.Call(ipc.MethodPublicationHandshake, &ipc.PublicationHandshakeParams{Identity: identity}, &result); err != nil {
		t.Fatalf("exact publication handshake: %v", err)
	}
	if result.Identity != identity {
		t.Fatalf("handshake identity = %+v, want %+v", result.Identity, identity)
	}

	mismatches := map[string]func(*ipc.PublicationIdentity){
		"path":     func(value *ipc.PublicationIdentity) { value.ExecutablePath = "/different/no-mistakes" },
		"raw hash": func(value *ipc.PublicationIdentity) { value.ExecutableSHA256 = strings.Repeat("0", 64) },
		"build":    func(value *ipc.PublicationIdentity) { value.BuildSHA = strings.Repeat("c", 40) },
		"protocol": func(value *ipc.PublicationIdentity) { value.Protocol = "factory-publication-v2" },
	}
	for name, mutate := range mismatches {
		t.Run(name, func(t *testing.T) {
			candidate := identity
			mutate(&candidate)
			if err := client.Call(ipc.MethodPublicationHandshake, &ipc.PublicationHandshakeParams{Identity: candidate}, &ipc.PublicationHandshakeResult{}); err == nil {
				t.Fatal("publication handshake accepted an incompatible CLI identity")
			}
		})
	}
}

func TestPublicationRPCParsesStrictPayloadsAndCallsOnlyInjectedService(t *testing.T) {
	identity := publicationRPCIdentity()
	service := newPublicationRPCFake()
	client := startPublicationRPCServer(t, service, identity)
	defer client.Close()

	request, parsed := publicationRPCRequest(t, identity)
	var startRaw json.RawMessage
	if err := client.Call(ipc.MethodPublicationStart, &ipc.PublicationStartParams{Request: request}, &startRaw); err != nil {
		t.Fatalf("publication start RPC: %v", err)
	}
	startResult, err := publication.ParseResult(startRaw)
	if err != nil {
		t.Fatalf("start result is not canonical: %v", err)
	}
	if startResult.PublicationID != parsed.PublicationID || startResult.Status != publication.StatusChecking {
		t.Fatalf("start result = %+v", startResult)
	}

	authorizationRaw := publicationRPCAuthorization(parsed.PublicationID)
	var authorizeRaw json.RawMessage
	if err := client.Call(ipc.MethodPublicationAuthorize, &ipc.PublicationAuthorizeParams{Authorization: authorizationRaw}, &authorizeRaw); err != nil {
		t.Fatalf("publication authorize RPC: %v", err)
	}
	if result, err := publication.ParseResult(authorizeRaw); err != nil || result.Status != publication.StatusReadyForPR {
		t.Fatalf("authorize result = %+v, error = %v", result, err)
	}

	queryRaw := publicationRPCStatusQuery(parsed.PublicationID)
	var statusRaw json.RawMessage
	if err := client.Call(ipc.MethodPublicationStatus, &ipc.PublicationStatusParams{Query: queryRaw}, &statusRaw); err != nil {
		t.Fatalf("publication status RPC: %v", err)
	}
	if result, err := publication.ParseResult(statusRaw); err != nil || result.Status != publication.StatusReady {
		t.Fatalf("status result = %+v, error = %v", result, err)
	}

	starts, authorizations, statuses, created := service.snapshot()
	if len(starts) != 1 || starts[0].PublicationID != parsed.PublicationID || string(starts[0].CanonicalBytes) != string(request) {
		t.Fatalf("start service calls = %#v", starts)
	}
	if len(authorizations) != 1 || authorizations[0].PublicationID != parsed.PublicationID || authorizations[0].Decision != publication.DecisionGo {
		t.Fatalf("authorize service calls = %#v", authorizations)
	}
	if len(statuses) != 1 || statuses[0] != parsed.PublicationID {
		t.Fatalf("status service calls = %#v", statuses)
	}
	if created != 1 {
		t.Fatalf("created publication count = %d, want 1", created)
	}

	// The server contains only publication handlers. Their success without a
	// RunManager, repository lookup, or AXI handler is the executable boundary
	// proving that admission never routes through init/attach/axi run.
	if err := client.Call(ipc.MethodGetRun, &ipc.GetRunParams{RunID: "ordinary"}, &ipc.GetRunResult{}); err == nil {
		t.Fatal("publication-only server unexpectedly exposed an ordinary AXI handler")
	}
}

func TestPublicationRPCRejectsInvalidPayloadsAndResultsFailClosed(t *testing.T) {
	identity := publicationRPCIdentity()
	service := newPublicationRPCFake()
	client := startPublicationRPCServer(t, service, identity)
	defer client.Close()

	request, parsed := publicationRPCRequest(t, identity)
	otherIdentity := identity
	otherIdentity.BuildSHA = strings.Repeat("c", 40)
	otherPublisherRequest, _ := publicationRPCRequest(t, otherIdentity)
	tests := []struct {
		name   string
		method string
		params any
	}{
		{
			name:   "start noncanonical",
			method: ipc.MethodPublicationStart,
			params: &ipc.PublicationStartParams{Request: duplicatePublicationProtocolKey(request)},
		},
		{
			name:   "start unknown outer field",
			method: ipc.MethodPublicationStart,
			params: map[string]any{"request": json.RawMessage(request), "attach": true},
		},
		{
			name:   "start bound to another publisher",
			method: ipc.MethodPublicationStart,
			params: &ipc.PublicationStartParams{Request: otherPublisherRequest},
		},
		{
			name:   "authorize noncanonical",
			method: ipc.MethodPublicationAuthorize,
			params: &ipc.PublicationAuthorizeParams{Authorization: duplicatePublicationProtocolKey(publicationRPCAuthorization(parsed.PublicationID))},
		},
		{
			name:   "status noncanonical",
			method: ipc.MethodPublicationStatus,
			params: &ipc.PublicationStatusParams{Query: duplicatePublicationProtocolKey(publicationRPCStatusQuery(parsed.PublicationID))},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := client.Call(test.method, test.params, &json.RawMessage{}); err == nil {
				t.Fatal("publication RPC accepted an open or non-canonical payload")
			}
		})
	}
	starts, authorizations, statuses, _ := service.snapshot()
	if len(starts) != 0 || len(authorizations) != 0 || len(statuses) != 0 {
		t.Fatalf("invalid payload reached publication service: starts=%d authorize=%d status=%d", len(starts), len(authorizations), len(statuses))
	}

	invalid := validPublicationRPCResult(parsed.PublicationID, publication.StatusReady)
	invalid.Protocol = "factory-publication-v2"
	service.forcedResult = &invalid
	if err := client.Call(ipc.MethodPublicationStatus, &ipc.PublicationStatusParams{Query: publicationRPCStatusQuery(parsed.PublicationID)}, &json.RawMessage{}); err == nil {
		t.Fatal("publication RPC returned a non-v1 service result")
	}
}

func TestPublicationRPCWithoutServiceIsUnavailableFailClosed(t *testing.T) {
	client := startPublicationRPCServer(t, nil, publicationRPCIdentity())
	defer client.Close()

	request, parsed := publicationRPCRequest(t, publicationRPCIdentity())
	tests := []struct {
		method string
		params any
	}{
		{method: ipc.MethodPublicationStart, params: &ipc.PublicationStartParams{Request: request}},
		{method: ipc.MethodPublicationAuthorize, params: &ipc.PublicationAuthorizeParams{Authorization: publicationRPCAuthorization(parsed.PublicationID)}},
		{method: ipc.MethodPublicationStatus, params: &ipc.PublicationStatusParams{Query: publicationRPCStatusQuery(parsed.PublicationID)}},
	}
	for _, test := range tests {
		if err := client.Call(test.method, test.params, &json.RawMessage{}); err == nil {
			t.Fatalf("%s succeeded without a publication service", test.method)
		}
	}
}

func TestPublicationRPCRefusesActiveAgentPeerBeforeStartOrAuthorize(t *testing.T) {
	root, err := os.MkdirTemp("", "dpub-guard")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	p := paths.WithRoot(root)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	repo, err := database.InsertRepo(t.TempDir(), "https://github.com/example/project.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, "feature", strings.Repeat("d", 40), strings.Repeat("c", 40))
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	step, err := database.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.StartStep(step.ID); err != nil {
		t.Fatal(err)
	}
	pid := os.Getpid()
	if err := database.SetStepAgentActivity(step.ID, "agent started", &pid); err != nil {
		t.Fatal(err)
	}

	service := newPublicationRPCFake()
	identity := publicationRPCIdentity()
	request, parsed := publicationRPCRequest(t, identity)
	server := ipc.NewServer()
	registerPublicationHandlers(server, service, identity, func(ctx context.Context) error {
		return refuseNestedGateCaller(ctx, database, p, false)
	})
	if err := server.Listen(p.Socket()); err != nil {
		t.Fatal(err)
	}
	errCh := make(chan error, 1)
	go func() { errCh <- server.ServeReady() }()
	t.Cleanup(func() {
		server.Close()
		if err := <-errCh; err != nil {
			t.Errorf("serve guarded publication RPC: %v", err)
		}
	})
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	var handshake ipc.PublicationHandshakeResult
	if err := client.Call(ipc.MethodPublicationHandshake, &ipc.PublicationHandshakeParams{Identity: identity}, &handshake); err != nil {
		t.Fatalf("read-only handshake was refused: %v", err)
	}
	for _, test := range []struct {
		method string
		params any
	}{
		{method: ipc.MethodPublicationStart, params: &ipc.PublicationStartParams{Request: request}},
		{method: ipc.MethodPublicationAuthorize, params: &ipc.PublicationAuthorizeParams{Authorization: publicationRPCAuthorization(parsed.PublicationID)}},
	} {
		if err := client.Call(test.method, test.params, &json.RawMessage{}); err == nil || !strings.Contains(err.Error(), gatecontext.ErrorCode) {
			t.Fatalf("%s error = %v, want authenticated nested-peer refusal", test.method, err)
		}
	}
	service.forcedResult = func() *publication.Result {
		result := validPublicationRPCResult(parsed.PublicationID, publication.StatusReady)
		return &result
	}()
	if err := client.Call(ipc.MethodPublicationStatus, &ipc.PublicationStatusParams{Query: publicationRPCStatusQuery(parsed.PublicationID)}, &json.RawMessage{}); err != nil {
		t.Fatalf("read-only publication status was refused: %v", err)
	}
	starts, authorizations, statuses, _ := service.snapshot()
	if len(starts) != 0 || len(authorizations) != 0 || len(statuses) != 1 {
		t.Fatalf("nested peer reached mutation service: starts=%d authorize=%d statuses=%d", len(starts), len(authorizations), len(statuses))
	}
}

func TestPublicationRPCConcurrentIdenticalStartKeepsOneIdempotentPublication(t *testing.T) {
	identity := publicationRPCIdentity()
	service := newPublicationRPCFake()
	p := startPublicationRPCServerPath(t, service, identity)
	request, parsed := publicationRPCRequest(t, identity)

	const callers = 16
	errors := make(chan error, callers)
	results := make(chan publication.Result, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			client, err := ipc.Dial(p.Socket())
			if err != nil {
				errors <- err
				return
			}
			defer client.Close()
			var raw json.RawMessage
			if err := client.Call(ipc.MethodPublicationStart, &ipc.PublicationStartParams{Request: request}, &raw); err != nil {
				errors <- err
				return
			}
			result, err := publication.ParseResult(raw)
			if err != nil {
				errors <- err
				return
			}
			results <- result
		}()
	}
	wg.Wait()
	close(errors)
	close(results)
	for err := range errors {
		t.Errorf("concurrent publication start: %v", err)
	}
	for result := range results {
		if result.PublicationID != parsed.PublicationID || result.RunID != "01ARZ3NDEKTSV4RRFFQ69G5FAV" {
			t.Errorf("concurrent publication result = %+v", result)
		}
	}
	starts, _, _, created := service.snapshot()
	if len(starts) != callers {
		t.Fatalf("service start calls = %d, want one per RPC (%d)", len(starts), callers)
	}
	if created != 1 {
		t.Fatalf("idempotent publication creations = %d, want 1", created)
	}
	for _, call := range starts {
		if call.PublicationID != parsed.PublicationID || string(call.CanonicalBytes) != string(request) {
			t.Fatalf("concurrent start lost exact request binding: %+v", call)
		}
	}
}

func startPublicationRPCServer(t *testing.T, service publicationRPCService, identity ipc.PublicationIdentity) *ipc.Client {
	t.Helper()
	p := startPublicationRPCServerPath(t, service, identity)
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatalf("dial publication RPC server: %v", err)
	}
	return client
}

func startPublicationRPCServerPath(t *testing.T, service publicationRPCService, identity ipc.PublicationIdentity) *paths.Paths {
	t.Helper()
	root, err := os.MkdirTemp("", "dpub")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	p := paths.WithRoot(root)
	server := ipc.NewServer()
	registerPublicationHandlers(server, service, identity, func(context.Context) error { return nil })
	if err := server.Listen(p.Socket()); err != nil {
		t.Fatalf("listen publication RPC: %v", err)
	}
	errCh := make(chan error, 1)
	go func() { errCh <- server.ServeReady() }()
	t.Cleanup(func() {
		server.Close()
		if err := <-errCh; err != nil {
			t.Errorf("serve publication RPC: %v", err)
		}
	})
	return p
}

func publicationRPCIdentity() ipc.PublicationIdentity {
	return ipc.PublicationIdentity{
		ExecutablePath:   "/opt/pinned/no-mistakes",
		ExecutableSHA256: strings.Repeat("a", 64),
		BuildSHA:         strings.Repeat("b", 40),
		Protocol:         publication.ProtocolV1,
	}
}

func publicationRPCRequest(t *testing.T, identity ipc.PublicationIdentity) ([]byte, publication.ParsedRequest) {
	t.Helper()
	request := publication.Request{
		Protocol: publication.ProtocolV1,
		Factory: publication.FactoryBinding{
			RunID:                "factory-run-rpc",
			TerminalT10Sequence:  10,
			RunStatePrefixSHA256: strings.Repeat("1", 64),
			PlanBindingSHA256:    strings.Repeat("2", 64),
		},
		WorkContract: publication.WorkContractBinding{Path: ".agent/work-contract.toml", SHA256: strings.Repeat("3", 64)},
		BuildIntent: publication.BuildIntentProjection{
			Summary:            "publish the exact candidate through the daemon RPC",
			AcceptanceCriteria: []string{"one idempotent publication is admitted"},
		},
		Candidate: publication.CandidateBinding{
			RepositoryID: "01ARZ3NDEKTSV4RRFFQ69G5FAV",
			HeadRef:      "refs/heads/codex/factory-publication-v1",
			BaseRef:      "refs/heads/main",
			BaseSHA:      strings.Repeat("c", 40),
			CommitSHA:    strings.Repeat("d", 40),
			TreeSHA:      strings.Repeat("e", 40),
		},
		Publisher: publication.PublisherBinding{
			ExecutablePath:   identity.ExecutablePath,
			ExecutableSHA256: identity.ExecutableSHA256,
			BuildSHA:         identity.BuildSHA,
			Protocol:         identity.Protocol,
		},
		Scopes: publication.PublicationScopes{
			Push: publication.PushScope{Mode: publication.PushModeExactCommit, RemoteIdentity: "github.com/example/project", DestinationRef: "refs/heads/codex/factory-publication-v1"},
			PR:   publication.PRScope{Mode: publication.PRModeCreateOrUpdateExactHead, BaseRef: "refs/heads/main", HeadRef: "refs/heads/codex/factory-publication-v1"},
			CI:   publication.CIScope{Mode: publication.CIModeObserveExactHead},
		},
	}
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := publication.ParseRequest(raw)
	if err != nil {
		t.Fatal(err)
	}
	return raw, parsed
}

func publicationRPCAuthorization(publicationID string) []byte {
	return []byte(`{"protocol":"factory-publication-v1","decision":"GO","publication_id":"` + publicationID + `","kind":"push","attempt":1,"commit_sha":"` + strings.Repeat("d", 40) + `","remote_identity":"github.com/example/project","destination_ref":"refs/heads/codex/factory-publication-v1","base_ref":"","head_ref":"refs/heads/codex/factory-publication-v1","draft_sha256":"","effect_digest":"` + strings.Repeat("e", 64) + `","decision_digest":"` + strings.Repeat("f", 64) + `"}`)
}

func publicationRPCStatusQuery(publicationID string) []byte {
	return []byte(`{"protocol":"factory-publication-v1","publication_id":"` + publicationID + `"}`)
}

func duplicatePublicationProtocolKey(raw []byte) []byte {
	return append([]byte(`{"protocol":"factory-publication-v1",`), raw[1:]...)
}

func validPublicationRPCResult(publicationID string, status publication.ResultStatus) publication.Result {
	result := publication.Result{
		Protocol:      publication.ProtocolV1,
		PublicationID: publicationID,
		RunID:         "01ARZ3NDEKTSV4RRFFQ69G5FAV",
		HeadSHA:       strings.Repeat("d", 40),
		Status:        status,
	}
	if status != publication.StatusReadyForPush && status != publication.StatusReadyForPR {
		return result
	}
	challenge := publication.EffectChallenge{
		PublicationID: publicationID, Attempt: 1, CommitSHA: result.HeadSHA,
		RemoteIdentity: "github.com/example/project",
		DestinationRef: "refs/heads/codex/factory-publication-v1",
		HeadRef:        "refs/heads/codex/factory-publication-v1",
		EffectDigest:   strings.Repeat("e", 64),
	}
	if status == publication.StatusReadyForPush {
		challenge.Kind = publication.EffectPush
	} else {
		challenge.Kind = publication.EffectPR
		challenge.BaseRef = "refs/heads/main"
		challenge.Marker = "<!-- no-mistakes-factory-publication-v1:" + publicationID + ":" + result.HeadSHA + " -->"
		challenge.PreparedDraft = "Inspectable draft\n\n" + challenge.Marker + "\n"
		digest := sha256.Sum256([]byte(challenge.PreparedDraft))
		challenge.DraftSHA256 = hex.EncodeToString(digest[:])
	}
	bound, err := publication.BindEffectChallengeDecisions(challenge)
	if err != nil {
		panic(err)
	}
	result.Challenge = &bound
	return result
}
