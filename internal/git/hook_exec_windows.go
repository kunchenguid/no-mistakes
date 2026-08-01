//go:build windows

package git

import "os"

func gateHookExecutableMode(info os.FileInfo) bool {
	return info.Mode().IsRegular()
}
