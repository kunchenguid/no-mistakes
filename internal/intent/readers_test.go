package intent

import "testing"

func TestAllReaders_NoDisabled(t *testing.T) {
	got := AllReaders(nil)
	if len(got) != 1 || got[0].Name() != "omp" {
		t.Fatalf("readers = %v, want only omp", got)
	}
}

func TestAllReaders_Disabled(t *testing.T) {
	got := AllReaders(map[string]bool{"omp": true})
	if len(got) != 0 {
		t.Errorf("expected the disabled omp reader to be absent, got %v", got)
	}
}
