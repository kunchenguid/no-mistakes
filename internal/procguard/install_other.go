//go:build !unix

package procguard

// Install is a no-op on platforms procguard does not shim. There is no
// pkill/killall analog to interpose on the agent PATH, so the OS-native layer
// (documented) is the only signal-scope control available there.
func Install(root string) error { return nil }
