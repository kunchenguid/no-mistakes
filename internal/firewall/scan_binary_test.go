package firewall

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestGitHubCheck_BinaryContentFailsAsError(t *testing.T) {
	dir := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_COUNT=0", "HOME="+dir,
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return string(out)
	}
	git("init", "-q")
	git("commit", "--allow-empty", "-qm", "base")
	base := strings.TrimSpace(git("rev-parse", "HEAD"))
	if err := os.WriteFile(filepath.Join(dir, "inventory.dat"), []byte("\x00bind 10.0.0.5\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git("add", "inventory.dat")
	git("commit", "-qm", "add fixture")
	diffs := map[string]string{
		"aggregate": git("diff", "--no-ext-diff", "--no-textconv", base+"...HEAD"),
		"encoded":   git("diff", "--no-ext-diff", "--no-textconv", "--binary", base+"...HEAD"),
	}
	git("rm", "-q", "inventory.dat")
	git("commit", "-qm", "remove fixture")
	if diff := git("diff", base+"...HEAD"); diff != "" {
		t.Fatal("expected empty aggregate after removal")
	}
	diffs["history"] = git("log", "--no-ext-diff", "--no-textconv", "--format=", "--root", "-m", "-p", base+"..HEAD")
	for name, diff := range diffs {
		t.Run(name, func(t *testing.T) {
			res := Scan(Input{Diff: diff})
			if res.Error == "" || res.Conclusion() != "error" || len(res.Findings) != 0 {
				t.Fatalf("unjudged content must be an error: %+v", res)
			}
			out, code, err := GitHubCheck(Input{Diff: diff}, CheckOptions{})
			if code != ExitError || err == nil || out != PublicErrorText("") {
				t.Fatalf("out=%q code=%d err=%v", out, code, err)
			}
		})
	}
}

func TestScan_BinaryMarkerInTextIsOrdinaryContent(t *testing.T) {
	res := Scan(Input{Diff: unified("docs.md", []string{"Binary files a/example and b/example differ", "GIT binary patch"})})
	if res.Error != "" {
		t.Fatalf("text mistaken for omitted content: %s", res.Error)
	}
}
