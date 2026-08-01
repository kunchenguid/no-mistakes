//go:build !windows

package cli

import (
	"fmt"
	"os"
)

func readReceiveCapabilityHandle(_ string) (*os.File, error) {
	return nil, fmt.Errorf("receive capability handles are unsupported on this platform")
}
