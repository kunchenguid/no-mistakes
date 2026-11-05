package steps

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/branchsync"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
)

// The oracle is issue #1433's successful-push contract. Real commits and the
// durable SQLite push binding are independent of the synthetic forge response.
func TestPRStep_ExactPushedHeadPostcondition(t *testing.T) {
	for _, route := range []string{"create", "update", "persisted"} {
		for _, fault := range []string{"", "wrong_sha", "moved_head", "missing_sha", "wrong_repository", "wrong_source_repository", "wrong_owner", "wrong_branch", "closed", "merged", "unreadable", "lookup_failed", "missing_push", "wrong_push_target", "wrong_push_ref"} {
			t.Run(route+"/"+fault, func(t *testing.T) {
				t.Parallel()
				dir, base, head := setupGitRepo(t)
				sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
				number := 42
				url := "https://github.com/test/repo/pull/42"
				discovered := url
				if route == "create" {
					discovered, number, url = "", 99, "https://github.com/test/repo/pull/99"
				}
				if route == "persisted" {
					sctx.Run.PRURL = &url
					if err := sctx.DB.UpdateRunPRURL(sctx.Run.ID, url); err != nil {
						t.Fatal(err)
					}
					discovered = ""
				}
				env, _ := fakeGH(t, discovered)
				proof := map[string]any{
					"number": number, "html_url": url, "state": "open", "merged": false,
					"base": map[string]any{"repo": map[string]any{"full_name": "test/repo"}},
					"head": map[string]any{"ref": "feature", "sha": head, "repo": map[string]any{"full_name": "test/repo", "owner": map[string]any{"login": "test"}}},
				}
				prHead := proof["head"].(map[string]any)
				switch fault {
				case "wrong_sha", "moved_head":
					prHead["sha"] = base
				case "missing_sha":
					delete(prHead, "sha")
				case "wrong_repository":
					proof["base"] = map[string]any{"repo": map[string]any{"full_name": "other/repo"}}
				case "wrong_source_repository":
					prHead["repo"].(map[string]any)["full_name"] = "test/other"
				case "wrong_owner":
					prHead["repo"].(map[string]any)["owner"] = map[string]any{"login": "other"}
				case "wrong_branch":
					prHead["ref"] = "other"
				case "closed":
					proof["state"] = "closed"
				case "merged":
					proof["merged"] = true
				}
				raw, err := json.Marshal(proof)
				if err != nil {
					t.Fatal(err)
				}
				if fault == "unreadable" {
					raw = []byte("{")
				}
				env = append(env, "FAKE_CLI_PR_PROOF_JSON="+string(raw))
				if fault == "lookup_failed" {
					env = append(env, "FAKE_CLI_PR_PROOF_FAIL=1")
				}
				if fault == "moved_head" {
					env = append(env, "FAKE_CLI_PR_PROOF_AFTER_UPDATE=1")
				}
				sctx.Env = env
				binding := db.PushBinding{HeadSHA: head, TargetKind: "upstream", TargetFingerprint: branchsync.TargetFingerprint(resolvePushURL(sctx)), Ref: "refs/heads/feature"}
				if fault == "wrong_push_target" {
					binding.TargetFingerprint = strings.Repeat("0", 64)
				}
				if fault == "wrong_push_ref" {
					binding.Ref = "refs/heads/other"
				}
				if fault != "missing_push" {
					if err := sctx.DB.UpdateRunPublication(sctx.Run.ID, binding); err != nil {
						t.Fatal(err)
					}
				}
				var logs []string
				sctx.Log = func(line string) { logs = append(logs, line) }
				outcome, executeErr := (&PRStep{}).Execute(sctx)
				if fault == "" {
					if executeErr != nil || outcome == nil || outcome.PRURL != url {
						t.Fatalf("matching push: outcome=%+v err=%v", outcome, executeErr)
					}
				} else {
					if executeErr == nil {
						t.Fatalf("unproven %s PR succeeded: %+v", fault, outcome)
					}
					if outcome != nil && outcome.PRURL != "" {
						t.Fatalf("unverified PR outcome: %+v", outcome)
					}
					for _, line := range logs {
						if strings.Contains(line, "created pull request:") {
							t.Fatalf("reported unverified PR: %s", line)
						}
					}
				}
				stored, err := sctx.DB.GetRun(sctx.Run.ID)
				if err != nil {
					t.Fatal(err)
				}
				if fault != "" && route != "persisted" && stored.PRURL != nil && *stored.PRURL != "" {
					t.Fatalf("persisted unverified URL: %s", *stored.PRURL)
				}
				if fault != "missing_push" && (stored.LastPushedSHA == nil || *stored.LastPushedSHA != head || stored.PushRef == nil || *stored.PushRef != binding.Ref) {
					t.Fatalf("lost push/custody evidence: %+v", stored)
				}
				if fault == "" && (stored.PRURL == nil || *stored.PRURL != url) {
					t.Fatalf("verified URL not persisted: %+v", stored.PRURL)
				}
			})
		}
	}
}

// Keep os linked here while fixtures are migrated to explicit proof responses.
var _ = os.Environ
