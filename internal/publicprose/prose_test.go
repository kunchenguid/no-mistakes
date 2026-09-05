package publicprose

import "testing"

func TestText_PreservesEvidence(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct{ name, input, want string }{
		{"subject", "fix: Captain: restore reclamation", "fix: restore reclamation"},
		{"risk", "✅ Low: Captain, the change is bounded", "✅ Low: the change is bounded"},
		{"mixed case", "✅ Low: cApTaIn, the change is bounded", "✅ Low: cApTaIn, the change is bounded"},
		{"upper case", "CAPTAIN: the change is bounded", "CAPTAIN: the change is bounded"},
		{"lowercase roles", "Roles: captain, crew and ship.", "Roles: captain, crew and ship."},
		{"lowercase risk", "✅ Low: captain, crew and ship selection is unchanged", "✅ Low: captain, crew and ship selection is unchanged"},
		{"error prefix", "fails with: captain: undefined", "fails with: captain: undefined"},
		{"bold term label", "- **captain**: now persisted across sessions", "- **captain**: now persisted across sessions"},
		{"capitalized bold term label", "- **Captain**: the role that owns the ship", "- **Captain**: the role that owns the ship"},
		{"bold label sentence", "**Note:** Captain, fix this", "**Note:** fix this"},
		{"bold label testing", "**Testing:** Captain, ran the suite", "**Testing:** ran the suite"},
		{"finding", "- ⚠️ Captain, guard stale wakes", "- ⚠️ guard stale wakes"},
		{"ordered paren item", "1) Captain, guard stale wakes", "1) guard stale wakes"},
		{"ordered dot item", "12. Captain, guard stale wakes", "12. guard stale wakes"},
		{"sentences", "Tests passed. Captain, checks are complete.", "Tests passed. checks are complete."},
		{"domain", "The captain, crew and ship remain unchanged.", "The captain, crew and ship remain unchanged."},
		{"quoted", `Keep "Captain, ready" and 'Captain: ready' dialogue.`, `Keep "Captain, ready" and 'Captain: ready' dialogue.`},
		{"curly quotes", "Keep “Captain, ready” and ‘Captain: ready’ dialogue.", "Keep “Captain, ready” and ‘Captain: ready’ dialogue."},
		{"escaped quotes", "Keep &#34;Captain, ready&#34; dialogue.", "Keep &#34;Captain, ready&#34; dialogue."},
		{"inline code", "Captain, keep ``Captain: `ready` `` intact", "keep ``Captain: `ready` `` intact"},
		{"fenced code", "````diff\n+Captain: fixture\n```\nCaptain, still code\n````\nCaptain, done", "````diff\n+Captain: fixture\n```\nCaptain, still code\n````\ndone"},
		{"tilde fence", "~~~text\nCaptain, fixture\n~~~\nCaptain, done", "~~~text\nCaptain, fixture\n~~~\ndone"},
		{"unclosed fence", "```text\nCaptain, fixture", "```text\nCaptain, fixture"},
		{"blockquote", "> Captain, quoted\nCaptain, continued quote\n\nCaptain, done", "> Captain, quoted\nCaptain, continued quote\n\ndone"},
		{"indented code", "    Captain: fixture\n\nCaptain, done", "    Captain: fixture\n\ndone"},
		{"html code", "<pre><code>Captain: fixture\nCaptain, code</code></pre>\nCaptain, done", "<pre><code>Captain: fixture\nCaptain, code</code></pre>\ndone"},
		{"html attribute", `<a title="Captain: fixture">link</a>`, `<a title="Captain: fixture">link</a>`},
		{"html prose", "<details>\n<summary>Captain, fix summary</summary>\n\nCaptain, fixed\n</details>", "<details>\n<summary>fix summary</summary>\n\nfixed\n</details>"},
		{"comment", "<!-- Captain: recorded -->\nCaptain, done", "<!-- Captain: recorded -->\ndone"},
		{"unmatched tick in dialogue", "\"Captain, type a ` character.\"\n\nCaptain, done", "\"Captain, type a ` character.\"\n\ndone"},
		{"markup in inline code", "`<code> Captain: literal`\n\nCaptain, done", "`<code> Captain: literal`\n\ndone"},
		{"inline span cannot cross block code", "A lone ` precedes code.\n\n```text\n`Captain: literal\n```\nCaptain, done", "A lone ` precedes code.\n\n```text\n`Captain: literal\n```\ndone"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := Text(tt.input)
			if got != tt.want {
				t.Errorf("body = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestText_BlockquoteBoundaries(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct{ name, input, want string }{
		{"reported heading", "> note\n## Captain, next steps", "> note\n## next steps"},
		{"heading and following prose", "> Captain, quoted\n## Captain, next steps\nCaptain: finish cleanup", "> Captain, quoted\n## next steps\nfinish cleanup"},
		{"bullet list", "> Captain, quoted\n- Captain, next steps", "> Captain, quoted\n- next steps"},
		{"ordered list", "> Captain, quoted\n1. Captain, next steps", "> Captain, quoted\n1. next steps"},
		{"fence and following prose", "> note\n```text\nCaptain, literal\n```\nCaptain: finish cleanup", "> note\n```text\nCaptain, literal\n```\nfinish cleanup"},
		{"paragraph continuation", "> Captain, quoted\nCaptain: continued quote", "> Captain, quoted\nCaptain: continued quote"},
		{"explicit quoted blocks", "> Captain, quoted\n> ## Captain, quoted heading\n> - Captain: quoted item", "> Captain, quoted\n> ## Captain, quoted heading\n> - Captain: quoted item"},
		{"noninterrupting number", "> note\n2. Captain, quoted continuation", "> note\n2. Captain, quoted continuation"},
		{"nonheading hash", "> note\n##Captain, quoted continuation", "> note\n##Captain, quoted continuation"},
		{"html pre block", "> Captain, quoted\n<pre>Captain: literal</pre>\nCaptain, done", "> Captain, quoted\n<pre>Captain: literal</pre>\ndone"},
		{"html comment", "> Captain, quoted\n<!-- note -->\nCaptain, done", "> Captain, quoted\n<!-- note -->\ndone"},
		{"html processing instruction", "> Captain, quoted\n<?instruction?>\nCaptain, done", "> Captain, quoted\n<?instruction?>\ndone"},
		{"html declaration", "> Captain, quoted\n<!DOCTYPE html>\nCaptain, done", "> Captain, quoted\n<!DOCTYPE html>\ndone"},
		{"html cdata", "> Captain, quoted\n<![CDATA[note]]>\nCaptain, done", "> Captain, quoted\n<![CDATA[note]]>\ndone"},
		{"html closing block tag", "> Captain, quoted\n</div>\nCaptain, done", "> Captain, quoted\n</div>\ndone"},
		{"html type seven", "> Captain, quoted\n<custom-tag>\nCaptain, continued quote", "> Captain, quoted\n<custom-tag>\nCaptain, continued quote"},
		{"html closing pre is type seven", "> Captain, quoted\n</pre>\nCaptain, continued quote", "> Captain, quoted\n</pre>\nCaptain, continued quote"},
		{"indented html continuation", "> Captain, quoted\n    <div>\nCaptain, continued quote", "> Captain, quoted\n    <div>\nCaptain, continued quote"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := Text(tt.input); got != tt.want {
				t.Errorf("body = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestText_InlineCodeBlockBoundaries(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct{ name, input, want string }{
		{"paragraph", "A literal ` character.\n\nCaptain, run `go test`.", "A literal ` character.\n\nrun `go test`."},
		{"heading", "A literal ` character.\n## Captain, run `go test`.", "A literal ` character.\n## run `go test`."},
		{"after heading", "## A literal ` character.\nCaptain, run `go test`.", "## A literal ` character.\nrun `go test`."},
		{"list item", "- A literal ` character.\n- Captain, run `go test`.", "- A literal ` character.\n- run `go test`."},
		{"ordered list item", "1. A literal ` character.\n2. Captain, run `go test`.", "1. A literal ` character.\n2. run `go test`."},
		{"ordered list high start", "9. A literal ` character.\n10. Captain, run `go test`.", "9. A literal ` character.\n10. run `go test`."},
		{"nested ordered list item", "1. outer\n   1. A literal ` character.\n   2. Captain, run `go test`.", "1. outer\n   1. A literal ` character.\n   2. run `go test`."},
		{"nested heading", "- outer\n  - A literal ` character.\n    ## Captain, run `go test`.\n    Captain, done", "- outer\n  - A literal ` character.\n    ## run `go test`.\n    done"},
		{"after list item heading", "- ## A literal ` character.\n  Captain, run `go test`.", "- ## A literal ` character.\n  run `go test`."},
		{"thematic break", "A literal ` character.\n---\nCaptain, run `go test`.", "A literal ` character.\n---\nrun `go test`."},
		{"html block", "A literal ` character.\n<div>Captain, run `go test`.</div>", "A literal ` character.\n<div>run `go test`.</div>"},
		{"soft wrapped span", "Keep `literal\nCaptain: ready` intact.\n\nCaptain, done", "Keep `literal\nCaptain: ready` intact.\n\ndone"},
		{"soft wrapped markup in span", "Keep `literal\n<em>Captain: ready</em>` intact.\n\nCaptain, done", "Keep `literal\n<em>Captain: ready</em>` intact.\n\ndone"},
		{"type seven inside span", "Keep `literal\n<br>\nCaptain: ready` intact.\n\nCaptain, done", "Keep `literal\n<br>\nCaptain: ready` intact.\n\ndone"},
		{"noninterrupting number inside span", "Keep `literal\n2. Captain: ready` intact.\n\nCaptain, done", "Keep `literal\n2. Captain: ready` intact.\n\ndone"},
		{"successive noninterrupting numbers inside span", "Keep `literal\n2. still literal\n3. Captain: ready` intact.\n\nCaptain, done", "Keep `literal\n2. still literal\n3. Captain: ready` intact.\n\ndone"},
		{"noninterrupting number inside list span", "- Keep `literal\n  2. Captain: ready` intact.\n\nCaptain, done", "- Keep `literal\n  2. Captain: ready` intact.\n\ndone"},
		{"noninterrupting number inside nested list span", "- outer\n  - Keep `literal\n    2) Captain: ready` intact.\n\nCaptain, done", "- outer\n  - Keep `literal\n    2) Captain: ready` intact.\n\ndone"},
		{"noninterrupting number before hashes inside list span", "- Keep `literal\n  2. ## still literal\n  Captain: ready` intact.\n\nCaptain, done", "- Keep `literal\n  2. ## still literal\n  Captain: ready` intact.\n\ndone"},
		{"nested ordered list interrupts at one", "- outer\n  - A literal ` character.\n    1. Captain, run `go test`.", "- outer\n  - A literal ` character.\n    1. run `go test`."},
		{"nested ordered list after blank", "- A literal ` character.\n\n  2. Captain, run `go test`.", "- A literal ` character.\n\n  2. run `go test`."},
		{"nested ordered heading after blank", "- A literal ` character.\n\n  2. ## Captain, run `go test`.", "- A literal ` character.\n\n  2. ## run `go test`."},
		{"nested ordered sibling heading", "- outer\n  1. A literal ` character.\n  2. ## Captain, run `go test`.", "- outer\n  1. A literal ` character.\n  2. ## run `go test`."},
		{"ordered heading inside list span", "- A literal ` character.\n  2. ## Captain, run `go test`.", "- A literal ` character.\n  2. ## Captain, run `go test`."},
		{"ordered heading inside nested list span", "- outer\n  - A literal ` character.\n    2. ## Captain, run `go test`.", "- outer\n  - A literal ` character.\n    2. ## Captain, run `go test`."},
		{"multiline html code", "<code>literal\n\nCaptain: ready\n</code>\nCaptain, done", "<code>literal\n\nCaptain: ready\n</code>\ndone"},
		{"multiline html comment", "<!-- literal ` character\n\nCaptain: ready ` -->\nCaptain, done", "<!-- literal ` character\n\nCaptain: ready ` -->\ndone"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := Text(tt.input); got != tt.want {
				t.Errorf("body = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestText_QuoteBlockBoundaries(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct{ name, input, want string }{
		{
			"unbalanced intent quote",
			"Fix the \"flaky test\n\n## What Changed\n\n- Captain, guard wakes\n\n✅ Low: Captain, bounded. The \"foo\" helper. Captain, done",
			"Fix the \"flaky test\n\n## What Changed\n\n- guard wakes\n\n✅ Low: bounded. The \"foo\" helper. done",
		},
		{"inch mark bullet", "- Support 5\" displays\n- Captain, guard wakes\n\n✅ Low: Captain, bounded. The \"foo\" helper.", "- Support 5\" displays\n- guard wakes\n\n✅ Low: bounded. The \"foo\" helper."},
		{"unbalanced quote before heading", "An odd \" mark\n## Captain, next steps \"x\"", "An odd \" mark\n## next steps \"x\""},
		{"unbalanced quote before thematic break", "An odd \" mark\n---\nCaptain, next \"x\"", "An odd \" mark\n---\nnext \"x\""},
		{"unbalanced curly quote before blank line", "An odd “ mark\n\nCaptain, next ”x”", "An odd “ mark\n\nnext ”x”"},
		{"unbalanced quote before fence", "An odd \" mark\n```text\nCaptain: literal\n```\nCaptain, next \"x\"", "An odd \" mark\n```text\nCaptain: literal\n```\nnext \"x\""},
		{"unbalanced quote before blockquote", "An odd \" mark\n> Captain, quoted\n\nCaptain, next \"x\"", "An odd \" mark\n> Captain, quoted\n\nnext \"x\""},
		{"soft wrapped dialogue", "Keep \"Captain, ready\nand waiting\" dialogue.\n\nCaptain, done", "Keep \"Captain, ready\nand waiting\" dialogue.\n\ndone"},
		{"dialogue after reset", "An odd \" mark\n\nKeep \"Captain, ready\" dialogue. Captain, done", "An odd \" mark\n\nKeep \"Captain, ready\" dialogue. done"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := Text(tt.input); got != tt.want {
				t.Errorf("body = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestText_QuoteBlockStarts(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct{ name, input, want string }{
		{"blank line", "An odd \" mark\n\nCaptain, next \"x\"", "An odd \" mark\n\nnext \"x\""},
		{"heading line", "An odd \" mark\n## Captain, next \"x\"", "An odd \" mark\n## next \"x\""},
		{"line after heading", "## Odd \" heading\nCaptain, next \"x\"", "## Odd \" heading\nnext \"x\""},
		{"line after indented heading", "   # Odd \" heading\nCaptain, next \"x\"", "   # Odd \" heading\nnext \"x\""},
		{"dash bullet", "- Support 5\" displays\n- Captain, guard wakes. The \"foo\" helper. Captain, done", "- Support 5\" displays\n- guard wakes. The \"foo\" helper. done"},
		{"plus bullet", "+ Support 5\" displays\n+ Captain, guard wakes. The \"foo\" helper. Captain, done", "+ Support 5\" displays\n+ guard wakes. The \"foo\" helper. done"},
		{"star bullet", "* Support 5\" displays\n* Captain, guard wakes. The \"foo\" helper. Captain, done", "* Support 5\" displays\n* guard wakes. The \"foo\" helper. done"},
		{"ordered dot", "1. Support 5\" displays\n2. Captain, guard wakes. The \"foo\" helper. Captain, done", "1. Support 5\" displays\n2. guard wakes. The \"foo\" helper. done"},
		{"ordered paren", "1) Support 5\" displays\n2) Captain, guard wakes. The \"foo\" helper. Captain, done", "1) Support 5\" displays\n2) guard wakes. The \"foo\" helper. done"},
		{"ordered high start", "9. Support 5\" displays\n10. Captain, guard wakes. The \"foo\" helper.", "9. Support 5\" displays\n10. guard wakes. The \"foo\" helper."},
		{"indented bullet", "- Support 5\" displays\n   - Captain, guard wakes. The \"foo\" helper.", "- Support 5\" displays\n   - guard wakes. The \"foo\" helper."},
		{"four space nested bullet", "- Support 5\" displays\n    - Captain, nested item", "- Support 5\" displays\n    - nested item"},
		{"tab nested bullet", "- Support 5\" displays\n\t- Captain, nested item", "- Support 5\" displays\n\t- nested item"},
		{"four space nested ordered", "1. Support 5\" displays\n    2. Captain, nested item", "1. Support 5\" displays\n    2. nested item"},
		{"nested bullet after blank", "- Support 5\" displays\n\n    - Captain, nested item", "- Support 5\" displays\n\n    - nested item"},
		{"nested paragraph", "- Support 5\" displays\n\n    Captain, nested paragraph", "- Support 5\" displays\n\n    nested paragraph"},
		{"code inside list item", "- item\n\n      Captain: code\n\nCaptain, done", "- item\n\n      Captain: code\n\ndone"},
		{"parent code after nested list", "- outer\n  - inner\n\n  parent paragraph\n\n      Captain: parent code\n\nCaptain, done", "- outer\n  - inner\n\n  parent paragraph\n\n      Captain: parent code\n\ndone"},
		{"parent code after two nested lists", "- outer\n  - inner\n    - innermost\n\n  parent paragraph\n\n      Captain: parent code\n\nCaptain, done", "- outer\n  - inner\n    - innermost\n\n  parent paragraph\n\n      Captain: parent code\n\ndone"},
		{"ordered parent code after nested list", "10. outer\n    - inner\n\n    parent paragraph\n\n        Captain: parent code\n\nCaptain, done", "10. outer\n    - inner\n\n    parent paragraph\n\n        Captain: parent code\n\ndone"},
		{"code after list closed", "- item\n\nParagraph\n\n    Captain: code\n\nCaptain, done", "- item\n\nParagraph\n\n    Captain: code\n\ndone"},
		{"code after star break", "* * *\n\n    Captain: code after star break\n\nCaptain, done", "* * *\n\n    Captain: code after star break\n\ndone"},
		{"code after dash break", "- - -\n\n    Captain: code after dash break\n\nCaptain, done", "- - -\n\n    Captain: code after dash break\n\ndone"},
		{"code after underscore break", "_ _ _\n\n    Captain: code after underscore break\n\nCaptain, done", "_ _ _\n\n    Captain: code after underscore break\n\ndone"},
		{"star break closes list", "- item\n\n* * *\n\n    Captain: code\n\nCaptain, done", "- item\n\n* * *\n\n    Captain: code\n\ndone"},
		{"dash break closes list", "- item\n\n---\n\n    Captain: code\n\nCaptain, done", "- item\n\n---\n\n    Captain: code\n\ndone"},
		{"code after heading closes list", "- item\n## Next\n    Captain: code", "- item\n## Next\n    Captain: code"},
		{"code after list item heading and dedent", "- ## Heading\nParagraph\n\n    Captain: code\n\nCaptain, done", "- ## Heading\nParagraph\n\n    Captain: code\n\ndone"},
		{"heading in list code stays literal", "- item\n\n      ## Captain: literal\n\nCaptain, done", "- item\n\n      ## Captain: literal\n\ndone"},
		{"tab code outside list", "Intro\n\n\tCaptain: code\n\nCaptain, done", "Intro\n\n\tCaptain: code\n\ndone"},
		{"thematic break", "An odd \" mark\n***\nCaptain, next \"x\"", "An odd \" mark\n***\nnext \"x\""},
		{"fence open", "An odd \" mark\n```text\nCaptain: literal\n```\nCaptain, next \"x\"", "An odd \" mark\n```text\nCaptain: literal\n```\nnext \"x\""},
		{"fence close", "```text\nAn odd \" mark\n```\nCaptain, next \"x\"", "```text\nAn odd \" mark\n```\nnext \"x\""},
		{"blockquote marker", "An odd \" mark\n> Captain, quoted\n\nCaptain, next \"x\"", "An odd \" mark\n> Captain, quoted\n\nnext \"x\""},
		{"indented code", "An odd \" mark\n    Captain: literal\nCaptain, next \"x\"", "An odd \" mark\n    Captain: literal\nnext \"x\""},
		{"tab indented code", "An odd \" mark\n\tCaptain: literal\nCaptain, next \"x\"", "An odd \" mark\n\tCaptain: literal\nnext \"x\""},
		{"html block", "An odd \" mark\n<details>\nCaptain, next \"x\"", "An odd \" mark\n<details>\nnext \"x\""},
		{"html block with content", "An odd \" mark\n<summary>Captain, next \"x\"</summary>", "An odd \" mark\n<summary>next \"x\"</summary>"},
		{"html comment block", "An odd \" mark\n<!-- note -->\nCaptain, next \"x\"", "An odd \" mark\n<!-- note -->\nnext \"x\""},
		{"lone tag line", "An odd \" mark\n<br>\nCaptain, next \"x\"", "An odd \" mark\n<br>\nnext \"x\""},
		{"paragraph continuation", "Keep \"Captain, ready\nand waiting\" dialogue.\n\nCaptain, done", "Keep \"Captain, ready\nand waiting\" dialogue.\n\ndone"},
		{"single quoted continuation", "Keep 'Captain, ready\nand waiting' dialogue.\n\nCaptain, done", "Keep 'Captain, ready\nand waiting' dialogue.\n\ndone"},
		{"inline tag continuation", "Keep \"Captain, ready\n<em>and</em> waiting\" dialogue.\n\nCaptain, done", "Keep \"Captain, ready\n<em>and</em> waiting\" dialogue.\n\ndone"},
		{"number continuation", "Keep \"Captain, ready\n2024 was fine\" dialogue.\n\nCaptain, done", "Keep \"Captain, ready\n2024 was fine\" dialogue.\n\ndone"},
		{"hash continuation", "Keep \"Captain, ready\n#1 priority\" dialogue.\n\nCaptain, done", "Keep \"Captain, ready\n#1 priority\" dialogue.\n\ndone"},
		{"emphasis continuation", "Keep \"Captain, ready\n*and* waiting\" dialogue.\n\nCaptain, done", "Keep \"Captain, ready\n*and* waiting\" dialogue.\n\ndone"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := Text(tt.input); got != tt.want {
				t.Errorf("body = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestText_ThematicBreaks(t *testing.T) {
	t.Parallel()
	for _, separator := range []string{"---", "***", "___", "  - - -  ", " *\t* *\t", "   _ _ _ _\t"} {
		t.Run(separator, func(t *testing.T) {
			input := "> Captain, quoted evidence\n" + separator + "\nCaptain, next steps"
			want := "> Captain, quoted evidence\n" + separator + "\nnext steps"
			if got := Text(input); got != want {
				t.Errorf("body = %q, want %q", got, want)
			}
		})
	}
	for _, input := range []string{
		"> note\n--\nCaptain, quoted continuation",
		"> note\n-_*\nCaptain, quoted continuation",
		"> note\n___ suffix\nCaptain, quoted continuation",
		"> note\n    ---\nCaptain, quoted continuation",
		"> note\n> ---\n> Captain, quoted evidence",
		"```text\n> note\n---\nCaptain, literal output\n```",
	} {
		if got := Text(input); got != input {
			t.Errorf("body = %q, want unchanged %q", got, input)
		}
	}
}
