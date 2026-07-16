package intent

import "testing"

func TestAllReaders_NoDisabled(t *testing.T) {
	got := AllReaders(nil)
	if len(got) != 7 {
		t.Errorf("expected 7 readers, got %d", len(got))
	}
}

func TestAllReaders_Disabled(t *testing.T) {
	got := AllReaders(map[string]bool{"codex": true, "omp": true})
	if len(got) != 5 {
		t.Errorf("expected 5 readers, got %d", len(got))
	}
	for _, r := range got {
		if r.Name() == "codex" || r.Name() == "omp" {
			t.Errorf("disabled reader %q present", r.Name())
		}
	}
}
