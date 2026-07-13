package steps

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

type basecampReference struct {
	CardID string
	URL    string
}

type basecampReferenceSource uint8

const (
	basecampSourceCommit basecampReferenceSource = iota + 1
	basecampSourceIntent
)

type basecampReferenceCandidate struct {
	basecampReference
	source basecampReferenceSource
	index  int
	kind   int
}

var (
	basecampURLPattern    = regexp.MustCompile(`https://(?:app|3)\.basecamp\.com/[^\s<>()\[\]{}"']+`)
	basecampHashPattern   = regexp.MustCompile(`(?i)\bBC#\s*(\d+)\b`)
	basecampPhrasePattern = regexp.MustCompile(`(?i)\bBasecamp[ \t]+card[ \t]+(\d+)\b`)
)

func collectBasecampReferences(intentText, commitLog string) []basecampReference {
	var refs []basecampReference
	positions := make(map[string]int)
	urlSources := make(map[string]basecampReferenceSource)

	merge := func(candidates []basecampReferenceCandidate) {
		for _, candidate := range candidates {
			position, exists := positions[candidate.CardID]
			if !exists {
				positions[candidate.CardID] = len(refs)
				refs = append(refs, candidate.basecampReference)
				if candidate.URL != "" {
					urlSources[candidate.CardID] = candidate.source
				}
				continue
			}
			if candidate.URL == "" {
				continue
			}
			currentSource := urlSources[candidate.CardID]
			if refs[position].URL == "" || candidate.source > currentSource {
				refs[position].URL = candidate.URL
				urlSources[candidate.CardID] = candidate.source
			}
		}
	}

	merge(extractBasecampReferenceCandidates(intentText, basecampSourceIntent))
	merge(extractBasecampReferenceCandidates(commitLog, basecampSourceCommit))
	return refs
}

func extractBasecampReferenceCandidates(text string, source basecampReferenceSource) []basecampReferenceCandidate {
	if strings.TrimSpace(text) == "" {
		return nil
	}

	var candidates []basecampReferenceCandidate
	for _, match := range basecampURLPattern.FindAllStringIndex(text, -1) {
		raw := strings.TrimRight(text[match[0]:match[1]], ".,;:!?")
		cardID, canonicalURL, ok := parseBasecampCardURL(raw)
		if !ok {
			continue
		}
		candidates = append(candidates, basecampReferenceCandidate{
			basecampReference: basecampReference{CardID: cardID, URL: canonicalURL},
			source:            source,
			index:             match[0],
			kind:              0,
		})
	}

	for _, pattern := range []*regexp.Regexp{basecampHashPattern, basecampPhrasePattern} {
		for _, match := range pattern.FindAllStringSubmatchIndex(text, -1) {
			if len(match) < 4 || match[2] < 0 || match[3] < 0 {
				continue
			}
			candidates = append(candidates, basecampReferenceCandidate{
				basecampReference: basecampReference{CardID: text[match[2]:match[3]]},
				source:            source,
				index:             match[0],
				kind:              1,
			})
		}
	}

	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].index == candidates[j].index {
			return candidates[i].kind < candidates[j].kind
		}
		return candidates[i].index < candidates[j].index
	})
	return candidates
}

func parseBasecampCardURL(raw string) (string, string, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" {
		return "", "", false
	}
	host := strings.ToLower(u.Hostname())
	if host != "app.basecamp.com" && host != "3.basecamp.com" {
		return "", "", false
	}

	path := strings.TrimSuffix(u.Path, "/")
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(parts) != 6 || parts[1] != "buckets" || parts[3] != "card_tables" || parts[4] != "cards" {
		return "", "", false
	}
	if !decimalDigits(parts[0]) || !decimalDigits(parts[2]) || !decimalDigits(parts[5]) {
		return "", "", false
	}

	u.Scheme = "https"
	u.Host = host
	u.Path = "/" + strings.Join(parts, "/")
	u.RawPath = ""
	u.RawQuery = ""
	u.ForceQuery = false
	u.Fragment = ""
	return parts[5], u.String(), true
}

func decimalDigits(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func renderBasecampSection(refs []basecampReference) string {
	if len(refs) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("## Basecamp\n\n")
	for i, ref := range refs {
		if i > 0 {
			b.WriteString("\n")
		}
		if ref.URL != "" {
			fmt.Fprintf(&b, "- [Basecamp card %s](%s)", ref.CardID, ref.URL)
			continue
		}
		fmt.Fprintf(&b, "- Basecamp card %s — canonical URL not provided", ref.CardID)
	}
	return b.String()
}

func basecampWarningFindingsJSON(refs []basecampReference) string {
	findings := types.Findings{Summary: "Basecamp references detected without canonical URLs"}
	for _, ref := range refs {
		if ref.URL != "" {
			continue
		}
		findings.Items = append(findings.Items, types.Finding{
			ID:          "basecamp-url-missing-" + ref.CardID,
			Severity:    "warning",
			Description: fmt.Sprintf("Basecamp card %s has no canonical URL; the PR body includes an unlinked reference.", ref.CardID),
			Action:      types.ActionNoOp,
		})
	}
	if len(findings.Items) == 0 {
		return ""
	}
	raw, err := json.Marshal(findings)
	if err != nil {
		return ""
	}
	return string(raw)
}
