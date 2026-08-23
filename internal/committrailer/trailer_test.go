package committrailer

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseManyCanonicalizesAndDedupes(t *testing.T) {
	got, err := ParseMany([]string{
		"  Co-Authored-By: Phiora Agent <agent@phiora.test>  ",
		"Reviewed-by: Reviewer <reviewer@phiora.test>",
		"Co-Authored-By: Phiora Agent <agent@phiora.test>",
	})
	if err != nil {
		t.Fatalf("ParseMany() error = %v", err)
	}
	want := []Trailer{
		{Token: "Co-Authored-By", Value: "Phiora Agent <agent@phiora.test>"},
		{Token: "Reviewed-by", Value: "Reviewer <reviewer@phiora.test>"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ParseMany() = %#v, want %#v", got, want)
	}
	if got[0].String() != "Co-Authored-By: Phiora Agent <agent@phiora.test>" {
		t.Fatalf("canonical string = %q", got[0].String())
	}
}

func TestParseRejectsUnsafeInput(t *testing.T) {
	cases := []string{
		"",
		"   ",
		": missing token",
		"Co-Authored-By:",
		"Co-Authored-By",
		"-Co-Authored-By: Name <name@example.test>",
		"--trailer: Name <name@example.test>",
		"Co Authored By: Name <name@example.test>",
		"Co-Authored-By\n: Name <name@example.test>",
		"Co-Authored-By: Name\n <name@example.test>",
		"Co-Authored-By: Name\x00 <name@example.test>",
		"Co-Authored-By: \x1f",
		"Co-Authored-By: --no-verify",
		"Co-Authored-By: -c user.name=attacker",
	}
	for _, input := range cases {
		t.Run(strings.ReplaceAll(input, "\x00", "NUL"), func(t *testing.T) {
			if _, err := Parse(input); err == nil {
				t.Fatalf("Parse(%q) succeeded, want rejection", input)
			}
		})
	}
}

func TestAppendGitCommitArgsUsesArgumentVector(t *testing.T) {
	trailers, err := ParseMany([]string{
		"Co-Authored-By: Phiora Agent <agent@phiora.test>",
		"Reviewed-by: Reviewer <reviewer@phiora.test>",
	})
	if err != nil {
		t.Fatal(err)
	}
	got := AppendGitCommitArgs([]string{"commit", "-m", "subject"}, trailers)
	want := []string{
		"commit", "-m", "subject",
		"--trailer", "Co-Authored-By: Phiora Agent <agent@phiora.test>",
		"--trailer", "Reviewed-by: Reviewer <reviewer@phiora.test>",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("AppendGitCommitArgs() = %#v, want %#v", got, want)
	}
}

func TestAppendGitCommitArgsAbsentInputIsByteCompatible(t *testing.T) {
	base := []string{"commit", "--no-verify", "-m", "subject"}
	got := AppendGitCommitArgs(base, nil)
	if !reflect.DeepEqual(got, base) {
		t.Fatalf("AppendGitCommitArgs(nil) = %#v, want unchanged %#v", got, base)
	}
}
