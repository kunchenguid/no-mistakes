//go:build !windows

package agent

func isTransientPIDOpenError(error) bool { return false }

func isTransientPIDReplaceError(error) bool { return false }
