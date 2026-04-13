package buildinfo

import "testing"

func TestString(t *testing.T) {
	want := "dev (unknown) unknown"
	if got := String(); got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}
