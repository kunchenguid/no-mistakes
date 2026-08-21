package forgejo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestChecksPreserveProviderStateAndTargetLink(t *testing.T) {
	target := nativeActionsTarget(91, 0)
	statuses := fmt.Sprintf(`[{"context":"CI / test (pull_request)","state":"failure","description":"boom","target_url":%q,"created_at":null,"updated_at":null}]`, target)
	host := newTestHost(&fakeRecorder{responses: []fakeResponse{{stdout: checksJSON("failure", "not_required", false, statuses, `[]`)}}})

	checks, err := host.GetChecks(context.Background(), testPR())
	if err != nil {
		t.Fatalf("GetChecks() error = %v", err)
	}
	if len(checks) != 1 || checks[0].State != "failure" || checks[0].Link != target {
		t.Fatalf("GetChecks() = %+v, want provider failure state and exact target link", checks)
	}
}

func TestFetchFailedCheckLogsUsesCanonicalTargetAndExactIdentities(t *testing.T) {
	target := nativeActionsTarget(7, 1)
	statuses := fmt.Sprintf(`[{"context":"CI / test (pull_request)","state":"failure","target_url":%q}]`, target)
	recorder := &fakeRecorder{responses: []fakeResponse{
		{stdout: fixture(t, "status-forgejo-16.json")},
		{stdout: checksJSON("failure", "not_required", false, statuses, `[]`)},
		{stdout: failedLogRunListJSON(testHeadSHA, testActionRun{id: 91, number: 7})},
		{stdout: failedLogRunViewJSON(91, 7, testHeadSHA, []string{
			`{"id":900,"run_id":91,"name":"unrelated","status":"failure","log":"unrelated failed"}`,
			`{"id":501,"run_id":91,"name":"test","status":"failure","log":"assertion failed"}`,
		})},
	}}
	host := newTestHost(recorder)
	if err := host.Available(context.Background()); err != nil {
		t.Fatalf("Available() error = %v", err)
	}

	logs, err := host.FetchFailedCheckLogs(context.Background(), testPR(), "feature/forgejo", testHeadSHA, []string{"CI / test (pull_request)"})
	if err != nil {
		t.Fatalf("FetchFailedCheckLogs() error = %v", err)
	}
	if !strings.Contains(logs, "Forgejo Actions run 7, job test:") || !strings.Contains(logs, "assertion failed") || strings.Contains(logs, "unrelated") {
		t.Fatalf("FetchFailedCheckLogs() = %q, want only the targeted failed-job log", logs)
	}
	wantList := []string{"run", "list", "--repo", testRepo, "--fields", "all", "--base-url", testBaseURL, "--token-env", "FORGEJO_TEST_TOKEN", "--json"}
	if got := recorder.calls[2].args; !reflect.DeepEqual(got, wantList) {
		t.Fatalf("run list args = %#v, want %#v", got, wantList)
	}
	want := []string{"run", "view", "--repo", testRepo, "91", "--log-failed", "--base-url", testBaseURL, "--token-env", "FORGEJO_TEST_TOKEN", "--json"}
	if got := recorder.calls[3].args; !reflect.DeepEqual(got, want) {
		t.Fatalf("run view args = %#v, want %#v", got, want)
	}
}

