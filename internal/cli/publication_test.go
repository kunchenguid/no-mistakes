package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/buildinfo"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/publication"
	"github.com/kunchenguid/no-mistakes/internal/telemetry"
)

const publicationTestBuildSHA = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

type publicationCLIFixture struct {
	t        *testing.T
	paths    *paths.Paths
	server   *ipc.Server
	identity ipc.PublicationIdentity

	mu      sync.Mutex
	calls   []string
	results map[string]publication.Result
}

func newPublicationCLIFixture(t *testing.T) *publicationCLIFixture {
	t.Helper()
	nmHome := makeSocketSafeTempDir(t)
	t.Setenv("NM_HOME", nmHome)
	t.Setenv("NO_MISTAKES_TELEMETRY", "off")
	t.Setenv("NO_MISTAKES_NO_UPDATE_CHECK", "1")

	originalCommit := buildinfo.Commit
	buildinfo.Commit = publicationTestBuildSHA
	t.Cleanup(func() { buildinfo.Commit = originalCommit })

	executable := executablePath()
	raw, err := os.ReadFile(executable)
	if err != nil {
		t.Fatalf("read test executable: %v", err)
	}
	binaryDigest := sha256.Sum256(raw)
	identity := ipc.PublicationIdentity{
		ExecutablePath:   executable,
		ExecutableSHA256: hex.EncodeToString(binaryDigest[:]),
		BuildSHA:         publicationTestBuildSHA,
		Protocol:         publication.ProtocolV1,
	}

	p := paths.WithRoot(nmHome)
	server := ipc.NewServer()
	fixture := &publicationCLIFixture{
		t:        t,
		paths:    p,
		server:   server,
		identity: identity,
		results:  make(map[string]publication.Result),
	}
	server.Handle(ipc.MethodHealth, func(context.Context, json.RawMessage) (any, error) {
		fixture.record(ipc.MethodHealth)
		return &ipc.HealthResult{Status: "ok"}, nil
	})
	server.Handle(ipc.MethodGateContext, func(context.Context, json.RawMessage) (any, error) {
		fixture.record(ipc.MethodGateContext)
		return &ipc.GateContextResult{}, nil
	})
	server.Handle(ipc.MethodPublicationHandshake, func(_ context.Context, params json.RawMessage) (any, error) {
		fixture.record(ipc.MethodPublicationHandshake)
		var request ipc.PublicationHandshakeParams
		if err := json.Unmarshal(params, &request); err != nil {
			return nil, err
		}
		return &ipc.PublicationHandshakeResult{Identity: fixture.identity}, nil
	})
	for _, method := range []string{ipc.MethodPublicationStart, ipc.MethodPublicationAuthorize, ipc.MethodPublicationStatus} {
		method := method
		server.Handle(method, func(_ context.Context, params json.RawMessage) (any, error) {
			fixture.record(method)
			if len(params) == 0 {
				return nil, fmt.Errorf("missing publication params")
			}
			result, ok := fixture.results[method]
			if !ok {
				return nil, fmt.Errorf("test result not configured for %s", method)
			}
			return result, nil
		})
	}

	errCh := make(chan error, 1)
	if err := server.Listen(p.Socket()); err != nil {
		t.Fatalf("listen on publication test socket: %v", err)
	}
	go func() { errCh <- server.ServeReady() }()
	t.Cleanup(func() {
		server.Close()
		if err := <-errCh; err != nil {
			t.Errorf("publication test server: %v", err)
		}
	})
	return fixture
}

func TestCurrentPublicationIdentityUsesSharedPublisherBinding(t *testing.T) {
	originalCommit := buildinfo.Commit
	buildinfo.Commit = publicationTestBuildSHA
	t.Cleanup(func() { buildinfo.Commit = originalCommit })

	shared, err := publication.CurrentPublisherBinding(executablePath())
	if err != nil {
		t.Fatal(err)
	}
	got, err := currentPublicationIdentity()
	if err != nil {
		t.Fatal(err)
	}
	want := ipc.PublicationIdentity{
		ExecutablePath: shared.ExecutablePath, ExecutableSHA256: shared.ExecutableSHA256,
		BuildSHA: shared.BuildSHA, Protocol: shared.Protocol,
	}
	if got != want {
		t.Fatalf("CLI identity = %#v, shared helper = %#v", got, want)
	}
}

func (f *publicationCLIFixture) record(method string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, method)
}

func (f *publicationCLIFixture) callsFor(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	count := 0
	for _, call := range f.calls {
		if call == method {
			count++
		}
	}
	return count
}

