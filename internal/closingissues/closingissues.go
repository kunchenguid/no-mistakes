// Package closingissues owns validation and canonicalization of explicit issue
// references supplied through axi run --closes.
package closingissues

import (
	"fmt"
	"sort"
	"strings"
)

// Normalize validates, deduplicates case-insensitively, and deterministically
// orders issue references. Same-repository references are stored as decimal
// numbers; cross-repository references use project#number, where project is a
// provider project path: "owner/repository" on GitHub and, on GitLab, a path
// that may nest under subgroups ("group/subgroup/repository").
func Normalize(values []string) ([]string, error) {
	seen := make(map[string]struct{}, len(values))
	refs := make([]string, 0, len(values))
	for _, value := range values {
		if strings.TrimSpace(value) == "" {
			return nil, fmt.Errorf("closing issue reference must not be empty")
		}
		ref, err := normalize(value)
		if err != nil {
			return nil, err
		}
		if ref == "" {
			continue
		}
		key := strings.ToLower(ref)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		refs = append(refs, ref)
	}
	sort.Slice(refs, func(i, j int) bool { return compare(refs[i], refs[j]) < 0 })
	return refs, nil
}

// Encode returns the stable database representation of already-normalized
// references. Newlines are safe separators because validation rejects them.
func Encode(refs []string) (string, error) {
	normalized, err := Normalize(refs)
	if err != nil {
		return "", err
	}
	return strings.Join(normalized, "\n"), nil
}

// Decode validates a stored representation instead of trusting database text.
func Decode(value string) ([]string, error) {
	if strings.TrimSpace(value) == "" {
		return nil, nil
	}
	return Normalize(strings.Split(value, "\n"))
}

// Covers reports whether every normalized ref in want is present in have.
func Covers(have, want []string) bool {
	set := make(map[string]struct{}, len(have))
	for _, ref := range have {
		set[strings.ToLower(ref)] = struct{}{}
	}
	for _, ref := range want {
		if _, ok := set[strings.ToLower(ref)]; !ok {
			return false
		}
	}
	return true
}

// Localize returns ref as a bare issue number when it is qualified with repo
// (owner/repository, compared case-insensitively), so a PR's own repository
// names each issue one way. Other references are returned unchanged.
func Localize(ref, repo string) string {
	prefix, number, qualified := strings.Cut(ref, "#")
	if qualified && repo != "" && strings.EqualFold(prefix, repo) {
		return number
	}
	return ref
}

// Target returns the syntax the forge's closing keywords expect after the
// keyword. GitHub and GitLab both accept "#42" for the project's own issues and
// "project#42" for a cross-project one, so the stored ref is already the target.
func Target(ref string) string {
	if strings.Contains(ref, "#") {
		return ref
	}
	return "#" + ref
}

// Parts returns template data for a normalized reference. A nested project
// path (a GitLab subgroup path) leaves the subgroup segments in repository, so
// the reference still round-trips through owner + repository.
func Parts(ref string) (owner, repository, issue, target string) {
	prefix, issue, qualified := strings.Cut(ref, "#")
	if !qualified {
		return "", "", ref, Target(ref)
	}
	owner, repository, _ = strings.Cut(prefix, "/")
	return owner, repository, issue, Target(ref)
}

func normalize(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	prefix, number, qualified := strings.Cut(value, "#")
	if !qualified {
		number = value
		prefix = ""
	} else if !validProjectPath(prefix) {
		return "", fmt.Errorf("invalid closing issue reference %q: expected a number or project#number", value)
	}
	if !positiveDecimal(number) {
		return "", fmt.Errorf("invalid closing issue reference %q: issue number must be a positive decimal number", value)
	}
	if prefix == "" {
		return number, nil
	}
	return strings.ToLower(prefix) + "#" + number, nil
}

// validProjectPath reports whether prefix is a provider project path: at least
// two "/"-separated segments, each a well-formed project path segment. Two
// segments is the GitHub owner/repository shape; a GitLab path may nest under
// subgroups, so the count is open above it.
func validProjectPath(prefix string) bool {
	segments := strings.Split(prefix, "/")
	if len(segments) < 2 {
		return false
	}
	for i, segment := range segments {
		if i < len(segments)-1 && (strings.HasPrefix(segment, "-") || strings.HasSuffix(segment, "-")) {
			return false
		}
		if !validProjectSegment(segment) {
			return false
		}
	}
	return true
}

// validProjectSegment reports whether one "/"-separated piece of a project path
// is well formed. The rule is the repository one because a project path
// segment - GitLab group and project paths alike - allows ".", "_" and "-"
// inside it; a GitHub owner is a narrower case that rides on the same rule.
func validProjectSegment(value string) bool {
	if value == "" || value == "." || value == ".." {
		return false
	}
	for _, r := range value {
		if !asciiLetterOrDigit(r) && r != '-' && r != '_' && r != '.' {
			return false
		}
	}
	return true
}

func positiveDecimal(value string) bool {
	if value == "" || value[0] == '0' {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func asciiLetterOrDigit(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
}

func compare(a, b string) int {
	aPrefix, aNumber, aQualified := strings.Cut(a, "#")
	bPrefix, bNumber, bQualified := strings.Cut(b, "#")
	if !aQualified {
		aNumber, aPrefix = a, ""
	}
	if !bQualified {
		bNumber, bPrefix = b, ""
	}
	if c := strings.Compare(strings.ToLower(aPrefix), strings.ToLower(bPrefix)); c != 0 {
		return c
	}
	if len(aNumber) != len(bNumber) {
		return len(aNumber) - len(bNumber)
	}
	return strings.Compare(aNumber, bNumber)
}
