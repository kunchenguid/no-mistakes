package steps

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const (
	oldCallout = "## Not validated live\n\n⚠️ **1 of 3 scenarios were not driven live against the product.**\n\n- **Untested:** login - no staging account"
	newCallout = "## Not validated live\n\n⚠️ **1 of 3 scenarios were not driven live against the product. 1 exception or skip let this run continue without that validation.**\n\n- **Untested:** login - no staging account\n- **Skipped by operator** (`ci`): no reason recorded"
	intentLead = "## Intent\n\nShip it.\n\nSecond paragraph."
	// What follows the lead differs by appendix mode, and none of it starts
	// with a "## " heading in minimal or collapsed bodies.
	minimalTailText   = "🟢 Low risk: synthetic\n\n<!-- no-mistakes-pipeline-attestation: {\"head_sha\":\"abc\"} -->\n"
	collapsedTailText = "## What Changed\n\n- adds a flag\n\n<details>\n<summary>Validation</summary>\n\n## Testing\n\n- ok\n\n</details>\n"
)

var exactIntent = publishedIntent{exact: intentLead}

func TestSpliceValidationCallout(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, text, callout, want string
		intent                    publishedIntent
	}{
		{
			name:    "minimal: the risk line and attestation after the callout are kept",
			text:    intentLead + "\n\n" + oldCallout + "\n\n" + minimalTailText,
			callout: newCallout,
			intent:  exactIntent,
			want:    intentLead + "\n\n" + newCallout + "\n\n" + minimalTailText,
		},
		{
			name:    "collapsed: the fold after the callout is kept",
			text:    intentLead + "\n\n" + oldCallout + "\n\n" + collapsedTailText,
			callout: newCallout,
			intent:  exactIntent,
			want:    intentLead + "\n\n" + newCallout + "\n\n" + collapsedTailText,
		},
		{
			name:    "inserts after a multi-paragraph Intent",
			text:    intentLead + "\n\n" + minimalTailText,
			callout: newCallout,
			intent:  exactIntent,
			want:    intentLead + "\n\n" + newCallout + "\n\n" + minimalTailText,
		},
		{
			name:    "inserts at the top when no Intent was published",
			text:    minimalTailText,
			callout: newCallout,
			want:    newCallout + "\n\n" + minimalTailText,
		},
		{
			name:    "replaces at the top when no Intent was published",
			text:    oldCallout + "\n\n" + collapsedTailText,
			callout: newCallout,
			want:    newCallout + "\n\n" + collapsedTailText,
		},
		{
			name:    "an unchanged callout splices back byte for byte",
			text:    intentLead + "\n\n" + oldCallout + "\n\n" + minimalTailText,
			callout: oldCallout,
			intent:  exactIntent,
			want:    intentLead + "\n\n" + oldCallout + "\n\n" + minimalTailText,
		},
		{
			name:    "a brief Intent is found by its shape in configured order",
			text:    "## Intent\n\nGoal line.\n\n### Scope\n- a\n- b\n\n### Why\n- c\n\n" + minimalTailText,
			callout: newCallout,
			intent:  publishedIntent{brief: true},
			want:    "## Intent\n\nGoal line.\n\n### Scope\n- a\n- b\n\n### Why\n- c\n\n" + newCallout + "\n\n" + minimalTailText,
		},
		{
			name:    "nothing to add leaves the text alone",
			text:    intentLead + "\n\n" + minimalTailText,
			callout: "",
			intent:  exactIntent,
			want:    intentLead + "\n\n" + minimalTailText,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := spliceValidationCallout(tc.text, tc.callout, tc.intent)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("spliceValidationCallout() =\n%q\nwant\n%q", got, tc.want)
			}
		})
	}
}

func TestSpliceValidationCallout_RefusesAnUnexpectedLead(t *testing.T) {
	t.Parallel()
	for name, text := range map[string]string{
		"edited Intent":     "## Intent\n\nShip it now.\n\n" + minimalTailText,
		"malformed callout": intentLead + "\n\n" + notValidatedLiveHeading + "\n\n- **Untested:** login - no summary\n\n" + minimalTailText,
	} {
		if _, err := spliceValidationCallout(text, newCallout, exactIntent); err == nil {
			t.Errorf("%s: splice rewrote a body whose lead it cannot bound", name)
		}
	}
}

