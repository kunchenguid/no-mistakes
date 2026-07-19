//go:build darwin

package procguard

// SeatbeltSignalProfile returns a macOS Seatbelt (sandbox-exec) profile that
// permits everything a gate agent normally does but denies delivering a signal
// to any process outside its own sandbox. It is the OS-native, kernel-enforced
// hardening layer that closes the residual the PATH-interposition guard cannot:
// the shell builtin `kill`, an absolute-path `/bin/kill`, and direct kill(2)
// syscalls all go through Seatbelt regardless of PATH.
//
// Rule semantics (last matching rule wins in Seatbelt):
//
//	(allow default)                        - keep full FS/network/exec access
//	(deny signal)                          - deny every signal delivery
//	(allow signal (target self))           - a process may still signal itself
//	(allow signal (target same-sandbox))   - and other processes it spawned
//	                                         under this same sandbox (its scope)
//
// The `signal` operation and its `(target self)` / `(target same-sandbox)`
// filters are the same primitives Apple's own system profiles use (see
// /System/Library/Sandbox/Profiles/*.sb), which is the evidence that this is a
// supported, enforced boundary rather than advisory.
//
// This is provided as a validated, ready-to-wire primitive. It is NOT wired into
// the default agent launch here because a Seatbelt wrap must first be validated
// against each heavy agent binary (claude/codex/...) to confirm `(allow default)`
// does not perturb them, and that validation requires running the full pipeline,
// which is out of scope for repairing the pipeline. The portable interposition
// guard is the active default; this is the recommended platform-specific
// hardening on top.
func SeatbeltSignalProfile() string {
	return "(version 1)\n" +
		"(allow default)\n" +
		"(deny signal)\n" +
		"(allow signal (target self))\n" +
		"(allow signal (target same-sandbox))\n"
}
