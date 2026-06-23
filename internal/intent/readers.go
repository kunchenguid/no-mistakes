package intent

// AllReaders returns the default set of agent transcript readers, minus any
// the caller has disabled by name. Disabled names are matched
// case-insensitively against each reader's Name().
func AllReaders(disabled map[string]bool) []Reader {
	all := []Reader{
		NewClaudeReader(),
		NewCodexReader(),
		NewOpenCodeReader(),
		NewRovoDevReader(),
		NewPiReader(),
		NewCopilotReader(),
	}
	if len(disabled) == 0 {
		return all
	}
	out := make([]Reader, 0, len(all))
	for _, r := range all {
		if disabled[r.Name()] {
			continue
		}
		out = append(out, r)
	}
	return out
}
