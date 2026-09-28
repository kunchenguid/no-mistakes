package pipeline

import (
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// CanRecheckCIProvider recognizes only provider-read gates, not code defects,
// review-bot decisions, or interrupted repairs. Keep the legacy uncategorized
// form answerable after upgrades; IDs alone never identify this condition.
func CanRecheckCIProvider(raw string) bool {
	findings, err := types.ParseFindingsJSON(raw)
	if err != nil || len(findings.Items) == 0 {
		return false
	}
	for _, f := range findings.Items {
		if f.ActionOrDefault() != types.ActionAskUser || f.File != "" || f.Check != "" || f.CheckID != "" {
			return false
		}
		if f.Category == types.FindingCategoryCIProviderRead {
			continue
		}
		if f.Category != "" || f.Severity != "warning" ||
			!strings.HasPrefix(f.Description, "CI checks could not be read from the provider: ") ||
			!strings.Contains(f.Description, "Verify that the provider CLI or credentials are installed, authenticated, and support the required check-reading command.") {
			return false
		}
	}
	return true
}