// publishedBodyRefresh builds the body the PR step publishes for mode, then
// records an operator skip at the CI gate and returns the published body, the
// callouts the run had before and after that skip, and the body the CI step's
// refresh produces.
func publishedBodyRefresh(t *testing.T, mode string, keep, untested bool) (before, oldCallout, newCallout, refreshed string, sctx *pipeline.StepContext) {
	t.Helper()
	sctx = newCalloutContext(t, mode, untested, untested)
	if keep {
		sctx.Config.PR.UpdateText = config.PRUpdateTextKeep
	}
	content, err := (&PRStep{}).buildPRContent(sctx, "feature", "main", sctx.Run.BaseSHA, scm.ProviderGitHub, 0)
	if err != nil {
		t.Fatal(err)
	}
	if oldCallout, err = validationCallout(sctx, scm.ProviderGitHub); err != nil {
		t.Fatal(err)
	}
	ci, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepCI)
	if err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.CompleteSkippedStep(ci.ID, 0, 0, "", types.OperatorSkipReason); err != nil {
		t.Fatal(err)
	}
	if newCallout, err = validationCallout(sctx, scm.ProviderGitHub); err != nil {
		t.Fatal(err)
	}
	refresh, err := validationCalloutRefresh(sctx, scm.ProviderGitHub, 0)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed, err = refresh(content.Body); err != nil {
		t.Fatalf("refresh %s body: %v\n%s", mode, err, content.Body)
	}
	return content.Body, oldCallout, newCallout, refreshed, sctx
}

// Every byte outside the callout must survive the refresh in every published
// body shape, and the attestation must still bind the head.
func TestValidationCalloutRefresh_PublishedBodies(t *testing.T) {
	t.Parallel()
	const intent = "## Intent\n\nLet a customer refund an order."
	for _, tc := range []struct {
		name string
		mode string
		keep bool
	}{
		{"default", config.PRAppendixFull, false},
		{"collapsed", config.PRAppendixCollapsed, false},
		{"minimal", config.PRAppendixMinimal, false},
		{"keep collapsed", config.PRAppendixCollapsed, true},
		{"keep minimal", config.PRAppendixMinimal, true},
	} {
		for _, untested := range []bool{true, false} {
			name := tc.name + "/replace"
			if !untested {
				name = tc.name + "/insert"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				before, oldCallout, newCallout, refreshed, sctx := publishedBodyRefresh(t, tc.mode, tc.keep, untested)
				t.Logf("before:\n%s\nafter:\n%s", before, refreshed)
				// The oracle: the published text with only the callout
				// replaced, or inserted directly after the Intent section.
				expect := func(text string) string {
					if untested {
						if !strings.Contains(text, oldCallout+"\n\n") {
							t.Fatalf("published text does not carry the run's callout:\n%s", text)
						}
						return strings.Replace(text, oldCallout, newCallout, 1)
					}
					return strings.Replace(text, intent+"\n\n", intent+"\n\n"+newCallout+"\n\n", 1)
				}
				if tc.keep {
					orig, err := parsePROwnedBody(before)
					if err != nil {
						t.Fatal(err)
					}
					got, err := parsePROwnedBody(refreshed)
					if err != nil {
						t.Fatalf("refreshed owned body no longer verifies: %v", err)
					}
					if got.before != orig.before || got.after != orig.after || got.appendix != expect(orig.appendix) {
						t.Fatalf("refresh changed more than the callout:\nwant appendix\n%s\ngot\n%s", expect(orig.appendix), got.appendix)
					}
				} else if want := expect(before); refreshed != want {
					t.Fatalf("refresh changed more than the callout:\nwant\n%s\ngot\n%s", want, refreshed)
				}
				assertFirstAttestationBindsHead(t, refreshed, sctx.Run.HeadSHA)
			})
		}
	}
}

func ownedCalloutBody(t *testing.T, appendixLead string) string {
	t.Helper()
	appendix := appendixLead + "\n\n" + pipelineAttestationCommentPrefix + `{"head_sha":"` + strings.Repeat("a", 40) + `","steps":[]}` + pipelineAttestationCommentClosingToken
	return "## Overview\n\nAuthor text\n\n" + wrapPRAppendix(appendix) + "\nAfter appendix: human note"
}

func TestRefreshValidationCallout_RefusesAnEditedAppendix(t *testing.T) {
	t.Parallel()
	body := strings.Replace(ownedCalloutBody(t, intentLead), "Ship it.", "Ship it now.", 1)
	if _, err := refreshValidationCallout(body, newCallout, exactIntent, 0); err == nil {
		t.Fatal("refresh rewrote an appendix someone else edited")
	}
}

func TestRefreshValidationCallout_RefusesABodyOverTheProviderLimit(t *testing.T) {
	t.Parallel()
	body := intentLead + "\n\n" + minimalTailText
	if _, err := refreshValidationCallout(body, newCallout, exactIntent, len([]rune(body))+10); err == nil {
		t.Fatal("refresh accepted a body the provider would truncate")
	}
	if got, err := refreshValidationCallout(body, newCallout, exactIntent, 0); err != nil || !strings.Contains(got, newCallout) {
		t.Fatalf("unlimited refresh = %q, %v", got, err)
	}
}
