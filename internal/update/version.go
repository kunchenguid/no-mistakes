package update

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

type semVersion struct {
	major       int
	minor       int
	patch       int
	prerelease  []string
	development bool
}

var (
	gitDescribeSuffix = regexp.MustCompile(`-\d+-g[0-9a-fA-F]+$`)
	gitDescribeMarker = regexp.MustCompile(`-\d+-g`)
)

func compareVersions(a, b string) (int, error) {
	va, err := parseVersion(a)
	if err != nil {
		return 0, err
	}
	vb, err := parseVersion(b)
	if err != nil {
		return 0, err
	}
	return va.compare(vb), nil
}

func parseVersion(raw string) (semVersion, error) {
	trimmed := strings.TrimSpace(raw)
	trimmed = strings.TrimPrefix(trimmed, "v")
	if trimmed == "" {
		return semVersion{}, fmt.Errorf("parse version %q: empty", raw)
	}

	development := false
	if strings.HasSuffix(trimmed, "-dirty") {
		trimmed = strings.TrimSuffix(trimmed, "-dirty")
		development = true
	}
	if match := gitDescribeSuffix.FindStringIndex(trimmed); match != nil {
		trimmed = trimmed[:match[0]]
		development = true
	} else if gitDescribeMarker.MatchString(trimmed) {
		return semVersion{}, fmt.Errorf("parse version %q: malformed git-describe suffix", raw)
	}

	v, err := parseSemVersion(trimmed, raw)
	if err != nil {
		return semVersion{}, err
	}
	v.development = development
	return v, nil
}

func parseSemVersion(trimmed, raw string) (semVersion, error) {
	if strings.Count(trimmed, "+") > 1 {
		return semVersion{}, fmt.Errorf("parse version %q: invalid build metadata", raw)
	}

	if before, metadata, ok := strings.Cut(trimmed, "+"); ok {
		if metadata == "" || !validVersionIdentifiers(metadata, true) {
			return semVersion{}, fmt.Errorf("parse version %q: invalid build metadata", raw)
		}
		trimmed = before
	}
	core := trimmed
	pre := ""
	if before, after, ok := strings.Cut(trimmed, "-"); ok {
		core = before
		pre = after
		if pre == "" {
			return semVersion{}, fmt.Errorf("parse version %q: empty prerelease", raw)
		}
	}

	parts := strings.Split(core, ".")
	if len(parts) == 0 || len(parts) > 3 {
		return semVersion{}, fmt.Errorf("parse version %q: invalid core", raw)
	}

	v := semVersion{}
	for len(parts) < 3 {
		parts = append(parts, "0")
	}
	ints := []*int{&v.major, &v.minor, &v.patch}
	for i, part := range parts {
		if part == "" || (len(part) > 1 && part[0] == '0') {
			return semVersion{}, fmt.Errorf("parse version %q: empty numeric segment", raw)
		}
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 {
			return semVersion{}, fmt.Errorf("parse version %q: invalid numeric segment %q", raw, part)
		}
		*ints[i] = n
	}

	if pre != "" {
		idents := strings.Split(pre, ".")
		for _, ident := range idents {
			if ident == "" || !validVersionIdentifier(ident) {
				return semVersion{}, fmt.Errorf("parse version %q: empty prerelease segment", raw)
			}
			if isNumericIdentifier(ident) && len(ident) > 1 && ident[0] == '0' {
				return semVersion{}, fmt.Errorf("parse version %q: invalid numeric prerelease segment %q", raw, ident)
			}
			v.prerelease = append(v.prerelease, ident)
		}
	}

	return v, nil
}

func validVersionIdentifiers(raw string, allowDots bool) bool {
	if raw == "" {
		return false
	}
	if allowDots {
		for _, ident := range strings.Split(raw, ".") {
			if !validVersionIdentifier(ident) {
				return false
			}
		}
		return true
	}
	return validVersionIdentifier(raw)
}

func validVersionIdentifier(ident string) bool {
	if ident == "" {
		return false
	}
	for i := 0; i < len(ident); i++ {
		c := ident[i]
		if (c < '0' || c > '9') && (c < 'A' || c > 'Z') && (c < 'a' || c > 'z') && c != '-' {
			return false
		}
	}
	return true
}

func isNumericIdentifier(ident string) bool {
	if ident == "" {
		return false
	}
	for i := 0; i < len(ident); i++ {
		if ident[i] < '0' || ident[i] > '9' {
			return false
		}
	}
	return true
}

func isDevVersion(version string) bool {
	if version == "" || version == "dev" {
		return true
	}
	_, err := parseVersion(version)
	return err != nil
}

func (v semVersion) compare(other semVersion) int {
	if diff := cmpInt(v.major, other.major); diff != 0 {
		return diff
	}
	if diff := cmpInt(v.minor, other.minor); diff != 0 {
		return diff
	}
	if diff := cmpInt(v.patch, other.patch); diff != 0 {
		return diff
	}

	if len(v.prerelease) == 0 && len(other.prerelease) == 0 {
		return cmpInt(boolInt(v.development), boolInt(other.development))
	}
	if len(v.prerelease) == 0 {
		return 1
	}
	if len(other.prerelease) == 0 {
		return -1
	}

	for i := 0; i < len(v.prerelease) && i < len(other.prerelease); i++ {
		if diff := comparePrereleaseIdentifier(v.prerelease[i], other.prerelease[i]); diff != 0 {
			return diff
		}
	}
	if diff := cmpInt(len(v.prerelease), len(other.prerelease)); diff != 0 {
		return diff
	}
	return cmpInt(boolInt(v.development), boolInt(other.development))
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func comparePrereleaseIdentifier(a, b string) int {
	ai, aerr := strconv.Atoi(a)
	bi, berr := strconv.Atoi(b)
	switch {
	case aerr == nil && berr == nil:
		return cmpInt(ai, bi)
	case aerr == nil:
		return -1
	case berr == nil:
		return 1
	default:
		return cmpInt(strings.Compare(a, b), 0)
	}
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

func releaseArchiveName(app, version string, platform platformSpec) string {
	ext := ".tar.gz"
	if platform.GOOS == "windows" {
		ext = ".zip"
	}
	return fmt.Sprintf("%s-%s-%s-%s%s", app, version, platform.GOOS, platform.GOARCH, ext)
}

func binaryName(app string, platform platformSpec) string {
	if platform.GOOS == "windows" {
		return app + ".exe"
	}
	return app
}
