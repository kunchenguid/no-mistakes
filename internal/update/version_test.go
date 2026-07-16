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
		{name: "ahead of release git describe is newer than its base", a: "v1.37.0-19-g285c8ee", b: "v1.37.0", wantCmp: 1},
		{name: "dirty ahead of release git describe is newer than its base", a: "v1.37.0-19-g285c8ee-dirty", b: "v1.37.0", wantCmp: 1},
		{name: "dirty tagged development build is newer than its base", a: "v1.37.0-dirty", b: "v1.37.0", wantCmp: 1},
		{name: "ahead build still accepts a genuinely later release", a: "v1.37.0-19-g285c8ee", b: "v1.38.0", wantCmp: -1},
		{name: "beta is older than its stable release", a: "v1.38.0-beta.1", b: "v1.38.0", wantCmp: -1},
		{name: "beta policy can compare a later beta", a: "v1.37.0", b: "v1.38.0-beta.1", wantCmp: -1},
		{name: "development build from beta tag stays ahead of that beta", a: "v1.38.0-beta.1-2-g285c8ee", b: "v1.38.0-beta.1", wantCmp: 1},
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
	tests := []struct {
		name    string
		current string
		latest  string
	}{
		{name: "dev", current: "dev", latest: "v1.2.3"},
		{name: "empty", current: "", latest: "v1.2.3"},
		{name: "malformed git describe", current: "v1.37.0-19-gnot-a-commit", latest: "v1.37.0"},
		{name: "empty prerelease", current: "v1.2.3-", latest: "v1.2.3"},
		{name: "invalid build metadata", current: "v1.2.3+", latest: "v1.2.3"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := compareVersions(tt.current, tt.latest); err == nil {
				t.Fatalf("compareVersions(%q, %q) should reject malformed current version", tt.current, tt.latest)
			}
		})
	}
}

func TestIsDevVersion(t *testing.T) {
	tests := []struct {
		version string
		want    bool
	}{
		{version: "dev", want: true},
		{version: "285c8ee", want: true},
		{version: "285c8ee-dirty", want: true},
		{version: "v1.37.0-19-g285c8ee", want: false},
		{version: "v1.37.0-19-g285c8ee-dirty", want: false},
		{version: "v1.37.0", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.version, func(t *testing.T) {
			if got := isDevVersion(tt.version); got != tt.want {
				t.Fatalf("isDevVersion(%q) = %v, want %v", tt.version, got, tt.want)
			}
		})
	}
}
