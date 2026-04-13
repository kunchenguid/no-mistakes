package git

import (
	"os"
	"path/filepath"
	"strings"
)

// PostReceiveHookScript returns the shell script for the post-receive hook.
// The hook notifies the daemon via the CLI so it works across platforms.
// It never blocks the push - notification failures are silently ignored.
func PostReceiveHookScript() string {
	exe, err := os.Executable()
	if err != nil {
		exe = "no-mistakes"
	}
	return postReceiveHookScript(exe)
}

func postReceiveHookScript(command string) string {
	return `#!/bin/sh
# no-mistakes post-receive hook
# Notify daemon of push. Non-blocking - push always succeeds.
NM_BIN=` + shellSingleQuote(command) + `
if [ ! -f "$NM_BIN" ]; then
  NM_BIN="$(command -v no-mistakes 2>/dev/null || echo no-mistakes)"
fi
while read oldrev newrev refname; do
  NM_HOOK_HELPER=1 "$NM_BIN" daemon notify-push \
    --gate "$(pwd)" \
    --ref "$refname" \
    --old "$oldrev" \
    --new "$newrev" >/dev/null 2>&1 || true
done
cat >&2 <<'BANNER'
` + "\033[36m" + `_  _ ____    _  _ _ ____ ___ ____ _  _ ____ ____
|\ | |  |    |\/| | [__   |  |__| |_/  |___ [__
| \| |__|    |  | | ___]  |  |  | | \_ |___ ___]` + "\033[0m" + `

  ` + "\033[32m" + `✓` + "\033[0m" + ` Pipeline started

  ` + "\033[90m" + `Run` + "\033[0m" + ` ` + "\033[1m" + `no-mistakes` + "\033[0m" + ` ` + "\033[90m" + `to review.` + "\033[0m" + `
BANNER
exit 0
`
}

func shellSingleQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

// InstallPostReceiveHook writes the post-receive hook script into
// the hooks directory of a bare repo at bareDir.
func InstallPostReceiveHook(bareDir string) error {
	hooksDir := filepath.Join(bareDir, "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		return err
	}
	hookPath := filepath.Join(hooksDir, "post-receive")
	return os.WriteFile(hookPath, []byte(PostReceiveHookScript()), 0o755)
}
