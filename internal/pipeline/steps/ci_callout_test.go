package steps

import (
	"strings"
	"testing"
)

const (
	oldCallout = "## Not validated live\n\n⚠️ **1 of 3 scenarios were not driven live against the product.**\n\n- **Untested:** login - no staging account"
	newCallout = "## Not validated live\n\n⚠️ **1 of 3 scenarios were not driven live against the product. 1 exception or skip let this run continue without that validation.**\n\n- **Untested:** login - no staging account\n- **Skipped by operator** (`ci`): no reason recorded"
	restOfBody = "## What Changed\n\n- adds a flag\n\n## Pipeline\n\n<!-- marker -->\n"
)

func TestSpliceValidationCallout(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, text, callout, want string
	}{
		{
			name:    "replaces the published callout",
			text:    "## Intent\n\nShip it.\n\n" + oldCallout + "\n\n" + restOfBody,
			callout: newCallout,
			want:    "## Intent\n\nShip it.\n\n" + newCallout + "\n\n" + restOfBody,
		},
		{
			name:    "inserts after Intent when the run had no earlier gap",
			text:    "## Intent\n\nShip it.\n\n" + restOfBody,
			callout: newCallout,
			want:    "## Intent\n\nShip it.\n\n" + newCallout + "\n\n" + restOfBody,
		},
		{
			name:    "inserts at the top without an Intent section",
			text:    restOfBody,
			callout: newCallout,
			want:    newCallout + "\n\n" + restOfBody,
		},
		{
			name:    "an unchanged callout splices back byte for byte",
			text:    "## Intent\n\nShip it.\n\n" + oldCallout + "\n\n" + restOfBody,
			callout: oldCallout,
			want:    "## Intent\n\nShip it.\n\n" + oldCallout + "\n\n" + restOfBody,
		},
		{
			name:    "a callout at the end of the text",
			text:    "## Intent\n\nShip it.\n\n" + oldCallout,
			callout: newCallout,
			want:    "## Intent\n\nShip it.\n\n" + newCallout,
		},
		{
			name:    "a heading inside a fence in Intent does not end the section",
			text:    "## Intent\n\n```md\n## What Changed\n```\n\n" + restOfBody,
			callout: newCallout,
			want:    "## Intent\n\n```md\n## What Changed\n```\n\n" + newCallout + "\n\n" + restOfBody,
		},
		{
			name:    "nothing to add leaves the text alone",
			text:    "## Intent\n\nShip it.\n\n" + restOfBody,
			callout: "",
			want:    "## Intent\n\nShip it.\n\n" + restOfBody,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := spliceValidationCallout(tc.text, tc.callout); got != tc.want {
				t.Fatalf("spliceValidationCallout() =\n%q\nwant\n%q", got, tc.want)
			}
		})
	}
}

func ownedCalloutBody(t *testing.T, appendixLead string) string {
	t.Helper()
	appendix := appendixLead + "\n\n## Pipeline\n\n" + pipelineAttestationCommentPrefix + `{"head_sha":"` + strings.Repeat("a", 40) + `","steps":[]}` + pipelineAttestationCommentClosingToken
	return "## Overview\n\nAuthor text\n\n" + wrapPRAppendix(appendix) + "\nAfter appendix: human note"
}

func TestRefreshValidationCallout_OwnedAppendixKeepsAuthorTextAndAValidDigest(t *testing.T) {
	t.Parallel()
	body := ownedCalloutBody(t, "## Intent\n\nShip it.")
	updated, err := refreshValidationCallout(body, newCallout, 0)
	if err != nil {
		t.Fatal(err)
	}
	parts, err := parsePROwnedBody(updated)
	if err != nil {
		t.Fatalf("refreshed owned body no longer verifies: %v\n%s", err, updated)
	}
	if parts.before != "## Overview\n\nAuthor text\n\n" || parts.after != "\nAfter appendix: human note" {
		t.Fatalf("author text changed: before=%q after=%q", parts.before, parts.after)
	}
	if !strings.HasPrefix(parts.appendix, "## Intent\n\nShip it.\n\n"+newCallout+"\n\n## Pipeline") {
		t.Fatalf("appendix = %q, want the callout after Intent", parts.appendix)
	}
	again, err := refreshValidationCallout(updated, newCallout, 0)
	if err != nil || again != updated {
		t.Fatalf("second refresh changed the body (err %v)", err)
	}
}

func TestRefreshValidationCallout_RefusesAnEditedAppendix(t *testing.T) {
	t.Parallel()
	body := strings.Replace(ownedCalloutBody(t, "## Intent\n\nShip it."), "Ship it.", "Ship it now.", 1)
	if _, err := refreshValidationCallout(body, newCallout, 0); err == nil {
		t.Fatal("refresh rewrote an appendix someone else edited")
	}
}

func TestRefreshValidationCallout_RefusesABodyOverTheProviderLimit(t *testing.T) {
	t.Parallel()
	body := "## Intent\n\nShip it.\n\n" + restOfBody
	if _, err := refreshValidationCallout(body, newCallout, len([]rune(body))+10); err == nil {
		t.Fatal("refresh accepted a body the provider would truncate")
	}
	if got, err := refreshValidationCallout(body, newCallout, 0); err != nil || !strings.Contains(got, newCallout) {
		t.Fatalf("unlimited refresh = %q, %v", got, err)
	}
}
