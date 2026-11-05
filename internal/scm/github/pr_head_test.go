package github

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func TestVerifyPRHead(t *testing.T) {
	t.Parallel()
	for _, fork := range []string{"", "fork/renamed"} {
		for _, fault := range []string{"", "sha", "missing_sha", "branch", "owner", "owner_and_sha", "source", "repository", "url", "number", "closed", "merged", "missing_state", "missing_merged", "null", "malformed", "lookup"} {
			t.Run(fork+"/"+fault, func(t *testing.T) {
				t.Parallel()
				head := strings.Repeat("a", 40)
				source := "test/repo"
				if fork != "" {
					source = fork
				}
				repo := map[string]any{"full_name": source, "owner": map[string]any{"login": strings.Split(source, "/")[0]}}
				liveHead := map[string]any{"ref": "feature", "sha": head, "repo": repo}
				live := map[string]any{"number": 42, "html_url": "https://github.com/test/repo/pull/42", "state": "open", "merged": false, "base": map[string]any{"repo": map[string]any{"full_name": "test/repo"}}, "head": liveHead}
				switch fault {
				case "sha":
					liveHead["sha"] = strings.Repeat("b", 40)
				case "missing_sha":
					delete(liveHead, "sha")
				case "branch":
					liveHead["ref"] = "other"
				case "owner":
					repo["owner"] = map[string]any{"login": "other"}
				case "owner_and_sha":
					repo["owner"] = map[string]any{"login": "other"}
					liveHead["sha"] = strings.Repeat("b", 40)
				case "source":
					repo["full_name"] = "other/repo"
				case "repository":
					live["base"] = map[string]any{"repo": map[string]any{"full_name": "other/repo"}}
				case "url":
					live["html_url"] = "https://github.com/other/repo/pull/42"
				case "number":
					live["number"] = 99
				case "closed":
					live["state"] = "closed"
				case "merged":
					live["merged"] = true
				case "missing_state":
					delete(live, "state")
				case "missing_merged":
					delete(live, "merged")
				}
				raw, err := json.Marshal(live)
				if err != nil {
					t.Fatal(err)
				}
				if fault == "null" {
					raw = []byte("null")
				}
				if fault == "malformed" {
					raw = []byte("{")
				}
				response := githubTestResponse{stdout: string(raw)}
				if fault == "lookup" {
					response = githubTestResponse{code: 1, stderr: "unavailable"}
				}
				h := NewWithFork(githubTestCmdFactory(map[string]githubTestResponse{"gh api --hostname github.com repos/test/repo/pulls/42": response}), func() bool { return true }, "github.com", "test/repo", fork, false)
				err = h.VerifyPRHead(context.Background(), &scm.PR{Number: "42", URL: "https://github.com/test/repo/pull/42"}, "feature", head)
				if (err != nil) != (fault != "") {
					t.Fatalf("fault %q: %v", fault, err)
				}
				var changed *scm.PRHeadChangedError
				if errors.As(err, &changed) != (fault == "sha") {
					t.Fatalf("only a valid identity with a different SHA may be retried: %v", err)
				}
				if fault == "sha" && (changed.HeadSHA != strings.Repeat("b", 40) || !errors.Is(err, scm.ErrHeadChanged)) {
					t.Fatalf("missing observed head or mismatch classification: %v", err)
				}
			})
		}
	}
}

func TestVerifyPRHeadRefusesUnboundIdentity(t *testing.T) {
	t.Parallel()
	h := New(githubTestCmdFactory(nil), func() bool { return true }, "github.com", "test/repo")
	for _, pr := range []*scm.PR{nil, {}, {Number: "42"}, {Number: "99", URL: "https://github.com/test/repo/pull/42"}, {Number: "42", URL: "https://github.com/other/repo/pull/42"}} {
		if err := h.VerifyPRHead(context.Background(), pr, "feature", strings.Repeat("a", 40)); err == nil {
			t.Fatalf("accepted unbound identity: %+v", pr)
		}
	}
}
