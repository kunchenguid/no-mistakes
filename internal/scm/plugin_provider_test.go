package scm

import "testing"

func TestPluginProviderIdentity(t *testing.T) {
	p := PluginProvider("ssm")
	if p != Provider("plugin:ssm") {
		t.Fatalf("PluginProvider = %q", p)
	}
	if name, ok := p.PluginName(); !ok || name != "ssm" || !p.IsPlugin() {
		t.Fatalf("PluginName = %q, %v", name, ok)
	}
	for _, builtin := range BuiltinProviders {
		if builtin.IsPlugin() {
			t.Fatalf("built-in %q reported as a plugin", builtin)
		}
	}
	if Provider("plugin:").IsPlugin() {
		t.Fatal("empty plugin name reported as a plugin")
	}
	if MaxPRBodyChars(p) != 0 {
		t.Fatalf("static limit for a plugin = %d, want 0 (plugins declare their own)", MaxPRBodyChars(p))
	}
}
