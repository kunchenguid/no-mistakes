package procguard

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/paths"
)

// BinDir returns the directory that holds the interposition shims for a given
// NM_HOME root. It is a stable location so a freshly started daemon reinstalls
// over the same path.
func BinDir(root string) string {
	return filepath.Join(root, "procguard", "bin")
}

// DefaultBinDir resolves BinDir for the active NM_HOME (or ~/.no-mistakes).
func DefaultBinDir() (string, error) {
	p, err := paths.New()
	if err != nil {
		return "", err
	}
	return BinDir(p.Root()), nil
}

// AugmentPATH prepends binDir to the PATH entry of env so the guard shims
// resolve ahead of the real kill/pkill/killall. It is a pure string transform:
// a missing PATH entry is synthesized, and an already-prepended binDir is left
// alone so repeated application is idempotent. The returned slice may share
// backing storage with env.
func AugmentPATH(env []string, binDir string) []string {
	if strings.TrimSpace(binDir) == "" {
		return env
	}
	sep := string(os.PathListSeparator)
	for i, entry := range env {
		if !strings.HasPrefix(entry, "PATH=") {
			continue
		}
		cur := strings.TrimPrefix(entry, "PATH=")
		if cur == binDir || strings.HasPrefix(cur, binDir+sep) {
			return env
		}
		if cur == "" {
			env[i] = "PATH=" + binDir
		} else {
			env[i] = "PATH=" + binDir + sep + cur
		}
		return env
	}
	return append(env, "PATH="+binDir)
}
