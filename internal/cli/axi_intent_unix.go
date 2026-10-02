//go:build !windows

package cli

import "syscall"

const intentFileNonblock = syscall.O_NONBLOCK
