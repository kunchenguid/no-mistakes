package agent

import (
	"fmt"
	"os"
	"path/filepath"
)

// omp (Oh My Pi) gate-instruction neutralization.
//
// Under the trusted project-settings opt-out (disable_project_settings: true)
// the gate must refuse an agent that would load the TARGET repository's own
// project instructions, because the gate runs with the maintainer's
// credentials in the target checkout and a hostile AGENTS.md/CLAUDE.md could
// otherwise redirect it.
//
// omp auto-discovers context files (AGENTS.md, CLAUDE.md, GEMINI.md,
// .omp/AGENTS.md, and other tool conventions), rules, skills, and extensions.
// `--no-context-files`, `--no-rules`, `--no-skills`, and `--no-extensions`
// disable those surfaces. Memory (mnemopi) is a cross-turn surface that can
// retain repo content and recall it later, so a `--config` overlay turns the
// memory backend off. omp applies a CLI `--config` overlay above project
// settings, and no-mistakes appends it last, so the target repo's own
// .omp/config.yml cannot override it.
const ompGateOverlayYAML = `# Written by no-mistakes for a gate run under disable_project_settings: true.
# Turns off mnemopi so repo content the gate reads cannot be retained and later
# recalled around the context-file suppression.
memory:
  backend: off
`

// ompGateSuppressionFlags disable repo-injectable instruction surfaces.
var ompGateSuppressionFlags = []string{"--no-context-files", "--no-rules", "--no-skills", "--no-extensions"}

// writeOMPGateOverlay writes the neutralization overlay to a temp file and
// returns its absolute path. The path is passed as one argv element (no shell
// composition), so spaces and platform path separators are safe.
func writeOMPGateOverlay() (string, error) {
	f, err := os.CreateTemp("", "nm-omp-gate-*.yml")
	if err != nil {
		return "", fmt.Errorf("create omp gate overlay: %w", err)
	}
	path := f.Name()
	if _, err := f.WriteString(ompGateOverlayYAML); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return "", fmt.Errorf("write omp gate overlay: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("close omp gate overlay: %w", err)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("resolve omp gate overlay path: %w", err)
	}
	return abs, nil
}
