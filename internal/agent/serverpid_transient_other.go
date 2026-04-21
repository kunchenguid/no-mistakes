//go:build !windows

package agent

func isTransientPIDOpenError(error) bool { return false }
