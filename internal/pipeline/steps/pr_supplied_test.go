package steps

import (
	"strings"
	"testing"
)

func TestNormalizeRunPRTitle(t *testing.T) {
	t.Parallel()
	if got, err := NormalizeRunPRTitle("  Add widget  "); err != nil || got != "Add widget" {
		t.Fatalf("title = %q, %v", got, err)
	}
	for name, title := range map[string]string{
		"whitespace":   " \t ",
		"oversized":    strings.Repeat("a", 4097),
		"invalid utf8": "bad \xff",
		"line break":   "first\nsecond",
		"bidi":         "safe ‮ txt",
		"invisible":    "zero​width",
	} {
		if _, err := NormalizeRunPRTitle(title); err == nil {
			t.Errorf("%s title accepted", name)
		}
	}
}

func TestValidateRunPRBody(t *testing.T) {
	t.Parallel()
	if err := ValidateRunPRBody("Our words.\r\n\n\t- item\n"); err != nil {
		t.Fatalf("ordinary Markdown refused: %v", err)
	}
	for name, body := range map[string]string{
		"whitespace":      " \n\t\n",
		"oversized":       strings.Repeat("a", MaxRunPRBodyBytes+1),
		"invalid utf8":    "bad \xff",
		"NUL":             "a\x00b",
		"escape":          "a\x1b[31mred",
		"C1 control":      "a\u0085b",
		"bidi override":   "a‮b",
		"bidi isolate":    "a⁧b",
		"appendix marker": "<!-- no-mistakes-pr-appendix:v1 sha256=x -->",
		"attestation":     pipelineAttestationCommentPrefix + "{} -->",
	} {
		if err := ValidateRunPRBody(body); err == nil {
			t.Errorf("%s body accepted", name)
		}
	}
}
