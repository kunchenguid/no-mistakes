package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
	toon "github.com/toon-format/toon-go"
)

func TestReviewedRecoveryCLIRequiresDisplayedDigestAndPreservesOriginalCaller(t *testing.T) {
	f := newCLIRecoverFixture(t)
	p, _ := paths.New()
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	pipeline := filepath.Join(filepath.Dir(f.local), "pipeline")
	cliGit(t, pipeline, "checkout", "--orphan", "reviewed-rewrite")
	if err := os.WriteFile(filepath.Join(pipeline, "file.txt"), []byte("reviewed replacement\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cliGit(t, pipeline, "commit", "-am", "reviewed rewrite")
	reviewed := cliGit(t, pipeline, "rev-parse", "HEAD")
	cliGit(t, pipeline, "push", "--force", "origin", "HEAD:refs/heads/feature/recover")
	if err := database.UpdateRunStatusWithVerifiedHead(f.runID, types.RunFailed, reviewed); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunReviewApprovedHeadSHA(f.runID, reviewed); err != nil {
		t.Fatal(err)
	}
	anchor := "refs/no-mistakes/recover/" + f.runID
	cliGit(t, f.gate, "update-ref", anchor, reviewed)
	args := []string{"axi", "sync", "--adopt-reviewed-head", reviewed, "--run", f.runID, "--expected-local-head", f.submitted}
	before := cliGit(t, f.local, "show-ref")
	out, err := executeCmd(args...)
	if err != nil {
		t.Fatalf("preview: %v %s", err, out)
	}
	if !strings.Contains(out, "reviewed replacement") || !strings.Contains(out, "consent_digest:") {
		t.Fatalf("missing displayed diff/consent: %s", out)
	}
	if cliGit(t, f.local, "show-ref") != before {
		t.Fatal("preview changed refs")
	}
	run, _ := database.GetRun(f.runID)
	if run.CustodyReturnedAt != nil {
		t.Fatal("preview stamped custody")
	}
	var preview struct {
		Recovery struct {
			Digest string `toon:"consent_digest"`
		} `toon:"reviewed_recovery"`
	}
	if err := toon.UnmarshalString(out, &preview); err != nil || len(preview.Recovery.Digest) != 64 {
		t.Fatalf("digest: %s", out)
	}
	bad := append(append([]string{}, args...), "--consent", strings.Repeat("0", 64))
	if out, err := executeCmd(bad...); err == nil {
		t.Fatalf("wrong consent accepted: %s", out)
	}
	if cliGit(t, f.local, "rev-parse", "HEAD") != f.submitted {
		t.Fatal("bad digest moved caller")
	}
	apply := append(append([]string{}, args...), "--consent", preview.Recovery.Digest)
	out, err = executeCmd(apply...)
	if err != nil {
		t.Fatalf("apply: %v %s", err, out)
	}
	if !strings.Contains(out, "recovered: true") || cliGit(t, f.local, "rev-parse", "HEAD") != reviewed {
		t.Fatalf("apply result: %s", out)
	}
	if cliGit(t, f.local, "rev-parse", "refs/no-mistakes/recover-local/"+f.runID) != f.submitted {
		t.Fatal("lost submitted caller")
	}
}

func TestReviewedRecoveryCLIFlagsDoNotAuthorizeOrdinaryOrPartialRequests(t *testing.T) {
	for _, args := range [][]string{
		{"sync", "--run", "missing"},
		{"axi", "sync", "--consent", "anything"},
		{"sync", "--adopt-reviewed-head", "anything", "--yes"},
		{"axi", "sync", "--adopt-reviewed-head", "anything", "--recover"},
		{"axi", "sync", "--adopt-reviewed-head", "anything", "--keep-local"},
	} {
		if out, err := executeCmd(args...); err == nil {
			t.Fatalf("invalid flags accepted %v: %s", args, out)
		}
	}
}
