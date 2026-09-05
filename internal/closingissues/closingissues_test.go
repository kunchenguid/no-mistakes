package closingissues

import (
	"strings"
	"testing"
)

func TestNormalizeRepeatableReferencesDeduplicatesAndOrders(t *testing.T) {
	got, err := Normalize([]string{"owner/repo#10", "2", "OWNER/REPO#10", "10", "owner/repo#2"})
	if err != nil {
		t.Fatal(err)
	}
	if refs := strings.Join(got, ","); refs != "2,10,owner/repo#2,owner/repo#10" {
		t.Fatalf("Normalize() = %q", refs)
	}
}

func TestNormalizeRejectsMalformedReferences(t *testing.T) {
	for _, value := range []string{"", " ", "0", "#42", "owner/repo", "owner//repo#1", "owner/repo#0", "owner/repo#1#2", "owner name/repo#1"} {
		t.Run(value, func(t *testing.T) {
			if _, err := Normalize([]string{value}); err == nil {
				t.Fatalf("Normalize(%q) unexpectedly succeeded", value)
			}
		})
	}
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	encoded, err := Encode([]string{"owner/repo#9", "42", "42"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if refs := strings.Join(got, ","); refs != "42,owner/repo#9" {
		t.Fatalf("round trip = %q", refs)
	}
}
