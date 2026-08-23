package committrailer

import (
	"fmt"
	"strings"
	"unicode"
)

// Trailer is one validated git commit trailer.
type Trailer struct {
	Token string `json:"token"`
	Value string `json:"value"`
}

func (t Trailer) String() string {
	if t.Token == "" && t.Value == "" {
		return ""
	}
	return t.Token + ": " + t.Value
}

// Parse validates and canonicalizes one "<Token>: <Value>" trailer.
func Parse(input string) (Trailer, error) {
	if containsControl(input) {
		return Trailer{}, fmt.Errorf("must not contain control characters")
	}
	raw := strings.TrimSpace(input)
	if raw == "" {
		return Trailer{}, fmt.Errorf("must not be empty")
	}
	token, value, ok := strings.Cut(raw, ":")
	if !ok {
		return Trailer{}, fmt.Errorf("must use '<Token>: <Value>' syntax")
	}
	token = strings.TrimSpace(token)
	value = strings.TrimSpace(value)
	if token == "" {
		return Trailer{}, fmt.Errorf("token must not be empty")
	}
	if value == "" {
		return Trailer{}, fmt.Errorf("value must not be empty")
	}
	if strings.HasPrefix(token, "-") || strings.HasPrefix(value, "-") {
		return Trailer{}, fmt.Errorf("token and value must not be option-like")
	}
	if err := validateToken(token); err != nil {
		return Trailer{}, err
	}
	return Trailer{Token: token, Value: value}, nil
}

// ParseMany validates a repeatable trailer input, preserving order while
// collapsing duplicate identical trailers.
func ParseMany(inputs []string) ([]Trailer, error) {
	if len(inputs) == 0 {
		return nil, nil
	}
	out := make([]Trailer, 0, len(inputs))
	seen := map[string]struct{}{}
	for _, input := range inputs {
		trailer, err := Parse(input)
		if err != nil {
			return nil, err
		}
		key := trailer.String()
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, trailer)
	}
	return out, nil
}

// Canonicalize validates and de-duplicates already structured trailers.
func Canonicalize(trailers []Trailer) ([]Trailer, error) {
	if len(trailers) == 0 {
		return nil, nil
	}
	inputs := make([]string, 0, len(trailers))
	for _, trailer := range trailers {
		inputs = append(inputs, trailer.String())
	}
	return ParseMany(inputs)
}

// AppendGitCommitArgs appends --trailer argv pairs to a git commit argv vector.
func AppendGitCommitArgs(args []string, trailers []Trailer) []string {
	if len(trailers) == 0 {
		return args
	}
	out := append([]string(nil), args...)
	seen := map[string]struct{}{}
	for _, trailer := range trailers {
		value := trailer.String()
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, "--trailer", value)
	}
	return out
}

func containsControl(input string) bool {
	for _, r := range input {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}

func validateToken(token string) error {
	for i, r := range token {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			continue
		}
		if r == '-' && i > 0 {
			continue
		}
		return fmt.Errorf("token %q must contain only ASCII letters, digits, and hyphens", token)
	}
	return nil
}