func TestFetchFailedCheckLogsDeduplicatesAndSortsRunAndJobIDs(t *testing.T) {
	statuses := fmt.Sprintf(`[
		{"context":"CI / second (pull_request)","state":"failure","target_url":%q},
		{"context":"Lint / lint (pull_request)","state":"failure","target_url":%q},
		{"context":"CI / first (pull_request)","state":"failure","target_url":%q}
	]`, nativeActionsTarget(20, 1), nativeActionsTarget(10, 0), nativeActionsTarget(20, 0))
	recorder := &fakeRecorder{responses: []fakeResponse{
		{stdout: fixture(t, "status-forgejo-16.json")},
		{stdout: checksJSON("failure", "not_required", false, statuses, `[]`)},
		{stdout: failedLogRunListJSON(testHeadSHA, testActionRun{id: 10, number: 10}, testActionRun{id: 20, number: 20})},
		{stdout: failedLogRunViewJSON(10, 10, testHeadSHA, []string{`{"id":100,"run_id":10,"name":"lint","status":"failure","log":"lint failed"}`})},
		{stdout: failedLogRunViewJSON(20, 20, testHeadSHA, []string{
			`{"id":201,"run_id":20,"name":"test-second","status":"failure","log":"second failed"}`,
			`{"id":200,"run_id":20,"name":"test-first","status":"failure","log":"first failed"}`,
		})},
	}}
	host := newTestHost(recorder)
	if err := host.Available(context.Background()); err != nil {
		t.Fatalf("Available() error = %v", err)
	}

	logs, err := host.FetchFailedCheckLogs(context.Background(), testPR(), "feature/forgejo", testHeadSHA, []string{
		"CI / first (pull_request)", "CI / second (pull_request)", "Lint / lint (pull_request)",
	})
	if err != nil {
		t.Fatalf("FetchFailedCheckLogs() error = %v", err)
	}
	run10, run20 := strings.Index(logs, "run 10"), strings.Index(logs, "run 20")
	firstJob, secondJob := strings.Index(logs, "job test-first:"), strings.Index(logs, "job test-second:")
	if run10 < 0 || run20 < 0 || run10 > run20 || firstJob < 0 || secondJob < 0 || firstJob > secondJob {
		t.Fatalf("FetchFailedCheckLogs() = %q, want numeric run and job order", logs)
	}
	if len(recorder.calls) != 5 || recorder.calls[3].args[4] != "10" || recorder.calls[4].args[4] != "20" {
		t.Fatalf("run view calls = %#v, want one call each in 10,20 order", recorder.calls)
	}
}

func TestFetchFailedCheckLogsRejectsRunHeadAndJobMismatches(t *testing.T) {
	canonical := failedLogRunViewJSON(91, 91, testHeadSHA, []string{`{"id":501,"run_id":91,"name":"test","status":"failure","log":"failed"}`})
	tests := []struct {
		name     string
		jobIndex int
		view     string
		want     string
	}{
		{name: "wrong run", view: failedLogRunViewJSON(92, 91, testHeadSHA, []string{`{"id":501,"run_id":92,"name":"test","status":"failure","log":"failed"}`}), want: "run 92, expected 91"},
		{name: "wrong run number", view: failedLogRunViewJSON(91, 7, testHeadSHA, []string{`{"id":501,"run_id":91,"name":"test","status":"failure","log":"failed"}`}), want: "with number 7, expected 91"},
		{name: "wrong head", view: failedLogRunViewJSON(91, 91, strings.Repeat("b", 40), []string{`{"id":501,"run_id":91,"name":"test","status":"failure","log":"failed"}`}), want: "pull request head changed"},
		{name: "wrong job run", view: strings.Replace(canonical, `"run_id":91`, `"run_id":92`, 1), want: "job 501 for run 92"},
		{name: "empty job name", view: strings.Replace(canonical, `"name":"test"`, `"name":" "`, 1), want: "job 501 without a name"},
		{name: "duplicate job ID", view: failedLogRunViewJSON(91, 91, testHeadSHA, []string{
			`{"id":501,"run_id":91,"name":"first","status":"failure","log":"failed"}`,
			`{"id":501,"run_id":91,"name":"second","status":"failure","log":"failed"}`,
		}), want: "duplicate job 501"},
		{name: "missing target job", jobIndex: 999, view: canonical, want: "target job index 999 is absent"},
		{name: "target job did not fail", view: failedLogRunViewJSON(91, 91, testHeadSHA, []string{`{"id":501,"run_id":91,"name":"test","status":"success"}`}), want: `status "success", expected failure`},
		{name: "failed job omitted log", view: failedLogRunViewJSON(91, 91, testHeadSHA, []string{`{"id":501,"run_id":91,"name":"test","status":"failure"}`}), want: "without a log"},
		{name: "unsupported next", view: strings.TrimSuffix(canonical, "}") + `,"next":["Job logs are unsupported"]}`, want: "did not provide requested failed logs"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target := nativeActionsTarget(91, tt.jobIndex)
			statuses := fmt.Sprintf(`[{"context":"CI / test (pull_request)","state":"failure","target_url":%q}]`, target)
			recorder := &fakeRecorder{responses: []fakeResponse{
				{stdout: fixture(t, "status-forgejo-16.json")},
				{stdout: checksJSON("failure", "not_required", false, statuses, `[]`)},
				{stdout: failedLogRunListJSON(testHeadSHA, testActionRun{id: 91, number: 91})},
				{stdout: tt.view},
			}}
			host := newTestHost(recorder)
			if err := host.Available(context.Background()); err != nil {
				t.Fatalf("Available() error = %v", err)
			}
			logs, err := host.FetchFailedCheckLogs(context.Background(), testPR(), "feature/forgejo", testHeadSHA, []string{"CI / test (pull_request)"})
			if logs != "" || err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("FetchFailedCheckLogs() = (%q, %v), want error containing %q", logs, err, tt.want)
			}
		})
	}
}

