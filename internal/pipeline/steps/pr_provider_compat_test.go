package steps

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func TestPRStep_GitLabKeepsLegacyCreateBehavior(t *testing.T) {
	for _, mode := range []string{config.PRAppendixFull, config.PRAppendixCollapsed, config.PRAppendixMinimal} {
		for _, fault := range []string{"", "empty_identity", "empty_owned_identity", "empty_closing_identity", "persistence", "owned_readback", "closing_readback"} {
			t.Run(mode+"/"+fault, func(t *testing.T) {
				t.Parallel()
				dir, base, head := setupGitRepo(t)
				ag := &mockAgent{name: "test"}
				sctx := newPRTestContext(t, ag, dir, base, head, config.Commands{})
				sctx.Repo.UpstreamURL = "https://gitlab.com/test/repo.git"
				sctx.Config.PR.Appendix = mode
				env, _, stateFile := fakeGlabWithMRState(t)
				missingIdentity := strings.HasPrefix(fault, "empty_")
				if missingIdentity {
					env = append(env, "FAKE_CLI_MR_CREATE_OUTPUT=Merge request created")
				}
				if fault == "owned_readback" || fault == "closing_readback" {
					env = append(env, `FAKE_CLI_MR_READBACK_JSON={"iid":99,"title":"Changed title","description":"Changed body"}`)
				}
				sctx.Env = env
				completeCleanReview(t, sctx)
				if fault == "owned_readback" || fault == "empty_owned_identity" {
					title, body := "Supplied title", "Author narrative.\n"
					sctx.Run.PRTitle, sctx.Run.PRBody = &title, &body
				}
				if fault == "closing_readback" || fault == "empty_closing_identity" {
					if err := sctx.DB.UpdateRunClosingIssueRefs(sctx.Run.ID, []string{"95"}); err != nil {
						t.Fatal(err)
					}
				}
				closedDB := false
				ag.runFn = func(context.Context, agent.RunOpts) (*agent.Result, error) {
					if fault == "persistence" {
						if err := sctx.DB.Close(); err != nil {
							t.Fatal(err)
						}
						closedDB = true
					}
					return &agent.Result{Output: json.RawMessage(`{"title":"fix: preserve provider publication","body":"## What Changed\n\nPreserve provider publication."}`)}, nil
				}
				var logs []string
				sctx.Log = func(line string) { logs = append(logs, line) }
				assertNotice := capturePRProofNotice(t, sctx, "gitlab")
				out, executeErr := (&PRStep{}).Execute(sctx)
				wantError := ""
				switch fault {
				case "empty_owned_identity":
					wantError = "create returned no readable PR identity"
				case "empty_closing_identity":
					wantError = "verify closing issues: created pull request identity is unavailable"
				case "owned_readback":
					wantError = "created PR body differs"
				case "closing_readback":
					wantError = "verify closing issues: pull request body is missing Closes #95"
				}
				if wantError != "" {
					if executeErr == nil || !strings.Contains(executeErr.Error(), wantError) || out != nil {
						t.Fatalf("expected %q: outcome=%+v err=%v", wantError, out, executeErr)
					}
				} else if executeErr != nil || out == nil || out.Skipped {
					t.Fatalf("legacy create failed: outcome=%+v err=%v", out, executeErr)
				}
				const url = "https://gitlab.com/test/repo/-/merge_requests/99"
				if wantError == "" {
					wantURL := url
					if missingIdentity {
						wantURL = ""
					}
					if out.PRURL != wantURL {
						t.Fatalf("outcome URL = %q, want %q", out.PRURL, wantURL)
					}
				}
				loggedCreation := strings.Contains(strings.Join(logs, "\n"), "created pull request: "+url)
				if loggedCreation == missingIdentity {
					t.Fatalf("creation log changed legacy ordering: %v", logs)
				}
				if fault == "persistence" {
					if !closedDB {
						t.Fatal("URL persistence was not made unavailable")
					}
				} else {
					stored, err := sctx.DB.GetRun(sctx.Run.ID)
					if err != nil {
						t.Fatal(err)
					}
					if missingIdentity {
						if stored.PRURL != nil {
							t.Fatalf("missing identity persisted a URL: %v", stored.PRURL)
						}
					} else if stored.PRURL == nil || *stored.PRURL != url {
						t.Fatalf("create URL was not persisted before readback verification: %v", stored.PRURL)
					}
					if stored.LastPushedSHA == nil || *stored.LastPushedSHA != head {
						t.Fatal("publication lost durable push evidence")
					}
				}
				data, err := os.ReadFile(stateFile)
				if err != nil {
					t.Fatal(err)
				}
				var published struct {
					Description string `json:"description"`
				}
				if err := json.Unmarshal(data, &published); err != nil {
					t.Fatal(err)
				}
				assertNotice(published.Description)
			})
		}
	}
}

func TestPRStep_AzureKeepsZeroIDLegacyURL(t *testing.T) {
	t.Parallel()
	dir, base, head := setupGitRepo(t)
	sctx := newPRTestContext(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
	sctx.Repo.UpstreamURL = "https://dev.azure.com/test/project/_git/repo"
	binDir := fakeCLIBinDir(t)
	linkTestBinary(t, binDir, "az")
	bodyFile := filepath.Join(t.TempDir(), "azure-created-body.md")
	sctx.Env = fakeCLIEnv(binDir, map[string]string{
		"FAKE_CLI_MODE":         "az-empty-pr",
		"FAKE_CLI_PR_BODY_FILE": bodyFile,
	})
	assertNotice := capturePRProofNotice(t, sctx, string(scm.ProviderAzureDevOps))
	const wantURL = "https://dev.azure.com/test/project/_git/repo"
	out, err := (&PRStep{}).Execute(sctx)
	if err != nil || out == nil || out.Skipped || out.PRURL != wantURL {
		t.Fatalf("zero-ID Azure create changed legacy result: %+v %v", out, err)
	}
	body, err := os.ReadFile(bodyFile)
	if err != nil {
		t.Fatal(err)
	}
	assertNotice(string(body))
	stored, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil || stored == nil || stored.PRURL == nil || *stored.PRURL != wantURL {
		t.Fatalf("zero-ID Azure create did not persist the legacy URL: %+v %v", stored, err)
	}
}
