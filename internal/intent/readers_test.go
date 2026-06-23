package intent

import "testing"

func TestAllReaders_NoDisabled(t *testing.T) {
	got := AllReaders(nil)
	if len(got) != 6 {
		t.Errorf("expected 6 readers, got %d", len(got))
	}
}

func TestAllReaders_Disabled(t *testing.T) {
	got := AllReaders(map[string]bool{"codex": true, "rovodev": true})
	if len(got) != 4 {
		t.Errorf("expected 4 readers, got %d", len(got))
	}
	for _, r := range got {
		if r.Name() == "codex" || r.Name() == "rovodev" {
			t.Errorf("disabled reader %q present", r.Name())
		}
	}
}
