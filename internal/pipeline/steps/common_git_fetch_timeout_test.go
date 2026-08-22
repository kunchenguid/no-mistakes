package steps

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

// hangingGitRemote accepts TCP connections and then says nothing, which is what
// a dead SSH/git peer looks like to a fetch: connected, and never answering.
func hangingGitRemote(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			// Hold it open without replying.
			t.Cleanup(func() { _ = conn.Close() })
		}
	}()
	return fmt.Sprintf("git://%s/hang.git", listener.Addr().String())
}

func TestFetchRunUpstreamBranch_TimesOutAndSaysSo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	original := fetchUpstreamTimeout
	fetchUpstreamTimeout = 300 * time.Millisecond
	t.Cleanup(func() { fetchUpstreamTimeout = original })

	workDir := t.TempDir()
	if out, err := exec.Command("git", "-C", workDir, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}

	sctx := &pipeline.StepContext{
		Ctx:     context.Background(),
		WorkDir: workDir,
		Repo:    &db.Repo{UpstreamURL: hangingGitRemote(t)},
	}

	start := time.Now()
	// No deadline on the caller context: the fetch must impose its own.
	err := fetchRunUpstreamBranch(context.Background(), sctx, "main")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected the hanging fetch to fail")
	}
	if !errors.Is(err, ErrFetchTimeout) {
		t.Fatalf("error = %v, want it to wrap ErrFetchTimeout", err)
	}
	if elapsed > 30*time.Second {
		t.Fatalf("fetch took %v, the bound did not apply", elapsed)
	}
}
