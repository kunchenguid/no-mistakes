package cli

import "testing"

func TestDoctorAgentChecksIncludesGrok(t *testing.T) {
	for _, check := range doctorAgentChecks() {
		if check.name != "grok" {
			continue
		}
		if len(check.binaries) != 1 || check.binaries[0] != "grok" {
			t.Fatalf("grok doctor binaries = %v, want [grok]", check.binaries)
		}
		return
	}
	t.Fatal("doctor checks do not include the first-class grok agent")
}