func TestFetchFailedCheckLogsUsesLiveCheckHeadForRunLookup(t *testing.T) {
	target := nativeActionsTarget(7, 0)
	statuses := fmt.Sprintf(`[{"context":"CI / test (pull_request)","state":"failure","target_url":%q}]`, target)
	recorder := &fakeRecorder{responses: []fakeResponse{
		{stdout: fixture(t, "status-forgejo-16.json")},
		{stdout: checksJSON("failure", "not_required", false, statuses, `[]`)},
		{stdout: failedLogRunListJSON(testHeadSHA, testActionRun{id: 91, number: 7})},
		{stdout: failedLogRunViewJSON(91, 7, testHeadSHA, []string{`{"id":501,"run_id":91,"name":"test","status":"failure","log":"failed"}`})},
	}}
	host := newTestHost(recorder)
	if err := host.Available(context.Background()); err != nil {
		t.Fatalf("Available() error = %v", err)
	}
	logs, err := host.FetchFailedCheckLogs(context.Background(), testPR(), "feature/forgejo", strings.Repeat("b", 40), []string{"CI / test (pull_request)"})
	if err != nil || !strings.Contains(logs, "failed") || len(recorder.calls) != 4 {
		t.Fatalf("FetchFailedCheckLogs() = (%q, %v) with %d calls, want live-head log lookup", logs, err, len(recorder.calls))
	}
}

func TestFetchFailedCheckLogsRejectsUnprovenRunResolution(t *testing.T) {
	target := nativeActionsTarget(7, 0)
	statuses := fmt.Sprintf(`[{"context":"CI / test (pull_request)","state":"failure","target_url":%q}]`, target)
	valid := failedLogRunListJSON(testHeadSHA, testActionRun{id: 91, number: 7})
	tests := []struct {
		name string
		list string
		want string
	}{
		{name: "incomplete list", list: strings.Replace(valid, `"complete":true`, `"complete":false`, 1), want: "complete run identity set"},
		{name: "missing live head", list: failedLogRunListJSON(strings.Repeat("b", 40), testActionRun{id: 91, number: 7}), want: "did not return run number 7"},
		{name: "ambiguous run", list: failedLogRunListJSON(testHeadSHA, testActionRun{id: 91, number: 7}, testActionRun{id: 92, number: 7}), want: "multiple runs numbered 7"},
		{name: "wrong run URL", list: strings.Replace(valid, "/actions/runs/91", "/actions/runs/7", 1), want: "run identity mismatch"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := &fakeRecorder{responses: []fakeResponse{
				{stdout: fixture(t, "status-forgejo-16.json")},
				{stdout: checksJSON("failure", "not_required", false, statuses, `[]`)},
				{stdout: tt.list},
			}}
			host := newTestHost(recorder)
			if err := host.Available(context.Background()); err != nil {
				t.Fatalf("Available() error = %v", err)
			}
			logs, err := host.FetchFailedCheckLogs(context.Background(), testPR(), "feature/forgejo", testHeadSHA, []string{"CI / test (pull_request)"})
			if logs != "" || err == nil || !strings.Contains(err.Error(), tt.want) || len(recorder.calls) != 3 {
				t.Fatalf("FetchFailedCheckLogs() = (%q, %v) with %d calls, want error containing %q", logs, err, len(recorder.calls), tt.want)
			}
		})
	}
}

