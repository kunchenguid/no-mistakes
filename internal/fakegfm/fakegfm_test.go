package fakegfm

import "testing"

func TestRender(t *testing.T) {
	for name, tc := range map[string]struct{ in, want string }{
		"multi-line comment block is removed": {
			in:   "a\n<!--\nCloses #5\n-->\nb",
			want: "a\nb",
		},
		"unterminated comment block runs to the end": {
			in:   "a\n<!-- Closes #5\nCloses #6",
			want: "a",
		},
		"inline comment closed on its line is removed": {
			in:   "a <!-- Closes #5 --> b",
			want: "a  b",
		},
		"stray inline comment marker is literal": {
			in:   "Drops stray <!-- markers\n\nCloses #4",
			want: "Drops stray &lt;!-- markers\n\nCloses #4",
		},
		"inline code span": {
			in:   "Fixes `#12` and `<!--`",
			want: "Fixes <code>#12</code> and <code>&lt;!--</code>",
		},
		"raw pre element stays": {
			in:   "<pre>\nFixes #9\n</pre>",
			want: "<pre>\nFixes #9\n</pre>",
		},
		"table row cells render as separate blocks": {
			in:   "| kind | ref |\n| --- | --- |\n| fix | #5 |",
			want: "<tr>\n<td>kind</td>\n<td>ref</td>\n</tr>\n<tr>\n<td>fix</td>\n<td>#5</td>\n</tr>",
		},
		"list item renders as its own block": {
			in:   "Fixes:\n- #5",
			want: "Fixes:\n<ul>\n<li>#5</li>\n</ul>",
		},
		// Verified live against GitHub's renderer.
		"issue URL in the context repository": {
			in:   "Closes https://github.com/test/repo/issues/12",
			want: `Closes <a href="https://github.com/test/repo/issues/12">#12</a>`,
		},
		"issue URL in another repository": {
			in:   "Fixes https://github.com/cli/cli/issues/5",
			want: `Fixes <a href="https://github.com/cli/cli/issues/5">cli/cli#5</a>`,
		},
		"issue URL in a code span": {
			in:   "Fixes `https://github.com/cli/cli/pull/5`",
			want: "Fixes <code>https://github.com/cli/cli/pull/5</code>",
		},
		"fenced code": {
			in:   "```\nFixes #9 <b>\n```",
			want: "<pre><code>\nFixes #9 &lt;b&gt;\n</code></pre>",
		},
	} {
		if got := Render(tc.in, "test/repo"); got != tc.want {
			t.Errorf("%s: Render(%q) = %q, want %q", name, tc.in, got, tc.want)
		}
	}
}
