package pipeline

import "github.com/kunchenguid/no-mistakes/internal/types"

func normalizeFindingsJSON(raw string, prefix string) string {
	if raw == "" {
		return ""
	}
	findings, err := types.ParseFindingsJSON(raw)
	if err != nil {
		return raw
	}
	normalized := types.NormalizeFindings(findings, prefix)
	normalizedRaw, err := types.MarshalFindingsJSON(normalized)
	if err != nil {
		return raw
	}
	return normalizedRaw
}

func filterFindingsJSON(raw string, ids []string) string {
	if raw == "" || len(ids) == 0 {
		return raw
	}
	findings, err := types.ParseFindingsJSON(raw)
	if err != nil {
		return raw
	}
	filtered := types.FilterFindings(findings, ids)
	filteredRaw, err := types.MarshalFindingsJSON(filtered)
	if err != nil {
		return raw
	}
	return filteredRaw
}
