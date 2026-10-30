package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/skill"
)

func TestInitReportsExternallyManagedSkills(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		report := skill.InstallReport{Managed: []string{".claude/skills/no-mistakes"}}
		if mixed {
			report.Written = []string{".agents/skills/no-mistakes/SKILL.md"}
		}
		var out bytes.Buffer
		printSkillInstallReport(&out, report, nil)
		if !strings.Contains(out.String(), "skipped externally managed skill: ~/.claude/skills/no-mistakes") {
			t.Fatalf("managed path not reported: %s", out.String())
		}
		if got := strings.Contains(out.String(), "installed for agents at user level"); got != mixed {
			t.Fatalf("installed message = %v for mixed = %v: %s", got, mixed, out.String())
		}
	}
}
