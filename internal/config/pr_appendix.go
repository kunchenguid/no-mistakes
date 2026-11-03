package config

import "fmt"

const (
	// PRUpdateTextKeep leaves an existing PR title in place and keeps the
	// narrative above the appendix. The appendix and attestation still refresh.
	PRUpdateTextKeep = "keep"
	// PRUpdateTextRegenerate replaces the title and body on update. It is the
	// default when the key is absent or empty.
	PRUpdateTextRegenerate = "regenerate"

	// PRAppendixFull publishes Intent (when enabled), Risk Assessment, Testing,
	// and Pipeline as separate sections. It is the default.
	PRAppendixFull = "full"
	// PRAppendixCollapsed folds Risk Assessment, Testing, and Pipeline into one
	// closed Validation details block. Intent stays outside that block.
	PRAppendixCollapsed = "collapsed"
	// PRAppendixMinimal publishes a one-line risk level and the pipeline
	// attestation, and omits Testing and Pipeline prose.
	PRAppendixMinimal = "minimal"
)

// AppendixMode reports the effective publication mode after config merging.
// An empty value falls back to full when no layer selected a mode. Validation
// rejects every other string at parse time.
func (p PR) AppendixMode() string {
	switch p.Appendix {
	case PRAppendixCollapsed, PRAppendixMinimal, PRAppendixFull:
		return p.Appendix
	default:
		return PRAppendixFull
	}
}

// KeepsUpdateText reports whether an existing PR's title and narrative stay
// in place. Empty and regenerate both rewrite, which is today's behavior.
func (p PR) KeepsUpdateText() bool {
	return p.UpdateText == PRUpdateTextKeep
}

func validatePRUpdateText(value string) error {
	switch value {
	case "", PRUpdateTextKeep, PRUpdateTextRegenerate:
		return nil
	default:
		return fmt.Errorf("pr.update_text: %q is not a valid mode (want %q or %q)", value, PRUpdateTextKeep, PRUpdateTextRegenerate)
	}
}

func validatePRAppendix(mode string) error {
	switch mode {
	case "", PRAppendixFull, PRAppendixCollapsed, PRAppendixMinimal:
		return nil
	default:
		return fmt.Errorf("pr.appendix: %q is not a valid mode (want %q, %q, or %q)", mode, PRAppendixFull, PRAppendixCollapsed, PRAppendixMinimal)
	}
}
