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

	publicBody := getRaw(t, ts.URL+"/v1/firewall/verdicts/"+v.ID+"/public")
	if strings.Contains(publicBody, "10.0.0.5") || strings.Contains(publicBody, "cfg.txt") {
		t.Fatalf("public payload leaked: %s", publicBody)
	}
	if !strings.Contains(publicBody, `"conclusion":"failure"`) {
		t.Fatalf("public payload: %s", publicBody)
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

func TestIngest_ScansWhenFindingsOmitted(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(filepath.Join(dir, "firewall.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	srv := &Server{Store: store, PortalBase: "http://portal.lan", IngestTok: "secret"}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/firewall/verdicts", bytes.NewBufferString(`{
		"repo":"carverauto/serviceradar",
		"branch":"fm/example",
		"head_sha":"abc",
		"diff":`+jsonString(unified("cfg.txt", []string{"bind 10.0.0.5"}))+`
	}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("status=%d", res.StatusCode)
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
