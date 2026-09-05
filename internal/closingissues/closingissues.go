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
// numbers; cross-repository references use owner/repository#number.
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

// Target returns the syntax GitHub closing keywords expect after the keyword.
func Target(ref string) string {
	if strings.Contains(ref, "#") {
		return ref
	}
	return "#" + ref
}

// Parts returns template data for a normalized reference.
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
	} else if strings.Contains(number, "#") || strings.Count(prefix, "/") != 1 {
		return "", fmt.Errorf("invalid closing issue reference %q: expected a number or owner/repository#number", value)
	}
	if !positiveDecimal(number) {
		return "", fmt.Errorf("invalid closing issue reference %q: issue number must be a positive decimal number", value)
	}
	if prefix == "" {
		return number, nil
	}
	owner, repo, _ := strings.Cut(prefix, "/")
	if !validOwner(owner) || !validRepository(repo) {
		return "", fmt.Errorf("invalid closing issue reference %q: expected owner/repository#number", value)
	}
	return strings.ToLower(owner+"/"+repo) + "#" + number, nil
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

func validOwner(value string) bool {
	if value == "" || value[0] == '-' || value[len(value)-1] == '-' {
		return false
	}
	for _, r := range value {
		if !asciiLetterOrDigit(r) && r != '-' {
			return false
		}
	}
	return true
}

func validRepository(value string) bool {
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
