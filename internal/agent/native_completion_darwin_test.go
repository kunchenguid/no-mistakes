package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Exercise the public native invocation, including the CLI's successful exit,
// output parsing, child settling, inspection deadlines and group cleanup.
func TestNativeCompletionWithSlowHostInspection(t *testing.T) {
	for _, mode := range []string{"clean", "finishing", "live", "slow-first", "slow-later"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			argsFile := filepath.Join(dir, "inspection.args")
			firstProbe := filepath.Join(dir, "first-probe")
			// Legacy ps ignores -g. The completion inspector must override it.
			t.Setenv("COMMAND_MODE", "legacy")
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			inspector := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$COMMAND_MODE" "$*" >> %q
if [ "$1" = -eo ] || [ %q = slow-first ]; then
  sleep 5
elif [ %q = slow-later ]; then
  if [ -e %q ]; then sleep 5; fi
  touch %q
fi
exec /bin/ps "$@"
`, argsFile, mode, mode, firstProbe, firstProbe)
			if err := os.WriteFile(filepath.Join(dir, "ps"), []byte(inspector), 0o700); err != nil {
				t.Fatal(err)
			}
			child := "sleep 0.4 >/dev/null 2>&1 &\n"
			if mode == "clean" {
				child = ""
			}
			if mode == "live" || mode == "slow-later" {
				child = "sleep 30 >/dev/null 2>&1 &\n"
			}
			bin := filepath.Join(dir, "codex")
			script := "#!/bin/sh\ncat >/dev/null\n" + child + `printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"{\"ok\":true}"}}'` + "\nexit 0\n"
			if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			native, err := agent.New(types.AgentCodex, bin, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer native.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			var leader atomic.Int64
			result, err := native.Run(ctx, agent.RunOpts{
				Prompt:                   "return the completed result",
				CWD:                      dir,
				JSONSchema:               json.RawMessage(`{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"]}`),
				RequireCompletedChildren: true,
				OnLifecycle: func(event agent.LifecycleEvent) {
					if event.Phase == agent.LifecyclePhaseStart {
						leader.Store(int64(event.PID))
					}
				},
			})
			switch mode {
			case "clean", "finishing":
				if err != nil || result == nil || string(result.Output) != `{"ok":true}` {
					t.Fatalf("completed native invocation: result=%+v err=%v", result, err)
				}
			case "live":
				if err == nil || !strings.Contains(err.Error(), "native check completion is unverified") {
					t.Fatalf("live owned child was accepted: %v", err)
				}
			case "slow-first", "slow-later":
				if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "native check completion is unverified") {
					t.Fatalf("slow targeted inspection certified completion: %v", err)
				}
				if mode == "slow-later" && !strings.Contains(err.Error(), "last observed pid ") {
					t.Fatalf("lost the last observed member on inspection failure: %v", err)
				}
			}
			if mode != "clean" {
				args, readErr := os.ReadFile(argsFile)
				if readErr != nil {
					t.Fatal(readErr)
				}
				want := "unix2003\n-g " + strconv.FormatInt(leader.Load(), 10) + " -o pgid=,stat=,pid=,command=\n"
				if !strings.Contains(string(args), want) {
					t.Fatalf("inspection did not select the invocation's group: %q", args)
				}
			}
		})
	}
}
