//go:build windows

package branchsync

import (
	"fmt"
	"os"
)

func gateRefFileIdentityValue(info os.FileInfo) string {
	return fmt.Sprintf("%T:%#v", info.Sys(), info.Sys())
}

func acquireGateRefOSLock(file *os.File) error {
	return nil
}

func releaseGateRefOSLock(file *os.File) {}
