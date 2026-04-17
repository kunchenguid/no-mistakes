package update

import "testing"

func TestCompareVersions(t *testing.T) {
	tests := []struct {
		name    string
		a       string
		b       string
		wantCmp int
	}{
		{name: "equal with v prefix", a: "v1.2.3", b: "v1.2.3", wantCmp: 0},
		{name: "equal without v prefix", a: "1.2.3", b: "1.2.3", wantCmp: 0},
		{name: "patch newer", a: "v1.2.4", b: "v1.2.3", wantCmp: 1},
		{name: "patch older", a: "v1.2.3", b: "v1.2.4", wantCmp: -1},
		{name: "minor newer", a: "v1.3.0", b: "v1.2.9", wantCmp: 1},
		{name: "major newer", a: "v2.0.0", b: "v1.9.9", wantCmp: 1},
		{name: "missing patch treated as zero", a: "v1.2", b: "v1.2.0", wantCmp: 0},
		{name: "missing minor and patch treated as zero", a: "v1", b: "v1.0.0", wantCmp: 0},
		{name: "prerelease less than release", a: "v1.0.0-beta", b: "v1.0.0", wantCmp: -1},
		{name: "prerelease lexical compare", a: "v1.0.0-beta", b: "v1.0.0-rc1", wantCmp: -1},
		{name: "release greater than prerelease", a: "v1.0.0", b: "v1.0.0-rc1", wantCmp: 1},
		{name: "numeric prerelease compare", a: "v1.0.0-2", b: "v1.0.0-10", wantCmp: -1},
		{name: "build metadata ignored", a: "v1.2.3+abc", b: "v1.2.3+def", wantCmp: 0},
		{name: "prerelease with build metadata", a: "v1.2.3-rc1+abc", b: "v1.2.3-rc1+def", wantCmp: 0},
		{name: "different prerelease lengths", a: "v1.2.3-alpha.1", b: "v1.2.3-alpha", wantCmp: 1},
		{name: "numeric prerelease less than string", a: "v1.2.3-1", b: "v1.2.3-alpha", wantCmp: -1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := compareVersions(tt.a, tt.b)
			if err != nil {
				t.Fatalf("compareVersions(%q, %q) error = %v", tt.a, tt.b, err)
			}
			if got != tt.wantCmp {
				t.Fatalf("compareVersions(%q, %q) = %d, want %d", tt.a, tt.b, got, tt.wantCmp)
			}
		})
	}
}

func TestCompareVersionsRejectsInvalid(t *testing.T) {
	if _, err := compareVersions("dev", "v1.2.3"); err == nil {
		t.Fatal("compareVersions should reject non-semver input")
	}
}