func (f *publicationCLIFixture) assertOnlyPublicationCalls(t *testing.T) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	allowed := map[string]bool{
		ipc.MethodHealth:               true,
		ipc.MethodGateContext:          true,
		ipc.MethodPublicationHandshake: true,
		ipc.MethodPublicationStart:     true,
		ipc.MethodPublicationAuthorize: true,
		ipc.MethodPublicationStatus:    true,
	}
	for _, call := range f.calls {
		if !allowed[call] {
			t.Fatalf("publication command invoked non-publication method %q; calls = %#v", call, f.calls)
		}
	}
}

func (f *publicationCLIFixture) readyResult(publicationID string) publication.Result {
	return publication.Result{
		Protocol:      publication.ProtocolV1,
		PublicationID: publicationID,
		RunID:         "01ARZ3NDEKTSV4RRFFQ69G5FAV",
		HeadSHA:       strings.Repeat("d", 40),
		Status:        publication.StatusReady,
	}
}

func publicationCLIResultChallenge(t *testing.T, publicationID, headSHA string, status publication.ResultStatus) *publication.EffectChallenge {
	t.Helper()
	challenge := publication.EffectChallenge{
		PublicationID:  publicationID,
		Attempt:        1,
		CommitSHA:      headSHA,
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
		challenge.Marker = "<!-- no-mistakes-factory-publication-v1:" + publicationID + ":" + headSHA + " -->"
		challenge.PreparedDraft = "Inspectable draft\n\n" + challenge.Marker + "\n"
		digest := sha256.Sum256([]byte(challenge.PreparedDraft))
		challenge.DraftSHA256 = hex.EncodeToString(digest[:])
	}
	bound, err := publication.BindEffectChallengeDecisions(challenge)
	if err != nil {
		t.Fatal(err)
	}
	return &bound
}

func TestPublicationCommandsReadCanonicalStdinOrNamedFileAndWriteOneResult(t *testing.T) {
	fixture := newPublicationCLIFixture(t)
	request, parsed := publicationCLIRequest(t, fixture.identity)
	authorization := publicationAuthorizationBytes(parsed.PublicationID)
	query := publicationStatusQueryBytes(parsed.PublicationID)
	ready := fixture.readyResult(parsed.PublicationID)
	fixture.results[ipc.MethodPublicationStart] = ready
	fixture.results[ipc.MethodPublicationAuthorize] = ready
	fixture.results[ipc.MethodPublicationStatus] = ready

	recorder := &telemetryRecorder{}
	restoreTelemetry := telemetry.SetDefaultForTesting(recorder)
	t.Cleanup(restoreTelemetry)

	tests := []struct {
		name   string
		verb   string
		method string
		input  []byte
	}{
		{name: "start", verb: "start", method: ipc.MethodPublicationStart, input: request},
		{name: "authorize", verb: "authorize", method: ipc.MethodPublicationAuthorize, input: authorization},
		{name: "status", verb: "status", method: ipc.MethodPublicationStatus, input: query},
	}
	wantJSON, err := json.Marshal(ready)
	if err != nil {
		t.Fatal(err)
	}

	for _, test := range tests {
		t.Run(test.name+"_stdin", func(t *testing.T) {
			stdout, stderr, err := executePublicationCLI(test.input, "publication", test.verb)
			if err != nil {
				t.Fatalf("publication %s from stdin: %v; stderr=%q", test.verb, err, stderr)
			}
			if stdout != string(wantJSON)+"\n" {
				t.Fatalf("stdout = %q, want exactly one canonical result %q", stdout, string(wantJSON)+"\n")
			}
		})

		t.Run(test.name+"_named_file", func(t *testing.T) {
			requestPath := filepath.Join(t.TempDir(), test.verb+".json")
			if err := os.WriteFile(requestPath, test.input, 0o600); err != nil {
				t.Fatal(err)
			}
			stdout, stderr, err := executePublicationCLI(nil, "publication", test.verb, "--request", requestPath)
			if err != nil {
				t.Fatalf("publication %s from file: %v; stderr=%q", test.verb, err, stderr)
			}
			if stdout != string(wantJSON)+"\n" {
				t.Fatalf("stdout = %q, want exactly one canonical result %q", stdout, string(wantJSON)+"\n")
			}
		})

		if fixture.callsFor(test.method) != 2 {
			t.Fatalf("%s RPC calls = %d, want 2", test.method, fixture.callsFor(test.method))
		}
	}
	fixture.assertOnlyPublicationCalls(t)
	if len(recorder.events) != 0 {
		t.Fatalf("publication machine commands emitted telemetry: %#v", recorder.events)
	}
	if _, err := os.Stat(fixture.paths.DB()); !os.IsNotExist(err) {
		t.Fatalf("publication CLI created or touched the local AXI database before daemon admission: %v", err)
	}
}

