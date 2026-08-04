package agent

import "github.com/kunchenguid/no-mistakes/internal/types"

func copyPurposeModels(models map[types.AgentPurpose]string) map[types.AgentPurpose]string {
	if len(models) == 0 {
		return nil
	}
	copied := make(map[types.AgentPurpose]string, len(models))
	for purpose, model := range models {
		copied[purpose] = model
	}
	return copied
}

// argsWithPurposeModel replaces any operator-level model selection only when a
// purpose route exists, then appends exactly one adapter-native model flag.
// With no route it returns the original slice untouched, preserving historical
// argv byte-for-byte. Removing the old flag instead of relying on CLI-specific
// duplicate-flag precedence makes the purpose override deterministic.
func argsWithPurposeModel(args []string, model, longFlag, shortFlag string) []string {
	if model == "" {
		return args
	}
	out := make([]string, 0, len(args)+2)
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == longFlag || shortFlag != "" && arg == shortFlag {
			if i+1 < len(args) {
				i++
			}
			continue
		}
		if hasFlagValuePrefix(arg, longFlag) || shortFlag != "" && hasFlagValuePrefix(arg, shortFlag) {
			continue
		}
		out = append(out, arg)
	}
	out = append(out, longFlag, model)
	return out
}

func hasFlagValuePrefix(arg, flag string) bool {
	return len(arg) > len(flag)+1 && arg[:len(flag)+1] == flag+"="
}
