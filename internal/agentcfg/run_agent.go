package agentcfg

import (
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// ValidateRunAgent accepts an explicit built-in backend, never auto-detection,
// a fallback list, an executable path, or a caller-supplied ACP command.
func ValidateRunAgent(name types.AgentName) error {
	switch name {
	case "", types.AgentClaude, types.AgentCodex, types.AgentGrok, types.AgentRovoDev,
		types.AgentOpenCode, types.AgentPi, types.AgentCopilot, types.AgentAntigravity,
		types.AgentCursor, types.AgentDevin:
		return nil
	default:
		return fmt.Errorf("--agent requires a supported explicit backend (claude, codex, grok, rovodev, opencode, pi, copilot, antigravity, cursor, devin)")
	}
}