func TestPublicationStartVerifiesPinnedPublisherBeforeAdmission(t *testing.T) {
	fixture := newPublicationCLIFixture(t)
	tests := []struct {
		name   string
		mutate func(*publication.Request)
	}{
		{
			name: "executable path",
			mutate: func(request *publication.Request) {
				request.Publisher.ExecutablePath = "/different/publisher/no-mistakes"
			},
		},
		{
			name: "raw executable SHA-256",
			mutate: func(request *publication.Request) {
				request.Publisher.ExecutableSHA256 = strings.Repeat("0", 64)
			},
		},
		{
			name: "build SHA",
			mutate: func(request *publication.Request) {
				request.Publisher.BuildSHA = strings.Repeat("c", 40)
			},
		},
		{
			name: "protocol",
			mutate: func(request *publication.Request) {
				request.Publisher.Protocol = "factory-publication-v2"
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request, _ := publicationCLIRequestValue(t, fixture.identity)
			test.mutate(&request)
			raw, err := json.Marshal(request)
			if err != nil {
				t.Fatal(err)
			}
			stdout, _, err := executePublicationCLI(raw, "publication", "start")
			if err == nil {
				t.Fatal("publication start accepted a request bound to another publisher")
			}
			if stdout != "" {
				t.Fatalf("failed pre-admission wrote machine success output %q", stdout)
			}
		})
	}
	if fixture.callsFor(ipc.MethodPublicationStart) != 0 {
		t.Fatalf("publisher mismatch reached admission %d times", fixture.callsFor(ipc.MethodPublicationStart))
	}
}

func TestPublicationRejectsIncompatibleDaemonHandshakeBeforeAdmission(t *testing.T) {
	fixture := newPublicationCLIFixture(t)
	request, _ := publicationCLIRequest(t, fixture.identity)
	fixture.identity.BuildSHA = strings.Repeat("c", 40)

	stdout, _, err := executePublicationCLI(request, "publication", "start")
	if err == nil {
		t.Fatal("publication start accepted an incompatible daemon")
	}
	if stdout != "" {
		t.Fatalf("incompatible daemon wrote machine result %q", stdout)
	}
	if fixture.callsFor(ipc.MethodPublicationHandshake) != 1 {
		t.Fatalf("handshake calls = %d, want 1", fixture.callsFor(ipc.MethodPublicationHandshake))
	}
	if fixture.callsFor(ipc.MethodPublicationStart) != 0 {
		t.Fatalf("incompatible daemon reached admission %d times", fixture.callsFor(ipc.MethodPublicationStart))
	}
}

func TestPublicationCommandsRejectNonCanonicalInputBeforeRPC(t *testing.T) {
	fixture := newPublicationCLIFixture(t)
	request, parsed := publicationCLIRequest(t, fixture.identity)
	tests := []struct {
		verb   string
		method string
		input  []byte
	}{
		{verb: "start", method: ipc.MethodPublicationStart, input: append(request, '\n')},
		{verb: "authorize", method: ipc.MethodPublicationAuthorize, input: append(publicationAuthorizationBytes(parsed.PublicationID), '\n')},
		{verb: "status", method: ipc.MethodPublicationStatus, input: append(publicationStatusQueryBytes(parsed.PublicationID), '\n')},
	}
	for _, test := range tests {
		t.Run(test.verb, func(t *testing.T) {
			stdout, _, err := executePublicationCLI(test.input, "publication", test.verb)
			if err == nil {
				t.Fatal("non-canonical input was accepted")
			}
			if stdout != "" {
				t.Fatalf("rejected input wrote stdout %q", stdout)
			}
			if fixture.callsFor(test.method) != 0 {
				t.Fatalf("rejected input invoked %s", test.method)
			}
		})
	}
}

