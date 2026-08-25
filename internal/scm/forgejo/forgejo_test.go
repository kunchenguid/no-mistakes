package forgejo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

const (
	testBaseURL = "https://forge.example:3443/git"
	testRepo    = "octo/widgets"
	testPRURL   = testBaseURL + "/octo/widgets/pulls/42"
	testHeadSHA = "0123456789abcdef0123456789abcdef01234567"
)

func TestResolveRemoteSupportsPortsAndPathPrefixes(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name       string
		remote     string
		configured string
		resolved   string
		wantBase   string
		wantRepo   string
	}{
		{
			name:     "infer HTTPS base prefix",
			remote:   "https://forgejo.example:3443/git/octo/widgets.git",
			wantBase: "https://forgejo.example:3443/git",
			wantRepo: "octo/widgets",
		},
		{
			name:       "configured self-hosted base",
			remote:     "https://code.example:3443/scm/octo/widgets.git",
			configured: "https://code.example:3443/scm/",
			wantBase:   "https://code.example:3443/scm",
			wantRepo:   "octo/widgets",
		},
		{
			name:       "canonical configured base",
			remote:     "https://code.example/octo/widgets.git",
			configured: "HTTPS://CODE.EXAMPLE:443/",
			wantBase:   "https://code.example",
			wantRepo:   "octo/widgets",
		},
		{
			name:       "canonical remote authority",
			remote:     "https://CODE.EXAMPLE:443/octo/widgets.git",
			configured: "https://code.example",
			wantBase:   "https://code.example",
			wantRepo:   "octo/widgets",
		},
		{
			name:       "canonical HTTP default port",
			remote:     "http://code.example/octo/widgets.git",
			configured: "HTTP://CODE.EXAMPLE:80",
			wantBase:   "http://code.example",
			wantRepo:   "octo/widgets",
		},
		{
			name:     "canonical inferred base",
			remote:   "https://FORGEJO.EXAMPLE:443/octo/widgets.git",
			wantBase: "https://forgejo.example",
			wantRepo: "octo/widgets",
		},
		{
			name:       "SSH origin with HTTPS base",
			remote:     "ssh://git@code.example:2222/scm/octo/widgets.git",
			configured: "https://code.example:3443/scm",
			wantBase:   "https://code.example:3443/scm",
			wantRepo:   "octo/widgets",
		},
		{
			name:       "scp origin with prefix",
			remote:     "git@code.example:scm/octo/widgets.git",
			configured: "https://code.example/scm",
			wantBase:   "https://code.example/scm",
			wantRepo:   "octo/widgets",
		},
		{
			name:       "SSH alias with resolved host",
			remote:     "git@forgejo-work:scm/octo/widgets.git",
			configured: "https://code.example/scm",
			resolved:   "code.example",
			wantBase:   "https://code.example/scm",
			wantRepo:   "octo/widgets",
		},
		{
			name:       "canonical pull URL",
			remote:     testPRURL,
			configured: testBaseURL,
			wantBase:   testBaseURL,
			wantRepo:   testRepo,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			base, repo, err := ResolveRemote(tt.remote, tt.configured, tt.resolved)
			if err != nil {
				t.Fatalf("ResolveRemote() error = %v", err)
			}
			if base != tt.wantBase || repo != tt.wantRepo {
				t.Fatalf("ResolveRemote() = (%q, %q), want (%q, %q)", base, repo, tt.wantBase, tt.wantRepo)
			}
		})
	}
}

func TestResolveRemoteRejectsIdentityMismatch(t *testing.T) {
	t.Parallel()
	_, _, err := ResolveRemote("https://other.example/scm/octo/widgets.git", "https://code.example/scm", "")
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("ResolveRemote() error = %v, want identity mismatch", err)
	}
}

func TestResolveRemoteRejectsUnsupportedURLScheme(t *testing.T) {
	t.Parallel()
	_, _, err := ResolveRemote("ftp://code.example/scm/octo/widgets.git", "https://code.example/scm", "")
	if err == nil || !strings.Contains(err.Error(), "scheme") {
		t.Fatalf("ResolveRemote() error = %v, want unsupported scheme", err)
	}
}

func TestAvailableUsesConfiguredExecutableAndRuntimeCapabilities(t *testing.T) {
	status := fixture(t, "status-forgejo-16.json")
	recorder := &fakeRecorder{responses: []fakeResponse{{stdout: status}}}
	host := newTestHost(recorder)

	if err := host.Available(context.Background()); err != nil {
		t.Fatalf("Available() error = %v", err)
	}
	wantArgs := []string{"status", "--base-url", testBaseURL, "--token-env", "FORGEJO_TEST_TOKEN", "--json"}
	if got := recorder.calls[0]; got.name != "/opt/tools/forgejo-axi-custom" || !reflect.DeepEqual(got.args, wantArgs) {
		t.Fatalf("command = %#v, want name custom and args %#v", got, wantArgs)
	}
	caps := host.Capabilities()
	if !caps.MergeableState || !caps.MergedProof || !caps.FailedCheckLogs {
		t.Fatalf("Capabilities() = %+v, want Forgejo 16 capabilities including failed logs", caps)
	}
}

