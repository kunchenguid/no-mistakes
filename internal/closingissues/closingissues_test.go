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
	for _, value := range []string{"", " ", "0", "#42", "owner/repo", "owner//repo#1", "owner/repo#0", "owner/repo#1#2", "owner name/repo#1", "single#1", "group//sub/project#1", "group/./project#1", "group/-/project#1"} {
		t.Run(value, func(t *testing.T) {
			if _, err := Normalize([]string{value}); err == nil {
				t.Fatalf("Normalize(%q) unexpectedly succeeded", value)
			}
		})
	}
}

// A GitLab project path nests under subgroups, so a cross-project reference is
// not limited to one slash; a GitHub owner/repository reference is the same
// shape with exactly two segments.
func TestNormalizeAcceptsSubgroupProjectPaths(t *testing.T) {
	got, err := Normalize([]string{"Group/SubGroup/Project#7", "group/subgroup/project#7", "other.io/my_group#3"})
	if err != nil {
		t.Fatal(err)
	}
	if refs := strings.Join(got, ","); refs != "group/subgroup/project#7,other.io/my_group#3" {
		t.Fatalf("Normalize() = %q", refs)
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

func TestLocalizeOwnRepositoryReference(t *testing.T) {
	for _, tc := range []struct{ ref, repo, want string }{
		{"owner/repo#95", "Owner/Repo", "95"},
		{"other/repo#95", "owner/repo", "other/repo#95"},
		{"95", "owner/repo", "95"},
		{"owner/repo#95", "", "owner/repo#95"},
	} {
		if got := Localize(tc.ref, tc.repo); got != tc.want {
			t.Fatalf("Localize(%q, %q) = %q, want %q", tc.ref, tc.repo, got, tc.want)
		}
	}
}

func TestRepositoryHyphensSurviveNormalizationAndStorage(t *testing.T) {
	for _, ref := range []string{"owner/repo-#42", "owner/-repo#42", "group/subgroup/-repo-#42"} {
		t.Run(ref, func(t *testing.T) {
			got, err := Normalize([]string{ref})
			if err != nil || len(got) != 1 || got[0] != ref {
				t.Fatalf("Normalize(%q) = %v, %v", ref, got, err)
			}
			encoded, err := Encode([]string{ref})
			if err != nil || encoded != ref {
				t.Fatalf("Encode(%q) = %q, %v", ref, encoded, err)
			}
			decoded, err := Decode(ref)
			if err != nil || len(decoded) != 1 || decoded[0] != ref {
				t.Fatalf("Decode(%q) = %v, %v", ref, decoded, err)
			}
		})
	}
}