func TestPublicationCLIExitZeroIsExactlyREADY(t *testing.T) {
	fixture := newPublicationCLIFixture(t)
	_, parsed := publicationCLIRequest(t, fixture.identity)
	query := publicationStatusQueryBytes(parsed.PublicationID)
	statuses := []publication.ResultStatus{
		publication.StatusChecking,
		publication.StatusReadyForPush,
		publication.StatusReadyForPR,
		publication.StatusCIObserving,
		publication.StatusReady,
		publication.StatusFailed,
		publication.StatusDrift,
		publication.StatusDenied,
		publication.StatusEffectUnknown,
	}
	for _, status := range statuses {
		t.Run(string(status), func(t *testing.T) {
			result := fixture.readyResult(parsed.PublicationID)
			result.Status = status
			if status == publication.StatusReadyForPush || status == publication.StatusReadyForPR {
				result.Challenge = publicationCLIResultChallenge(t, result.PublicationID, result.HeadSHA, status)
			}
			fixture.results[ipc.MethodPublicationStatus] = result
			stdout, _, err := executePublicationCLI(query, "publication", "status")
			var code int
			if err == nil {
				code = 0
			} else if typed, ok := err.(*exitError); ok {
				code = typed.code
			} else {
				t.Fatalf("status %s returned unclassified error %T: %v", status, err, err)
			}
			if status == publication.StatusReady && code != 0 {
				t.Fatalf("READY exit = %d, want 0", code)
			}
			if status != publication.StatusReady && code == 0 {
				t.Fatalf("%s exit = 0; only READY may succeed", status)
			}
			want, marshalErr := json.Marshal(result)
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			if stdout != string(want)+"\n" {
				t.Fatalf("stdout = %q, want %q", stdout, string(want)+"\n")
			}
		})
	}
}

func executePublicationCLI(input []byte, args ...string) (string, string, error) {
	cmd := newRootCmd()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.SetIn(bytes.NewReader(input))
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return stdout.String(), stderr.String(), err
}

func publicationCLIRequest(t *testing.T, identity ipc.PublicationIdentity) ([]byte, publication.ParsedRequest) {
	t.Helper()
	request, raw := publicationCLIRequestValue(t, identity)
	_ = request
	parsed, err := publication.ParseRequest(raw)
	if err != nil {
		t.Fatalf("parse test publication request: %v", err)
	}
	return raw, parsed
}

func publicationCLIRequestValue(t *testing.T, identity ipc.PublicationIdentity) (publication.Request, []byte) {
	t.Helper()
	request := publication.Request{
		Protocol: publication.ProtocolV1,
		Factory: publication.FactoryBinding{
			RunID:                "factory-run-cli",
			TerminalT10Sequence:  10,
			RunStatePrefixSHA256: strings.Repeat("a", 64),
			PlanBindingSHA256:    strings.Repeat("b", 64),
		},
		WorkContract: publication.WorkContractBinding{
			Path:   ".agent/work-contract.toml",
			SHA256: strings.Repeat("c", 64),
		},
		BuildIntent: publication.BuildIntentProjection{
			Summary:            "publish the exact completed candidate",
			AcceptanceCriteria: []string{"CI passes at the exact candidate head"},
		},
		Candidate: publication.CandidateBinding{
			RepositoryID: "01ARZ3NDEKTSV4RRFFQ69G5FAV",
			HeadRef:      "refs/heads/codex/factory-publication-v1",
			BaseRef:      "refs/heads/main",
			BaseSHA:      strings.Repeat("9", 40),
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
			Push: publication.PushScope{
				Mode:           publication.PushModeExactCommit,
				RemoteIdentity: "github.com/example/project",
				DestinationRef: "refs/heads/codex/factory-publication-v1",
			},
			PR: publication.PRScope{
				Mode:    publication.PRModeCreateOrUpdateExactHead,
				BaseRef: "refs/heads/main",
				HeadRef: "refs/heads/codex/factory-publication-v1",
			},
			CI: publication.CIScope{Mode: publication.CIModeObserveExactHead},
		},
	}
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	return request, raw
}

func publicationAuthorizationBytes(publicationID string) []byte {
	return []byte(`{"protocol":"factory-publication-v1","decision":"GO","publication_id":"` + publicationID + `","kind":"push","attempt":1,"commit_sha":"` + strings.Repeat("d", 40) + `","remote_identity":"github.com/example/project","destination_ref":"refs/heads/codex/factory-publication-v1","base_ref":"","head_ref":"refs/heads/codex/factory-publication-v1","draft_sha256":"","effect_digest":"` + strings.Repeat("e", 64) + `","decision_digest":"` + strings.Repeat("f", 64) + `"}`)
}

func publicationStatusQueryBytes(publicationID string) []byte {
	return []byte(`{"protocol":"factory-publication-v1","publication_id":"` + publicationID + `"}`)
}