func TestAvailableGatesMergedProofFromRuntimeCapability(t *testing.T) {
	status := fixture(t, "status-forgejo-16.json")
	status = strings.Replace(status, `"expected_head_merge":true`, `"expected_head_merge":false`, 1)
	if status == fixture(t, "status-forgejo-16.json") {
		t.Fatal("fixture does not contain expected-head merge capability")
	}
	host := newTestHost(&fakeRecorder{responses: []fakeResponse{{stdout: status}}})
	if err := host.Available(context.Background()); err != nil {
		t.Fatalf("Available() error = %v", err)
	}
	caps := host.Capabilities()
	if caps.MergedProof {
		t.Fatalf("Capabilities() = %+v, want merged proof disabled", caps)
	}
	if !caps.MergeableState || !caps.FailedCheckLogs {
		t.Fatalf("Capabilities() = %+v, want other probed capabilities preserved", caps)
	}
}

func TestAvailableGatesMergeabilityIndependentlyFromCommitStatuses(t *testing.T) {
	status := fixture(t, "status-forgejo-16.json")
	status = strings.Replace(status, `"commit_statuses":true`, `"commit_statuses":false`, 1)
	if status == fixture(t, "status-forgejo-16.json") {
		t.Fatal("fixture does not contain commit-status capability")
	}
	host := newTestHost(&fakeRecorder{responses: []fakeResponse{{stdout: status}}})
	if err := host.Available(context.Background()); err != nil {
		t.Fatalf("Available() error = %v", err)
	}
	caps := host.Capabilities()
	if !caps.MergeableState || !caps.MergedProof || caps.FailedCheckLogs {
		t.Fatalf("Capabilities() = %+v, want independent mergeability, merged proof, and status/log gating", caps)
	}
}

func TestAvailableAcceptsHostScopedTokenSource(t *testing.T) {
	status := strings.Replace(fixture(t, "status-forgejo-16.json"), "FORGEJO_TEST_TOKEN", "FORGEJO_TOKEN_FORGE_2E_EXAMPLE_3A_3443", 1)
	recorder := &fakeRecorder{responses: []fakeResponse{{stdout: status}}}
	host := New(Options{
		CommandFactory: recorder.factory,
		CLIAvailable:   func(string) bool { return true },
		Executable:     "/opt/tools/forgejo-axi-custom",
		BaseURL:        testBaseURL,
		Repository:     testRepo,
	})
	if err := host.Available(context.Background()); err != nil {
		t.Fatalf("Available() error = %v", err)
	}
	wantArgs := []string{"status", "--base-url", testBaseURL, "--json"}
	if gotArgs := recorder.calls[0].args; !reflect.DeepEqual(gotArgs, wantArgs) {
		t.Fatalf("status args = %#v, want %#v", gotArgs, wantArgs)
	}
}