func TestFetchFailedCheckLogsRejectsInvalidFreshChecksBeforeRunLookup(t *testing.T) {
	target := nativeActionsTarget(91, 0)
	statuses := fmt.Sprintf(`[{"context":"CI / test (pull_request)","state":"failure","target_url":%q}]`, target)
	checks := strings.Replace(checksJSON("failure", "not_required", false, statuses, `[]`), testHeadSHA, "", 1)
	recorder := &fakeRecorder{responses: []fakeResponse{
		{stdout: fixture(t, "status-forgejo-16.json")},
		{stdout: checks},
	}}
	host := newTestHost(recorder)
	if err := host.Available(context.Background()); err != nil {
		t.Fatalf("Available() error = %v", err)
	}
	logs, err := host.FetchFailedCheckLogs(context.Background(), testPR(), "feature/forgejo", testHeadSHA, []string{"CI / test (pull_request)"})
	if logs != "" || err == nil || !strings.Contains(err.Error(), "checks without a head SHA") || len(recorder.calls) != 2 {
		t.Fatalf("FetchFailedCheckLogs() = (%q, %v) with %d calls, want fresh-check validation error", logs, err, len(recorder.calls))
	}
}

func TestFetchFailedCheckLogsBoundsOutputAndHonorsCancellation(t *testing.T) {
	target := nativeActionsTarget(91, 0)
	statuses := fmt.Sprintf(`[{"context":"CI / test (pull_request)","state":"failure","target_url":%q}]`, target)

	t.Run("stdout limit", func(t *testing.T) {
		recorder := &fakeRecorder{responses: []fakeResponse{
			{stdout: fixture(t, "status-forgejo-16.json")},
			{stdout: checksJSON("failure", "not_required", false, statuses, `[]`)},
			{stdoutBytes: maxForgejoOutputBytes + 128*1024},
		}}
		host := newTestHost(recorder)
		if err := host.Available(context.Background()); err != nil {
			t.Fatalf("Available() error = %v", err)
		}
		logs, err := host.FetchFailedCheckLogs(context.Background(), testPR(), "feature/forgejo", testHeadSHA, []string{"CI / test (pull_request)"})
		if logs != "" || err == nil || !strings.Contains(err.Error(), "exceeded 1048576 bytes") {
			t.Fatalf("FetchFailedCheckLogs() = (%q, %v), want bounded-output error", logs, err)
		}
	})

	t.Run("aggregate log limit", func(t *testing.T) {
		largeLog := strings.Repeat("x", maxForgejoOutputBytes/2)
		responseFile := func(name, response string) fakeResponse {
			path := filepath.Join(t.TempDir(), name)
			if err := os.WriteFile(path, []byte(response), 0o600); err != nil {
				t.Fatal(err)
			}
			return fakeResponse{stdoutFile: path}
		}
		statuses := fmt.Sprintf(`[
			{"context":"CI / first (pull_request)","state":"failure","target_url":%q},
			{"context":"CI / second (pull_request)","state":"failure","target_url":%q}
		]`, nativeActionsTarget(91, 0), nativeActionsTarget(92, 0))
		recorder := &fakeRecorder{responses: []fakeResponse{
			{stdout: fixture(t, "status-forgejo-16.json")},
			{stdout: checksJSON("failure", "not_required", false, statuses, `[]`)},
			{stdout: failedLogRunListJSON(testHeadSHA, testActionRun{id: 91, number: 91}, testActionRun{id: 92, number: 92})},
			responseFile("run-91.json", failedLogRunViewJSON(91, 91, testHeadSHA, []string{fmt.Sprintf(`{"id":501,"run_id":91,"name":"first","status":"failure","log":%q}`, largeLog)})),
			responseFile("run-92.json", failedLogRunViewJSON(92, 92, testHeadSHA, []string{fmt.Sprintf(`{"id":502,"run_id":92,"name":"second","status":"failure","log":%q}`, largeLog)})),
		}}
		host := newTestHost(recorder)
		if err := host.Available(context.Background()); err != nil {
			t.Fatalf("Available() error = %v", err)
		}
		logs, err := host.FetchFailedCheckLogs(context.Background(), testPR(), "feature/forgejo", testHeadSHA, []string{
			"CI / first (pull_request)", "CI / second (pull_request)",
		})
		if logs != "" || err == nil || !strings.Contains(err.Error(), "failed check logs exceeded 1048576 bytes") {
			t.Fatalf("FetchFailedCheckLogs() = (%d bytes, %v), want aggregate-limit error", len(logs), err)
		}
	})

	t.Run("context cancellation", func(t *testing.T) {
		recorder := &fakeRecorder{responses: []fakeResponse{
			{stdout: fixture(t, "status-forgejo-16.json")},
			{stdout: checksJSON("failure", "not_required", false, statuses, `[]`)},
			{sleep: 2 * time.Second},
		}}
		host := newTestHost(recorder)
		if err := host.Available(context.Background()); err != nil {
			t.Fatalf("Available() error = %v", err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		_, err := host.FetchFailedCheckLogs(ctx, testPR(), "feature/forgejo", testHeadSHA, []string{"CI / test (pull_request)"})
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("FetchFailedCheckLogs() error = %v, want deadline exceeded", err)
		}
	})
}

func TestFetchFailedCheckLogsRedactsCommandErrors(t *testing.T) {
	target := nativeActionsTarget(91, 0)
	statuses := fmt.Sprintf(`[{"context":"CI / test (pull_request)","state":"failure","target_url":%q}]`, target)
	recorder := &fakeRecorder{responses: []fakeResponse{
		{stdout: fixture(t, "status-forgejo-16.json")},
		{stdout: checksJSON("failure", "not_required", false, statuses, `[]`)},
		{stdout: `{"error":"log failed with secret-token","code":"LOG_ERROR","details":{"url":"https://user:pass@forge.example/log?token=secret-token"}}`, code: 1},
	}}
	host := newTestHost(recorder)
	if err := host.Available(context.Background()); err != nil {
		t.Fatalf("Available() error = %v", err)
	}
	_, err := host.FetchFailedCheckLogs(context.Background(), testPR(), "feature/forgejo", testHeadSHA, []string{"CI / test (pull_request)"})
	if err == nil || !strings.Contains(err.Error(), "LOG_ERROR") || strings.Contains(err.Error(), "secret-token") || strings.Contains(err.Error(), "user:pass") {
		t.Fatalf("FetchFailedCheckLogs() error = %v, want code with secrets redacted", err)
	}
}

func TestFetchFailedCheckLogsRejectsNonCanonicalTargetsWithoutGuessing(t *testing.T) {
	tests := []struct {
		name   string
		target string
	}{
		{name: "external host", target: "https://ci.example/actions/runs/91/jobs/0"},
		{name: "absolute target", target: testBaseURL + nativeActionsTarget(91, 0)},
		{name: "wrong repository", target: "/other/widgets/actions/runs/91/jobs/0"},
		{name: "wrong base prefix", target: "/git/" + testRepo + "/actions/runs/91/jobs/0"},
		{name: "missing target", target: ""},
		{name: "zero run", target: "/" + testRepo + "/actions/runs/0/jobs/0"},
		{name: "leading-zero run", target: "/" + testRepo + "/actions/runs/091/jobs/0"},
		{name: "signed run", target: "/" + testRepo + "/actions/runs/+91/jobs/0"},
		{name: "leading-zero job", target: "/" + testRepo + "/actions/runs/91/jobs/00"},
		{name: "signed job", target: "/" + testRepo + "/actions/runs/91/jobs/+0"},
		{name: "malformed job index", target: "/" + testRepo + "/actions/runs/91/jobs/latest"},
		{name: "whitespace padded", target: " " + nativeActionsTarget(91, 0) + " "},
		{name: "empty fragment ambiguity", target: nativeActionsTarget(91, 0) + "#"},
		{name: "encoded path", target: "/octo/%77idgets/actions/runs/91/jobs/0"},
		{name: "empty query ambiguity", target: nativeActionsTarget(91, 0) + "?"},
		{name: "query ambiguity", target: nativeActionsTarget(91, 0) + "?attempt=2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var target any
			if tt.target != "" {
				target = tt.target
			}
			statuses, err := json.Marshal([]map[string]any{{
				"context": "CI / test (pull_request)", "state": "failure", "target_url": target,
			}})
			if err != nil {
				t.Fatal(err)
			}
			recorder := &fakeRecorder{responses: []fakeResponse{
				{stdout: fixture(t, "status-forgejo-16.json")},
				{stdout: checksJSON("failure", "not_required", false, string(statuses), `[]`)},
			}}
			host := newTestHost(recorder)
			if err := host.Available(context.Background()); err != nil {
				t.Fatalf("Available() error = %v", err)
			}
			logs, err := host.FetchFailedCheckLogs(context.Background(), testPR(), "feature/forgejo", testHeadSHA, []string{"CI / test (pull_request)"})
			if err != nil || logs != "" {
				t.Fatalf("FetchFailedCheckLogs() = (%q, %v), want unavailable without error", logs, err)
			}
			if len(recorder.calls) != 2 {
				t.Fatalf("calls = %d, want status + checks only; target must not trigger run guessing", len(recorder.calls))
			}
		})
	}
}

