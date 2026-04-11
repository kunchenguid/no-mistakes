package git

import (
	"os"
	"path/filepath"
)

// PostReceiveHookScript returns the shell script for the post-receive hook.
// The hook notifies the daemon of a push via Unix socket. It never blocks
// the push — notification failures are silently ignored.
func PostReceiveHookScript() string {
	return `#!/bin/sh
# no-mistakes post-receive hook
# Notify daemon of push. Non-blocking — push always succeeds.
SOCKET="${NM_HOME:-$HOME/.no-mistakes}/socket"
json_escape() { printf '%s' "$1" | sed 's/\\/\\\\/g; s/"/\\"/g' | tr -d '\n\r\t'; }
while read oldrev newrev refname; do
  if [ -S "$SOCKET" ]; then
    gate=$(json_escape "$(pwd)")
    ref=$(json_escape "$refname")
    printf '{"method":"push_received","params":{"gate":"%s","ref":"%s","old":"%s","new":"%s"}}\n' \
      "$gate" "$ref" "$oldrev" "$newrev" | nc -U "$SOCKET" >/dev/null 2>&1 || true
  fi
done
printf '%s\n' 'no-mistakes: pipeline started. Run ` + "`no-mistakes`" + ` to review.' >&2
exit 0
`
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