func TestAvailableRejectsIncompleteStatusIdentity(t *testing.T) {
	status := fixture(t, "status-forgejo-16.json")
	tests := []struct {
		name string
		old  string
		new  string
		want string
	}{
		{name: "API URL", old: `"api_url":"https://forge.example:3443/git/api/v1"`, new: `"api_url":""`, want: "API identity"},
		{name: "auth source", old: `"source":"FORGEJO_TEST_TOKEN"`, new: `"source":null`, want: "authentication source"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			response := strings.Replace(status, tt.old, tt.new, 1)
			if response == status {
				t.Fatalf("fixture does not contain %q", tt.old)
			}
			host := newTestHost(&fakeRecorder{responses: []fakeResponse{{stdout: response}}})
			err := host.Available(context.Background())
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Available() error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestAvailableRejectsUnprovenCapabilitySources(t *testing.T) {
	status := fixture(t, "status-forgejo-16.json")
	for _, source := range []string{"major-version", "other"} {
		t.Run(source, func(t *testing.T) {
			response := strings.Replace(status, `"probe":{"source":"swagger","complete":true}`, fmt.Sprintf(`"probe":{"source":%q,"complete":true}`, source), 1)
			if response == status {
				t.Fatal("fixture does not contain the Swagger capability probe")
			}
			host := newTestHost(&fakeRecorder{responses: []fakeResponse{{stdout: response}}})
			err := host.Available(context.Background())
			if err == nil || !strings.Contains(err.Error(), "capability probe") {
				t.Fatalf("Available() error = %v, want rejected capability probe", err)
			}
			if host.Capabilities().FailedCheckLogs {
				t.Fatal("unproven capability source advertised failed-check logs")
			}
		})
	}
}

func TestForgejo15KeepsStatusGatingWithoutActionLogs(t *testing.T) {
	recorder := &fakeRecorder{responses: []fakeResponse{
		{stdout: fixture(t, "status-forgejo-15.json")},
		{stdout: checksJSON("success", "not_required", true, `[{
			"context":"test/linux","state":"success","description":"ok","target_url":null,"created_at":"2025-01-01T00:00:00Z","updated_at":"2025-01-01T00:01:00Z"
		}]`, `[]`)},
	}}
	host := newTestHost(recorder)
	if err := host.Available(context.Background()); err != nil {
		t.Fatalf("Available() error = %v", err)
	}
	caps := host.Capabilities()
	if !caps.MergeableState || !caps.MergedProof || caps.FailedCheckLogs {
		t.Fatalf("Capabilities() = %+v, want mergeability and merged proof with independently unsupported logs", caps)
	}
	checks, err := host.GetChecks(context.Background(), testPR())
	if err != nil {
		t.Fatalf("GetChecks() error = %v", err)
	}
	if len(checks) != 1 || checks[0].Bucket != scm.CheckBucketPass {
		t.Fatalf("GetChecks() = %+v, want passing commit status", checks)
	}
	callsBeforeLogs := len(recorder.calls)
	logs, err := host.FetchFailedCheckLogs(context.Background(), testPR(), "feature/forgejo", testHeadSHA, []string{"test/linux"})
	if logs != "" || !errors.Is(err, scm.ErrUnsupported) {
		t.Fatalf("FetchFailedCheckLogs() = (%q, %v), want independently unsupported", logs, err)
	}
	if len(recorder.calls) != callsBeforeLogs {
		t.Fatal("unsupported Forgejo 15 logs issued a run command")
	}
}

func TestPRLifecycleCommandsAndIdempotentCreate(t *testing.T) {
	pr := pullJSON("open", false, testHeadSHA)
	recorder := &fakeRecorder{responses: []fakeResponse{
		{stdout: `{"found":true,"pull_request":` + pr + `,"search_info":{"complete":true,"pages":1,"fetched":1,"total":1}}`},
		{stdout: `{"created":false,"pull_request":` + pr + `}`},
		{stdout: `{"updated":true,"pull_request":` + pr + `}`},
		{stdout: `{"pull_request":` + pr + `}`},
	}}
	host := newTestHost(recorder)

	found, err := host.FindPR(context.Background(), "feature/forgejo", "main")
	if err != nil || found == nil || found.Number != "42" || found.URL != testPRURL || found.HeadSHA != testHeadSHA {
		t.Fatalf("FindPR() = (%+v, %v)", found, err)
	}
	created, err := host.CreatePR(context.Background(), "feature/forgejo", "main", scm.PRContent{Title: "Title", Body: "line one\nline two"})
	if err != nil || created == nil || created.Number != "42" {
		t.Fatalf("CreatePR() = (%+v, %v)", created, err)
	}
	if _, err := host.UpdatePR(context.Background(), created, scm.PRContent{Title: "New title", Body: "new body"}); err != nil {
		t.Fatalf("UpdatePR() error = %v", err)
	}
	state, err := host.GetPRState(context.Background(), created)
	if err != nil || state != scm.PRStateOpen {
		t.Fatalf("GetPRState() = (%q, %v)", state, err)
	}

	want := [][]string{
		{"pr", "find", "--repo", testRepo, "--head", "feature/forgejo", "--base", "main", "--state", "open", "--base-url", testBaseURL, "--token-env", "FORGEJO_TEST_TOKEN", "--json"},
		{"pr", "create", "--repo", testRepo, "--head", "feature/forgejo", "--base", "main", "--title", "Title", "--body", "line one\nline two", "--base-url", testBaseURL, "--token-env", "FORGEJO_TEST_TOKEN", "--json"},
		{"pr", "update", "--repo", testRepo, "42", "--title", "New title", "--body", "new body", "--base-url", testBaseURL, "--token-env", "FORGEJO_TEST_TOKEN", "--json"},
		{"pr", "view", "--repo", testRepo, "42", "--base-url", testBaseURL, "--token-env", "FORGEJO_TEST_TOKEN", "--json"},
	}
	for i := range want {
		if !reflect.DeepEqual(recorder.calls[i].args, want[i]) {
			t.Errorf("call %d args = %#v, want %#v", i, recorder.calls[i].args, want[i])
		}
	}
}

func TestFindPRNoMatchIsExplicit(t *testing.T) {
	host := newTestHost(&fakeRecorder{responses: []fakeResponse{{stdout: `{
		"found":false,"pull_request":null,
		"search_info":{"complete":true,"pages":1,"fetched":3,"total":3}
	}`}}})
	pr, err := host.FindPR(context.Background(), "missing", "main")
	if err != nil || pr != nil {
		t.Fatalf("FindPR() = (%+v, %v), want (nil, nil)", pr, err)
	}
}

func TestChecksFailClosedAcrossStates(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		overall       string
		requiredState string
		passes        bool
		statuses      string
		required      string
		wantBuckets   []scm.CheckBucket
	}{
		{
			name: "success", overall: "success", requiredState: "success", passes: true,
			statuses: `[{"context":"test/linux","state":"success","description":"ok","target_url":null,"created_at":null,"updated_at":null}]`,
			required: `[{"context":"test/linux","matched":["test/linux"],"state":"success"}]`, wantBuckets: []scm.CheckBucket{scm.CheckBucketPass},
		},
		{
			name: "pending", overall: "pending", requiredState: "pending", passes: false,
			statuses: `[{"context":"test/linux","state":"pending","description":null,"target_url":null,"created_at":null,"updated_at":null}]`,
			required: `[{"context":"test/linux","matched":["test/linux"],"state":"pending"}]`, wantBuckets: []scm.CheckBucket{scm.CheckBucketPending},
		},
		{
			name: "failure", overall: "failure", requiredState: "failure", passes: false,
			statuses: `[{"context":"test/linux","state":"failure","description":"boom","target_url":null,"created_at":null,"updated_at":null}]`,
			required: `[{"context":"test/linux","matched":["test/linux"],"state":"failure"}]`, wantBuckets: []scm.CheckBucket{scm.CheckBucketFail},
		},
		{
			name: "none stays empty for pipeline-owned no-ci handling", overall: "none", requiredState: "not_required", passes: false,
			statuses: `[]`, required: `[]`, wantBuckets: nil,
		},
		{
			name: "missing required context is failure", overall: "success", requiredState: "missing", passes: false,
			statuses: `[{"context":"lint","state":"success","description":null,"target_url":null,"created_at":null,"updated_at":null}]`,
			required: `[{"context":"test/*","matched":[],"state":"missing"}]`, wantBuckets: []scm.CheckBucket{scm.CheckBucketPass, scm.CheckBucketFail},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			host := newTestHost(&fakeRecorder{responses: []fakeResponse{{stdout: checksJSON(tt.overall, tt.requiredState, tt.passes, tt.statuses, tt.required)}}})
			got, err := host.GetChecks(context.Background(), testPR())
			if err != nil {
				t.Fatalf("GetChecks() error = %v", err)
			}
			var buckets []scm.CheckBucket
			for _, check := range got {
				buckets = append(buckets, check.Bucket)
			}
			if !reflect.DeepEqual(buckets, tt.wantBuckets) {
				t.Fatalf("buckets = %v, want %v (checks=%+v)", buckets, tt.wantBuckets, got)
			}
		})
	}
}

func TestChecksRejectMissingProtectionOutput(t *testing.T) {
	response := checksJSON(
		"success",
		"not_required",
		true,
		`[{"context":"test/linux","state":"success","updated_at":null}]`,
		`[]`,
	)
	withoutProtection := strings.Replace(response, ",\n\t\t\"protection\":{\"protected\":true,\"rule\":\"main\",\"status_checks_enabled\":true}", "", 1)
	if withoutProtection == response {
		t.Fatal("checks fixture did not contain protection output")
	}
	host := newTestHost(&fakeRecorder{responses: []fakeResponse{{stdout: withoutProtection}}})
	_, err := host.GetChecks(context.Background(), testPR())
	if err == nil || !strings.Contains(err.Error(), "protection") {
		t.Fatalf("GetChecks() error = %v, want missing protection error", err)
	}
}

func TestChecksRejectRequiredMatchesAbsentFromStatuses(t *testing.T) {
	host := newTestHost(&fakeRecorder{responses: []fakeResponse{{stdout: checksJSON(
		"success",
		"success",
		true,
		`[{"context":"test/linux","state":"success","updated_at":null}]`,
		`[{"context":"required/*","state":"success","matched":["required/linux"]}]`,
	)}}})
	_, err := host.GetChecks(context.Background(), testPR())
	if err == nil || !strings.Contains(err.Error(), "required/linux") {
		t.Fatalf("GetChecks() error = %v, want unknown matched context error", err)
	}
}

func TestChecksRejectInconsistentRequiredSummary(t *testing.T) {
	tests := []struct {
		name          string
		requiredState string
		required      string
		want          string
	}{
		{
			name: "unknown required item state", requiredState: "success",
			required: `[{"context":"test/*","matched":["test/linux"],"state":"unknown"}]`,
			want:     "unknown required status state",
		},
		{
			name: "aggregate does not match items", requiredState: "success",
			required: `[{"context":"test/*","matched":[],"state":"missing"}]`,
			want:     "inconsistent required status summary",
		},
		{
			name: "matched required context has no matches", requiredState: "success",
			required: `[{"context":"test/*","matched":[],"state":"success"}]`,
			want:     "has no matches",
		},
	}
	statuses := `[{"context":"test/linux","state":"success","description":"ok","target_url":null,"created_at":null,"updated_at":null}]`
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			host := newTestHost(&fakeRecorder{responses: []fakeResponse{{stdout: checksJSON("success", tt.requiredState, true, statuses, tt.required)}}})
			_, err := host.GetChecks(context.Background(), testPR())
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("GetChecks() error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestChecksAcceptsAdvancedHead(t *testing.T) {
	statuses := `[{"context":"test/linux","state":"success","description":"ok","target_url":null,"created_at":null,"updated_at":null}]`
	host := newTestHost(&fakeRecorder{responses: []fakeResponse{{stdout: strings.ReplaceAll(
		checksJSON("success", "not_required", true, statuses, `[]`), testHeadSHA, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
	}}})
	checks, err := host.GetChecks(context.Background(), testPR())
	if err != nil || len(checks) != 1 || checks[0].Bucket != scm.CheckBucketPass {
		t.Fatalf("GetChecks() = (%+v, %v), want passing advanced head", checks, err)
	}
}

func TestMergeabilityCommandDecoding(t *testing.T) {
	recorder := &fakeRecorder{responses: []fakeResponse{
		{stdout: fixture(t, "status-forgejo-16.json")},
		{stdout: `{"mergeability":{"number":42,"url":"` + testPRURL + `","head_sha":"` + testHeadSHA + `","forgejo_mergeable":false,"checks_pass":false,"mergeable":false,"reasons":["forgejo_not_mergeable"]}}`},
	}}
	host := newTestHost(recorder)
	if err := host.Available(context.Background()); err != nil {
		t.Fatalf("Available() error = %v", err)
	}
	got, err := host.GetMergeableState(context.Background(), testPR())
	if err != nil || got != scm.MergeableConflict {
		t.Fatalf("GetMergeableState() = (%q, %v), want conflict", got, err)
	}
	wantArgs := []string{"pr", "mergeability", "--repo", testRepo, "42", "--base-url", testBaseURL, "--token-env", "FORGEJO_TEST_TOKEN", "--json"}
	if gotArgs := recorder.calls[1].args; !reflect.DeepEqual(gotArgs, wantArgs) {
		t.Fatalf("mergeability args = %#v, want %#v", gotArgs, wantArgs)
	}
}

func TestMergeabilityAcceptsAdvancedHead(t *testing.T) {
	recorder := &fakeRecorder{responses: []fakeResponse{
		{stdout: fixture(t, "status-forgejo-16.json")},
		{stdout: `{"mergeability":{"number":42,"url":"` + testPRURL + `","head_sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","forgejo_mergeable":true,"checks_pass":true,"mergeable":true,"reasons":[]}}`},
	}}
	host := newTestHost(recorder)
	if err := host.Available(context.Background()); err != nil {
		t.Fatalf("Available() error = %v", err)
	}
	got, err := host.GetMergeableState(context.Background(), testPR())
	if err != nil || got != scm.MergeableOK {
		t.Fatalf("GetMergeableState() = (%q, %v), want mergeable advanced head", got, err)
	}
}

func TestMergedProofRequiresExpectedHeadAndCanonicalIdentity(t *testing.T) {
	proof := `{"merged":true,"number":42,"url":"` + testPRURL + `","head_sha":"` + testHeadSHA + `","merge_commit_sha":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","merged_at":"2025-01-02T00:00:00Z","merged_by":"alice"}`
	recorder := &fakeRecorder{responses: []fakeResponse{{stdout: `{"proof":` + proof + `}`}}}
	host := newTestHost(recorder)
	got, err := host.GetMergedProof(context.Background(), testPR(), testHeadSHA)
	if err != nil || !got.Merged || got.HeadSHA != testHeadSHA || got.MergeCommitSHA == "" {
		t.Fatalf("GetMergedProof() = (%+v, %v)", got, err)
	}
	wantArgs := []string{"pr", "merged", "--repo", testRepo, "42", "--base-url", testBaseURL, "--token-env", "FORGEJO_TEST_TOKEN", "--json"}
	if gotArgs := recorder.calls[0].args; !reflect.DeepEqual(gotArgs, wantArgs) {
		t.Fatalf("merged proof args = %#v, want %#v", gotArgs, wantArgs)
	}
}

func TestMergedProofRejectsEmptyExpectedHead(t *testing.T) {
	proof := `{"merged":true,"number":42,"url":"` + testPRURL + `","head_sha":"` + testHeadSHA + `","merge_commit_sha":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","merged_at":"2025-01-02T00:00:00Z","merged_by":"alice"}`
	host := newTestHost(&fakeRecorder{responses: []fakeResponse{{stdout: `{"proof":` + proof + `}`}}})
	_, err := host.GetMergedProof(context.Background(), testPR(), "")
	if err == nil || !strings.Contains(err.Error(), "expected head") {
		t.Fatalf("GetMergedProof() error = %v, want missing expected head", err)
	}
}

func TestMergedProofRejectsAlreadyMergedHeadRace(t *testing.T) {
	proof := `{"merged":true,"number":42,"url":"` + testPRURL + `","head_sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","merge_commit_sha":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","merged_at":"2025-01-02T00:00:00Z","merged_by":"alice"}`
	host := newTestHost(&fakeRecorder{responses: []fakeResponse{{stdout: `{"proof":` + proof + `}`}}})
	_, err := host.GetMergedProof(context.Background(), testPR(), testHeadSHA)
	if !errors.Is(err, scm.ErrHeadChanged) {
		t.Fatalf("GetMergedProof() error = %v, want ErrHeadChanged", err)
	}
}

func TestRejectsMismatchedPRIdentityAndIncompleteSearch(t *testing.T) {
	t.Run("input URL", func(t *testing.T) {
		host := newTestHost(&fakeRecorder{})
		_, err := host.GetPRState(context.Background(), &scm.PR{Number: "42", URL: "https://evil.example/octo/widgets/pulls/42"})
		if err == nil || !strings.Contains(err.Error(), "identity") {
			t.Fatalf("GetPRState() error = %v, want identity error", err)
		}
	})
	t.Run("output URL", func(t *testing.T) {
		bad := strings.ReplaceAll(pullJSON("open", false, testHeadSHA), testPRURL, "https://evil.example/octo/widgets/pulls/42")
		host := newTestHost(&fakeRecorder{responses: []fakeResponse{{stdout: `{"found":true,"pull_request":` + bad + `,"search_info":{"complete":true,"pages":1,"fetched":1,"total":1}}`}}})
		_, err := host.FindPR(context.Background(), "feature/forgejo", "main")
		if err == nil || !strings.Contains(err.Error(), "identity") {
			t.Fatalf("FindPR() error = %v, want identity error", err)
		}
	})
	t.Run("output number", func(t *testing.T) {
		bad := strings.ReplaceAll(pullJSON("open", false, testHeadSHA), `"number":42`, `"number":43`)
		bad = strings.ReplaceAll(bad, "/pulls/42", "/pulls/43")
		host := newTestHost(&fakeRecorder{responses: []fakeResponse{{stdout: `{"pull_request":` + bad + `}`}}})
		_, err := host.GetPRState(context.Background(), &scm.PR{Number: "42", URL: testPRURL})
		if err == nil || !strings.Contains(err.Error(), "number") {
			t.Fatalf("GetPRState() error = %v, want number identity error", err)
		}
	})
	t.Run("update output number", func(t *testing.T) {
		bad := strings.ReplaceAll(pullJSON("open", false, testHeadSHA), `"number":42`, `"number":43`)
		bad = strings.ReplaceAll(bad, "/pulls/42", "/pulls/43")
		host := newTestHost(&fakeRecorder{responses: []fakeResponse{{stdout: `{"updated":true,"pull_request":` + bad + `}`}}})
		_, err := host.UpdatePR(context.Background(), testPR(), scm.PRContent{Title: "title", Body: "body"})
		if err == nil || !strings.Contains(err.Error(), "number") {
			t.Fatalf("UpdatePR() error = %v, want number identity error", err)
		}
	})
	t.Run("mergeability output number", func(t *testing.T) {
		recorder := &fakeRecorder{responses: []fakeResponse{
			{stdout: fixture(t, "status-forgejo-16.json")},
			{stdout: `{"mergeability":{"number":43,"url":"` + testBaseURL + `/octo/widgets/pulls/43","head_sha":"` + testHeadSHA + `","forgejo_mergeable":true,"checks_pass":true,"mergeable":true,"reasons":[]}}`},
		}}
		host := newTestHost(recorder)
		if err := host.Available(context.Background()); err != nil {
			t.Fatalf("Available() error = %v", err)
		}
		_, err := host.GetMergeableState(context.Background(), testPR())
		if err == nil || !strings.Contains(err.Error(), "number") {
			t.Fatalf("GetMergeableState() error = %v, want number identity error", err)
		}
	})
	t.Run("merged proof output number", func(t *testing.T) {
		proof := `{"merged":true,"number":43,"url":"` + testBaseURL + `/octo/widgets/pulls/43","head_sha":"` + testHeadSHA + `","merge_commit_sha":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","merged_at":"2025-01-02T00:00:00Z","merged_by":"alice"}`
		host := newTestHost(&fakeRecorder{responses: []fakeResponse{{stdout: `{"proof":` + proof + `}`}}})
		_, err := host.GetMergedProof(context.Background(), testPR(), testHeadSHA)
		if err == nil || !strings.Contains(err.Error(), "number") {
			t.Fatalf("GetMergedProof() error = %v, want number identity error", err)
		}
	})
	t.Run("incomplete search", func(t *testing.T) {
		host := newTestHost(&fakeRecorder{responses: []fakeResponse{{stdout: `{"found":false,"pull_request":null,"search_info":{"complete":false,"pages":10,"fetched":0,"total":null}}`}}})
		_, err := host.FindPR(context.Background(), "feature/forgejo", "main")
		if err == nil || !strings.Contains(err.Error(), "incomplete") {
			t.Fatalf("FindPR() error = %v, want incomplete search error", err)
		}
	})
}

func TestCommandFailuresAreActionableAndRedacted(t *testing.T) {
	t.Run("executable not found", func(t *testing.T) {
		host := newTestHostWithOptions(&fakeRecorder{}, func(string) bool { return false })
		err := host.Available(context.Background())
		if err == nil || !strings.Contains(err.Error(), "forgejo_axi_path") || !strings.Contains(err.Error(), "not found") {
			t.Fatalf("Available() error = %v, want actionable missing executable", err)
		}
	})
	t.Run("nonzero JSON error redacts overlapping tokens", func(t *testing.T) {
		recorder := &fakeRecorder{responses: []fakeResponse{{
			stdout: `{"error":"request failed with secret-token","code":"HTTP_ERROR","details":{"url":"https://user:pass@forge.example/path?token=secret-token"},"help":["check secret-token"]}`,
			code:   1,
		}}}
		host := newTestHost(recorder)
		_, err := host.FindPR(context.Background(), "feature/forgejo", "main")
		if err == nil || !strings.Contains(err.Error(), "HTTP_ERROR") || strings.Contains(err.Error(), "secret-token") || strings.Contains(err.Error(), "-token") || strings.Contains(err.Error(), "user:pass") {
			t.Fatalf("FindPR() error = %v, want code with secrets redacted", err)
		}
	})
	t.Run("malformed output", func(t *testing.T) {
		host := newTestHost(&fakeRecorder{responses: []fakeResponse{{stdout: `{not-json`}}})
		_, err := host.FindPR(context.Background(), "feature/forgejo", "main")
		if err == nil || !strings.Contains(err.Error(), "invalid JSON") {
			t.Fatalf("FindPR() error = %v, want invalid JSON", err)
		}
	})
	t.Run("oversized JSON output", func(t *testing.T) {
		host := newTestHost(&fakeRecorder{responses: []fakeResponse{{stdoutBytes: maxForgejoOutputBytes + 1}}})
		_, err := host.FindPR(context.Background(), "feature/forgejo", "main")
		if err == nil || !strings.Contains(err.Error(), "output exceeded 1048576 bytes") {
			t.Fatalf("FindPR() error = %v, want bounded-output error", err)
		}
	})
	t.Run("oversized stderr", func(t *testing.T) {
		const prefix = "useful prefix: "
		const partialSecret = "secret"
		stderr := prefix + strings.Repeat("x", maxForgejoErrorOutputBytes-len(prefix)-len(partialSecret)) + "secret-token"
		stderrFile := filepath.Join(t.TempDir(), "stderr.txt")
		if err := os.WriteFile(stderrFile, []byte(stderr), 0o600); err != nil {
			t.Fatal(err)
		}
		host := newTestHost(&fakeRecorder{responses: []fakeResponse{{stderrFile: stderrFile, code: 1}}})
		_, err := host.FindPR(context.Background(), "feature/forgejo", "main")
		if err == nil {
			t.Fatal("FindPR() error = nil, want bounded stderr prefix")
		}
		if message := err.Error(); !strings.Contains(message, prefix) || strings.Contains(message, partialSecret) || len(message) > maxForgejoErrorOutputBytes+256 {
			t.Fatalf("FindPR() error length = %d, want bounded stderr prefix", len(message))
		}
	})
}

func TestCommandHonorsCancellationAndTimeout(t *testing.T) {
	for _, tt := range []struct {
		name    string
		context func() (context.Context, context.CancelFunc)
		want    error
	}{
		{
			name: "cancel",
			context: func() (context.Context, context.CancelFunc) {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx, cancel
			},
			want: context.Canceled,
		},
		{
			name: "deadline",
			context: func() (context.Context, context.CancelFunc) {
				return context.WithTimeout(context.Background(), 20*time.Millisecond)
			},
			want: context.DeadlineExceeded,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			recorder := &fakeRecorder{responses: []fakeResponse{{sleep: 2 * time.Second}}}
			host := newTestHost(recorder)
			ctx, cancel := tt.context()
			defer cancel()
			_, err := host.FindPR(ctx, "feature/forgejo", "main")
			if !errors.Is(err, tt.want) {
				t.Fatalf("FindPR() error = %v, want %v", err, tt.want)
			}
		})
	}
}

func testPR() *scm.PR {
	return &scm.PR{Number: "42", URL: testPRURL, HeadSHA: testHeadSHA}
}

func pullJSON(state string, merged bool, sha string) string {
	return fmt.Sprintf(`{
		"number":42,"url":%q,"api_url":%q,"state":%q,"draft":false,"title":"Forgejo support",
		"head":"feature/forgejo","base":"main","head_sha":%q,"mergeable":true,"merged":%t,
		"merge_commit_sha":null,"merged_at":null,"merged_by":null
	}`, testPRURL, testBaseURL+"/api/v1/repos/octo/widgets/pulls/42", state, sha, merged)
}

func checksJSON(overall, requiredState string, passes bool, statuses, required string) string {
	var decoded []json.RawMessage
	if err := json.Unmarshal([]byte(statuses), &decoded); err != nil {
		panic(fmt.Sprintf("invalid test statuses: %v", err))
	}
	reported := len(decoded)
	return fmt.Sprintf(`{
		"checks":{"sha":%q,"reported":%d,"state":%q,"statuses":%s,
		"required":%s,"required_state":%q,"passes":%t,
		"protection":{"protected":true,"rule":"main","status_checks_enabled":true}}
	}`, testHeadSHA, reported, overall, statuses, required, requiredState, passes)
}

func fixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

type fakeResponse struct {
	stdout      string
	stdoutFile  string
	stdoutBytes int
	stderr      string
	stderrFile  string
	code        int
	sleep       time.Duration
}

type fakeCall struct {
	name string
	args []string
}

type fakeRecorder struct {
	calls     []fakeCall
	responses []fakeResponse
}

func (r *fakeRecorder) factory(ctx context.Context, name string, args ...string) *exec.Cmd {
	r.calls = append(r.calls, fakeCall{name: name, args: append([]string(nil), args...)})
	response := fakeResponse{stderr: "unexpected forgejo-axi command", code: 1}
	if len(r.responses) > 0 {
		response = r.responses[0]
		r.responses = r.responses[1:]
	}
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestForgejoAXIHelperProcess", "--")
	cmd.Env = append(os.Environ(),
		"FORGEJO_TEST_HELPER=1",
		"FORGEJO_TEST_STDOUT="+response.stdout,
		"FORGEJO_TEST_STDOUT_FILE="+response.stdoutFile,
		fmt.Sprintf("FORGEJO_TEST_STDOUT_BYTES=%d", response.stdoutBytes),
		"FORGEJO_TEST_STDERR="+response.stderr,
		"FORGEJO_TEST_STDERR_FILE="+response.stderrFile,
		fmt.Sprintf("FORGEJO_TEST_EXIT_CODE=%d", response.code),
		fmt.Sprintf("FORGEJO_TEST_SLEEP=%d", response.sleep.Milliseconds()),
	)
	return cmd
}

func TestForgejoAXIHelperProcess(t *testing.T) {
	if os.Getenv("FORGEJO_TEST_HELPER") != "1" {
		return
	}
	if raw := os.Getenv("FORGEJO_TEST_SLEEP"); raw != "" && raw != "0" {
		var millis int
		_, _ = fmt.Sscanf(raw, "%d", &millis)
		time.Sleep(time.Duration(millis) * time.Millisecond)
	}
	if raw := os.Getenv("FORGEJO_TEST_STDOUT_BYTES"); raw != "" && raw != "0" {
		var count int
		_, _ = fmt.Sscanf(raw, "%d", &count)
		chunk := []byte(strings.Repeat("x", 4*1024))
		for count > 0 {
			write := len(chunk)
			if count < write {
				write = count
			}
			if _, err := os.Stdout.Write(chunk[:write]); err != nil {
				break
			}
			count -= write
		}
	} else if path := os.Getenv("FORGEJO_TEST_STDOUT_FILE"); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			_, _ = fmt.Fprint(os.Stderr, err)
			os.Exit(1)
		}
		_, _ = os.Stdout.Write(data)
	} else {
		_, _ = fmt.Fprint(os.Stdout, os.Getenv("FORGEJO_TEST_STDOUT"))
	}
	if path := os.Getenv("FORGEJO_TEST_STDERR_FILE"); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			_, _ = fmt.Fprint(os.Stderr, err)
			os.Exit(1)
		}
		_, _ = os.Stderr.Write(data)
	} else {
		_, _ = fmt.Fprint(os.Stderr, os.Getenv("FORGEJO_TEST_STDERR"))
	}
	if os.Getenv("FORGEJO_TEST_EXIT_CODE") != "0" {
		os.Exit(1)
	}
	os.Exit(0)
}

func newTestHost(recorder *fakeRecorder) *Host {
	return newTestHostWithOptions(recorder, func(string) bool { return true })
}

func newTestHostWithOptions(recorder *fakeRecorder, available func(string) bool) *Host {
	return New(Options{
		CommandFactory: recorder.factory,
		CLIAvailable:   available,
		Executable:     "/opt/tools/forgejo-axi-custom",
		BaseURL:        testBaseURL,
		Repository:     testRepo,
		TokenEnv:       "FORGEJO_TEST_TOKEN",
		Secrets:        []string{"secret", "secret-token", "pass"},
	})
}

func TestMain(m *testing.M) {
	if runtime.GOOS == "windows" {
		// The helper-process fake remains portable; this branch documents that
		// no shell executable is involved in these contract tests.
	}
	os.Exit(m.Run())
}
