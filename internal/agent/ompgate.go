package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// omp (Oh My Pi) gate-instruction neutralization.
//
// no-mistakes drives omp only through ACP (acpx), as the dynamic target
// `acp:omp`. Under the trusted project-settings opt-out
// (disable_project_settings: true) the gate must refuse any harness that would
// load the TARGET repository's own project instructions, because the gate runs
// with the maintainer's credentials in the target checkout and a hostile
// AGENTS.md/CLAUDE.md could otherwise redirect it (the ambient-authority
// incident that this whole boundary exists for - see agent.go).
//
// omp auto-discovers and injects the target repo's project instructions from
// several surfaces. Empirically (a standalone isolation probe against omp
// 18.2.0 driven through `acpx --agent "omp acp ..."`):
//
//   - Context files (AGENTS.md, CLAUDE.md, GEMINI.md, .omp/AGENTS.md, and the
//     other tool conventions omp discovers) are auto-loaded into the agent's
//     project context. omp has NO CLI flag to disable them; `--no-rules` does
//     NOT (it only governs RULES.md/rule files). The only mechanism is the
//     `disabledProviders` setting, which must be supplied through a `--config`
//     overlay. A CLI `--config` overlay is the highest settings layer (above
//     project `.omp/config.yml`) and array settings REPLACE lower layers, so a
//     hostile repo cannot re-enable a provider our overlay disabled.
//   - Rules, skills, and extensions are the remaining repo-injectable
//     instruction surfaces; `--no-rules`, `--no-skills`, and `--no-extensions`
//     disable their discovery.
//   - Memory (mnemopi) is a cross-turn surface omp has and pi does not: a gate
//     turn that reads the repo's AGENTS.md can auto-retain it, and a later turn
//     or run auto-recalls it as trusted operator memory - around the provider
//     suppression. `memory.backend: off` in the overlay turns it off.
//
// So the neutralized launch is `omp acp` plus those three flags plus a
// `--config` overlay that disables every context-file discovery provider and
// mnemopi memory. The probe confirmed: with providers enabled omp answers a
// codename planted only in the repo's AGENTS.md; with this overlay applied it
// does not, even when the repo's own .omp/config.yml tries to set
// `disabledProviders: []`.
const ompACPTarget = "omp"

// ompGateOverlayYAML disables every omp surface that could carry the target
// repository's own project instructions into the gate agent:
//
//   - disabledProviders removes every discovery provider that contributes a
//     context file (AGENTS.md/CLAUDE.md/GEMINI.md and the other tool
//     conventions). Disabling the whole provider is the fail-safe choice for a
//     security boundary: it drops the context file plus anything else that
//     provider would inject (MCP servers, skills, hooks, commands, settings),
//     and it stays correct as omp adds new context-file names under an existing
//     provider. The list is the full set of context-file-contributing providers
//     documented by omp's context-files reference.
//   - memory.backend: off disables mnemopi. This is not defense-in-depth
//     theater: empirically, with memory left on, a gate turn that reads the
//     repo's AGENTS.md while reviewing can auto-retain it into the operator's
//     global memory, and a later turn or run then auto-recalls it as trusted
//     operator memory - a cross-turn path around disabledProviders that omp has
//     and pi does not. A validation gate running with owner credentials must be
//     stateless with respect to that memory, so the overlay turns it off.
const ompGateOverlayYAML = `# Written by no-mistakes for a gate run under disable_project_settings: true.
# Neutralizes the target repository's project instructions by disabling every
# omp discovery provider that contributes a context file (AGENTS.md/CLAUDE.md/
# GEMINI.md, .omp/AGENTS.md, and the other supported conventions) and by turning
# off mnemopi memory so repo content the gate reads cannot be retained and later
# recalled around that suppression. A CLI --config overlay is the highest
# settings layer, so the target repo's own .omp/config.yml cannot override it.
disabledProviders:
  - native
  - claude
  - codex
  - gemini
  - opencode
  - github
  - agents
  - agents-md
  - claude-md
memory:
  backend: off
`

// ompGateSuppressionFlags disable the remaining repo-injectable instruction
// surfaces that are governed by CLI flags rather than by discovery providers.
var ompGateSuppressionFlags = []string{"--no-rules", "--no-skills", "--no-extensions"}

// neutralizesOMPGate reports whether an acpx invocation for the given target
// will launch omp with the target repository's project instructions
// neutralized. It fails closed: it is true ONLY for the omp target, only under
// the trusted opt-out, and only for the default launch (no operator ACP
// raw-command override). A custom raw command is opaque - we cannot prove it
// still applies the suppression overlay and flags - so it reports false and the
// gate refuses it rather than launching omp with project instructions loaded.
func neutralizesOMPGate(target, rawCommand string, disableProjectSettings bool) bool {
	return disableProjectSettings &&
		target == ompACPTarget &&
		strings.TrimSpace(rawCommand) == ""
}

// ompNeutralizedACPCommand is the acpx `--agent` raw command that launches omp
// as an ACP server with every project-instruction surface neutralized: the
// context-file providers via the --config overlay, and rules/skills/extensions
// via CLI flags. overlayPath must be a filesystem path free of shell
// metacharacters, which writeOMPGateOverlay guarantees.
func ompNeutralizedACPCommand(overlayPath string) string {
	parts := make([]string, 0, 4+len(ompGateSuppressionFlags))
	parts = append(parts, "omp", "acp", "--config", overlayPath)
	parts = append(parts, ompGateSuppressionFlags...)
	return strings.Join(parts, " ")
}

// writeOMPGateOverlay writes the neutralization overlay to a temp file and
// returns its ABSOLUTE path. The path must be absolute because acpx composes it
// into the `--agent` command string while omp resolves `--config` relative to
// the gate's working directory (the target checkout), not acpx's - so a relative
// overlay path (from a relative TMPDIR) would resolve against the wrong
// directory and miss. omp fails closed on a missing/unreadable overlay (it exits
// with "Config overlay not found" before the ACP session initializes, verified
// against omp 18.2.0), so a miss aborts the run rather than silently launching
// omp with project instructions loaded; the absolute path is what makes the
// intended overlay actually load in the first place.
//
// The path must also compose safely into acpx's whitespace-split `--agent`
// command: whitespace and quotes are genuinely unsafe on every OS. A backslash
// is the normal Windows path separator (every Windows temp path contains one),
// not a shell metacharacter, so it is rejected only off Windows, where a
// backslash in a path is anomalous. os.CreateTemp names add only
// [A-Za-z0-9_]-class characters, so a remaining unsafe character would be a
// platform anomaly; fail closed if one appears.
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
	if !overlayPathShellSafe(abs, runtime.GOOS) {
		_ = os.Remove(abs)
		return "", fmt.Errorf("omp gate overlay path is not shell-safe: %q", abs)
	}
	return abs, nil
}

// overlayPathShellSafe reports whether path composes safely into acpx's
// whitespace-split `--agent` command. Whitespace and quotes are unsafe on every
// OS. A backslash is the normal Windows path separator (every Windows temp path
// has one), not a metacharacter, so it is unsafe only off Windows, where a
// backslash in a path is anomalous. goos is passed explicitly so both branches
// are testable without the target platform.
func overlayPathShellSafe(path, goos string) bool {
	unsafe := " \t\r\n\"'"
	if goos != "windows" {
		unsafe += "\\"
	}
	return !strings.ContainsAny(path, unsafe)
}