func nativeActionsTarget(runNumber, jobIndex int) string {
	return fmt.Sprintf("/%s/actions/runs/%d/jobs/%d", testRepo, runNumber, jobIndex)
}

type testActionRun struct {
	id     int
	number int
}

func failedLogRunListJSON(headSHA string, runs ...testActionRun) string {
	identities := make([]string, 0, len(runs))
	for _, run := range runs {
		identities = append(identities, fmt.Sprintf(`{"id":%d,"url":%q,"api_url":%q,"head_sha":%q,"run_number":%d}`,
			run.id,
			testBaseURL+"/"+testRepo+"/actions/runs/"+strconv.Itoa(run.id),
			testBaseURL+"/api/v1/repos/"+testRepo+"/actions/runs/"+strconv.Itoa(run.id),
			headSHA,
			run.number,
		))
	}
	return fmt.Sprintf(`{"runs":[%s],"page_info":{"complete":true}}`, strings.Join(identities, ","))
}

func failedLogRunViewJSON(runID, runNumber int, headSHA string, jobs []string) string {
	return fmt.Sprintf(`{
		"run":{"id":%d,"url":%q,"api_url":%q,"title":"CI","event":"pull_request","branch":"feature/forgejo","head_sha":%q,"run_number":%d,"status":"failure","started_at":null,"completed_at":"2026-08-06T00:01:00Z"},
		"jobs":[%s]
	}`, runID, testBaseURL+"/"+testRepo+"/actions/runs/"+strconv.Itoa(runID), testBaseURL+"/api/v1/repos/"+testRepo+"/actions/runs/"+strconv.Itoa(runID), headSHA, runNumber, strings.Join(jobs, ","))
}
