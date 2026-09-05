package firewall

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestStoreAndAPI_AxiShapeAndPublicRedaction(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(filepath.Join(dir, "firewall.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	in := Input{
		Diff:    unified("cfg.txt", []string{"bind 10.0.0.5"}),
		Repo:    "carverauto/serviceradar",
		PRURL:   "https://github.com/carverauto/serviceradar/pull/1",
		HeadSHA: "abc123",
		Branch:  "fm/example",
	}
	res := Scan(in)
	v, err := store.Insert(in, res, "http://portal.lan")
	if err != nil {
		t.Fatal(err)
	}
	if v.Conclusion != "failure" {
		t.Fatalf("conclusion=%s", v.Conclusion)
	}
	axi := v.AxiRun()
	if axi.ID != v.ID || axi.Branch != in.Branch || axi.Head != in.HeadSHA {
		t.Fatalf("axi run shape: %+v", axi)
	}
	if len(axi.Steps) != 1 || axi.Steps[0].Step != "publish-firewall" {
		t.Fatalf("steps=%+v", axi.Steps)
	}
	if len(axi.Steps[0].Items) == 0 || !strings.Contains(axi.Steps[0].Items[0].Description, "10.0.0.5") {
		t.Fatalf("LAN findings should keep the snippet: %+v", axi.Steps[0].Items)
	}

	srv := &Server{Store: store, PortalBase: "http://portal.lan"}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	status := getJSON(t, ts.URL+"/v1/axi/runs/"+v.ID)
	if status["run"] == nil {
		t.Fatalf("missing run: %v", status)
	}

	noticeBody := getRaw(t, ts.URL+"/v1/firewall/verdicts/"+v.ID+"/notice")
	if strings.Contains(noticeBody, "10.0.0.5") || strings.Contains(noticeBody, "cfg.txt") {
		t.Fatalf("notice leaked: %s", noticeBody)
	}
	if !strings.Contains(noticeBody, "publish-policy-violation") {
		t.Fatalf("notice: %s", noticeBody)
	}

	resp := postJSON(t, ts.URL+"/v1/axi/runs/"+v.ID+"/respond", map[string]string{"action": "acknowledge"})
	if resp["conclusion"] != "failure" {
		t.Fatalf("respond must not green the check: %v", resp)
	}
	got, err := store.Get(v.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Conclusion != "failure" {
		t.Fatal("conclusion changed after acknowledge")
	}
	if len(got.Responses) != 1 || got.Responses[0].Action != "acknowledge" {
		t.Fatalf("responses=%+v", got.Responses)
	}
}

func TestIngest_RecordsThePublishedVerdictWithoutRescanning(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(filepath.Join(dir, "firewall.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	srv := &Server{Store: store, PortalBase: "http://portal.lan", IngestTok: "secret"}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	// The runner published a clean check for this diff. Even though the diff
	// still carries a live address, the portal must not derive its own verdict.
	run := ingestAs(t, ts.URL, "secret", `{
		"repo":"carverauto/serviceradar",
		"branch":"fm/example",
		"head_sha":"abc",
		"findings":[],
		"diff":`+jsonString(unified("cfg.txt", []string{"bind 10.0.0.5"}))+`
	}`)
	if run["outcome"] != "passed" {
		t.Fatalf("portal contradicted the published clean check: %v", run)
	}
	if run["gate"] != nil {
		t.Fatalf("clean ingest must not open a gate: %v", run["gate"])
	}

	stored, err := store.List(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 1 || stored[0].Conclusion != "success" {
		t.Fatalf("stored verdict must match the published one: %+v", stored)
	}

	failing := ingestAs(t, ts.URL, "secret", `{
		"repo":"carverauto/serviceradar",
		"branch":"fm/example",
		"head_sha":"def",
		"findings":[{"id":"f1","severity":"error","file":"cfg.txt","class":"ip_address","action":"ask-user","description":"ip_address match"}]
	}`)
	if failing["outcome"] != "failed" {
		t.Fatalf("a published violation must be recorded as failed: %v", failing)
	}

	unauth, err := http.Post(ts.URL+"/v1/firewall/verdicts", "application/json", bytes.NewBufferString(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	unauth.Body.Close()
	if unauth.StatusCode != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", unauth.StatusCode)
	}
}

func ingestAs(t *testing.T, base, token, body string) map[string]any {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, base+"/v1/firewall/verdicts", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("ingest status=%d", res.StatusCode)
	}
	var out map[string]any
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestAbort_ChangesRenderedOutcomeAndClearsGate(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(filepath.Join(dir, "firewall.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	in := Input{Diff: unified("cfg.txt", []string{"bind 10.0.0.5"}), Branch: "fm/example", HeadSHA: "abc"}
	v, err := store.Insert(in, Scan(in), "http://portal.lan")
	if err != nil {
		t.Fatal(err)
	}
	if before := v.AxiRun(); before.Outcome != "failed" || before.Gate == nil {
		t.Fatalf("pre-abort run should be a failed gate: %+v", before)
	}

	srv := &Server{Store: store, PortalBase: "http://portal.lan"}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	resp := postJSON(t, ts.URL+"/v1/axi/runs/"+v.ID+"/abort", map[string]string{})
	run, ok := resp["run"].(map[string]any)
	if !ok {
		t.Fatalf("abort response: %v", resp)
	}
	if run["outcome"] != StatusCancelled {
		t.Fatalf("abort must change the rendered outcome, got %v", run["outcome"])
	}
	if run["gate"] != nil {
		t.Fatalf("aborted run must not still be awaiting approval: %v", run["gate"])
	}

	got, err := store.Get(v.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after := got.AxiRun(); after.Outcome != StatusCancelled || after.Gate != nil {
		t.Fatalf("cancelled status not durable in the rendered run: %+v", after)
	}
	if got.Conclusion != "failure" {
		t.Fatalf("abort must not change the GitHub conclusion, got %s", got.Conclusion)
	}
}

func TestMutatingRoutesRequireTheIngestToken(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(filepath.Join(dir, "firewall.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	in := Input{Diff: unified("cfg.txt", []string{"bind 10.0.0.5"}), Branch: "fm/example", HeadSHA: "abc"}
	v, err := store.Insert(in, Scan(in), "http://portal.lan")
	if err != nil {
		t.Fatal(err)
	}

	srv := &Server{Store: store, PortalBase: "http://portal.lan", IngestTok: "secret"}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	for _, path := range []string{"/respond", "/abort"} {
		res, err := http.Post(ts.URL+"/v1/axi/runs/"+v.ID+path, "application/json", bytes.NewBufferString(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s without a token: want 401, got %d", path, res.StatusCode)
		}
	}

	got, err := store.Get(v.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status == StatusCancelled {
		t.Fatal("unauthenticated abort cancelled the verdict")
	}
	if len(got.Responses) != 0 {
		t.Fatalf("unauthenticated respond recorded an acknowledgement: %+v", got.Responses)
	}
	if run := got.AxiRun(); run.Outcome != "failed" || run.Gate == nil {
		t.Fatalf("live violation must still read as a pending gate: %+v", run)
	}

	authed := postJSONAuthed(t, ts.URL+"/v1/axi/runs/"+v.ID+"/abort", "secret")
	run, ok := authed["run"].(map[string]any)
	if !ok || run["outcome"] != StatusCancelled {
		t.Fatalf("authorized abort should cancel: %v", authed)
	}
}

func postJSONAuthed(t *testing.T, url, token string) map[string]any {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewBufferString(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestDefaultListenIsLoopback(t *testing.T) {
	if !strings.HasPrefix(DefaultListen, "127.0.0.1:") {
		t.Fatalf("default listen must be loopback, got %s", DefaultListen)
	}
}

func getJSON(t *testing.T, url string) map[string]any {
	t.Helper()
	res, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func getRaw(t *testing.T, url string) string {
	t.Helper()
	res, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(res.Body)
	return buf.String()
}

func postJSON(t *testing.T, url string, body any) map[string]any {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.Post(url, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
