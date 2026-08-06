package evidence

import "testing"

func TestNormalizeBranch_EmptyUsesDefault(t *testing.T) {
	got, err := NormalizeBranch("   ")
	if err != nil {
		t.Fatalf("NormalizeBranch: %v", err)
	}
	if got != DefaultBranch {
		t.Errorf("branch = %q, want default %q", got, DefaultBranch)
	}
}

func TestNormalizeBranch_AcceptsConfiguredNames(t *testing.T) {
	for _, name := range []string{
		"no-mistakes/evidence",
		"evidence",
		"team/ci/evidence",
		"evidence-2",
		"evidence.v2",
		" evidence ",
	} {
		got, err := NormalizeBranch(name)
		if err != nil {
			t.Errorf("NormalizeBranch(%q) rejected a valid name: %v", name, err)
			continue
		}
		if got == "" {
			t.Errorf("NormalizeBranch(%q) returned an empty name", name)
		}
	}
}

func TestNormalizeBranch_RejectsInvalidGitRefNames(t *testing.T) {
	for _, name := range []string{
		"evidence branch",
		"evidence..branch",
		"evidence~1",
		"evidence^",
		"evidence:x",
		"evidence?",
		"evidence*",
		"evidence[1]",
		`evidence\x`,
		"/evidence",
		"evidence/",
		"evidence//branch",
		".evidence",
		"team/.evidence",
		"evidence.lock",
		"evidence.",
		"-evidence",
		"HEAD",
		"@",
		"evidence@{1}",
		"refs/heads/evidence",
		"evidence\tbranch",
	} {
		if got, err := NormalizeBranch(name); err == nil {
			t.Errorf("NormalizeBranch(%q) accepted an invalid branch name as %q", name, got)
		}
	}
}
