//go:build unix

package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/runenv"
)

func TestCursorCatalogCancellationReapsOwnedChild(t *testing.T) {
	a, dir := cursorCatalogFixture(t, `import fs from 'node:fs'; import {spawn} from 'node:child_process'; export async function inspectAgentModels(){const child=spawn(process.execPath,['-e','setInterval(()=>{},1000)'],{stdio:'ignore'}); fs.writeFileSync(process.env.FIXTURE_PID,String(child.pid)); await new Promise(()=>setInterval(()=>{},1000));}`)
	pidfile := filepath.Join(dir, "owned.pid")
	a.subprocessContext = newSubprocessContext(runenv.Overlay{Set: map[string]string{"FIXTURE_PID": pidfile}})
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()
	err := a.preflightCursor(ctx, RunOpts{CWD: dir}, "agent_config.cursor")
	var refusal *CursorModelError
	if !errors.As(err, &refusal) || refusal.Status != CursorModelIndeterminate {
		t.Fatalf("deadline result: %v", err)
	}
	body, err := os.ReadFile(pidfile)
	if err != nil {
		t.Fatalf("fixture did not reach child spawn: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	// Cleanup is bounded even on failure, and touches only this fixture's child.
	defer syscall.Kill(pid, syscall.SIGKILL)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("catalog cancellation left its owned child alive")
}
