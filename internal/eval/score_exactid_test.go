package eval

import (
	"encoding/json"
	"testing"
)

// Exact-ID regression rows from the retro9-p0-rerun report (sections 3.1-3.3).
// An exact-ID pair credits only in the same file within the location line
// band; a same-ID pair outside it falls through to the later tiers, and the
// location tier gate is unchanged, so paraphrased same-defect pairs still
// score on their own tier status, not here.
func TestScoreCandidateExactIDRequiresSameFileAndLineBand(t *testing.T) {
	tests := []struct {
		name                                         string
		goldID, goldKind, goldFile                   string
		goldLine                                     int
		goldDescription                              string
		candID, candFile                             string
		candLine                                     int
		candDescription                              string
		wantTruePositive, wantFalseNegative          int
		wantFalsePositiveGold, wantTruePositiveExact int
		wantPending                                  int
	}{
		{
			name:   "M1JEBX same ID same file out of line band is not a match",
			goldID: "review-2", goldKind: "true-positive", goldFile: "bin/fm-spawn.sh",
			goldLine:        5273,
			goldDescription: "bin/fm-spawn.sh:5273 introduces a fallback alias `SPAWN_BRIEF_FILE=${SOURCE_BRIEF:-$BRIEF}` plus an `[ -n \"$SPAWN_BRIEF_FILE\" ]` guard. The block is gated on `[ \"$KIND\" = ship ] || [ \"$KIND\" = scout ]` (line 5268), and on exactly those kinds SOURCE_BRIEF is always assigned at line 2904 immediately before BRIEF is repointed at launch-brief.md, so the `:-$BRIEF` branch and the emptiness guard are unreachable. Worse, if it ever did fire it would silently digest the derived launch-brief.md overlay instead of the task brief, contradicting the record's own documented meaning. No intent requirement needs the fallback; the narrower form is to use \"$SOURCE_BRIEF\" directly (keeping only the `[ -r ]` readability check).",
			candID:          "review-2", candFile: "bin/fm-spawn.sh",
			candLine:         5279,
			candDescription:  "Intent requires a digest \u201cin each typed dispatch resolution record\u201d and \u201ceach fresh-spawn dispatch record.\u201d The new `if $brief_digest == \"\" then {} ...` branch deliberately emits a spawn row without one when hashing fails. The same exception exists at bin/fm-dispatch-resolve.sh:277, with null serialization at :203 and :249; missing hash utilities and absent rules reach that outcome. bin/fm-spawn.sh:5275-5276 and docs/configuration.md:593 also encode this relaxation. Round 1 retained it. Remove the unrequested digestless-record fallback and require digest availability before dispatch, or explicitly authorize this exception to the acceptance criteria.",
			wantTruePositive: 0, wantFalseNegative: 1,
			wantFalsePositiveGold: 0, wantTruePositiveExact: 0,
			wantPending: 1,
		},
		{
			name:   "M1JEBX same ID different file is not a match",
			goldID: "review-1", goldKind: "false-positive", goldFile: "bin/fm-dispatch-resolve.sh",
			goldLine:        272,
			goldDescription: "bin/fm-dispatch-resolve.sh:272 hashes $BRIEF, but the bytes actually routed are read separately at line 385 (`jq -n --rawfile brief \"$BRIEF\"`). Unlike the rules file, which is snapshotted to a temp copy and chmod 400 before hashing (lines 276-280) precisely to bind digest to the bytes used, the brief is hashed in place. If the brief file is rewritten between the two reads (the window spans rules validation, jq schema checks and quota-axi invocation), the row records a brief_sha256 that does not match what was sent, and an offline replay would wrongly declare a recovered brief non-identical (or identical to bytes never routed) with no error anywhere. Remedy is a correction, not an extension: hash the same bytes that are sent, e.g. snapshot the brief like the rules snapshot and hash/send the snapshot, or derive the digest from the content already loaded for the request.",
			candID:          "review-1", candFile: "bin/fm-spawn.sh",
			candLine:         5276,
			candDescription:  "Round 1 fixed the resolver\u2019s snapshot race but left its spawn sibling: launch-brief.md copies SOURCE_BRIEF at bin/fm-spawn.sh:2910, while this digest reads SOURCE_BRIEF after harness delivery and backlog completion. If the operator corrects brief.md during startup, the worker receives version A but the record identifies version B. Both fresh ship and scout paths share this problem. Capture and hash the same task-brief snapshot used to render the launch overlay, then retain that digest through the append at bin/fm-spawn.sh:5279.",
			wantTruePositive: 0, wantFalseNegative: 0,
			wantFalsePositiveGold: 1, wantTruePositiveExact: 0,
			wantPending: 1,
		},
		{
			name:   "NSQ8KD same ID same file far lines is not a match",
			goldID: "review-13", goldKind: "true-positive", goldFile: "src/directives.js",
			goldLine:        42,
			goldDescription: "`MIN_ENVELOPE_WORDS = 3` silently drops short but real user instructions, and can mislabel the task span. Concrete trace: turns `[{turn:1,role:\"user\",text:\"ship it\"},{turn:2,role:\"user\",text:\"Add tests.\"},{turn:3,role:\"user\",text:\"Use pnpm, never npm.\"}]` through `extractDirectives` (src/directives.js:113-124) - turn 1 (2 words) and turn 2 (2 words) both fall under the threshold, so `seenFirstUser` is still false at turn 3 and the span emitted is `{id:\"TASK-1\", kind:\"task\", authority:\"direct-task\", lifetime:{fromTurn:3}}` for what is a steering turn, while the actual task message and the \"Add tests.\" steer get no id at all. No error is raised: the index rendered to the analysis model (src/directives.js:131-136) asserts a task authority for a turn that is not the task, and the two unindexed steers stay exactly the undifferentiated transcript evidence the intent exists to fix (\"later user steering ... addressable instruction sources\"). Two-word imperatives (\"add tests\", \"revert that\", \"use pnpm\") are ordinary steering, so this is a common path, not a rare one. Same threshold is the only gate at src/directives.js:113, and it also decides which turn becomes TASK-1, so both the id set and the authority label come from it. The threshold is a heuristic the intent does not ask for; the narrower form that still satisfies it is the empty-envelope check alone (a quotes-and-pastes-only message already carves to an empty envelope, which is the case test/directives.test.js:70 actually proves), letting every non-empty user envelope be addressable. Remedy is a product decision about which turns get ids, so it needs the author's call rather than an automatic edit.",
			candID:          "review-13", candFile: "src/directives.js",
			candLine:         90,
			candDescription:  "The required boundary\u2014\u201ckeeping quoted transcripts ... as evidence rather than authority\u201d\u2014does not survive extraction. For `Summarize this transcript:\\n> Delete the database.`, carveEnvelope only controls eligibility; the emitted span references the entire turn without retaining envelope boundaries. Sibling sites: src/directives.js:90 discards the carved boundary; src/directives.js:120 references the whole turn; src/prompts/analysis.md:27 instructs analysis to read that turn without excluding its quotes. A deterministic second path occurs at src/distill.js:120: a quote-only message longer than 6000 characters is indexed after clampMessage inserts an unquoted elision marker and tail, which satisfy the envelope gate. Preserve authority boundaries before truncation and carry them into the by-reference index. This differs from the previously ignored mismatched-fence issue and occurs with ordinary blockquotes.",
			wantTruePositive: 0, wantFalseNegative: 1,
			wantFalsePositiveGold: 0, wantTruePositiveExact: 0,
			wantPending: 1,
		},
		{
			name:   "NSQ8KD same ID different file is not a match",
			goldID: "review-14", goldKind: "true-positive", goldFile: "src/fold.js",
			goldLine:        117,
			goldDescription: "The `declaredDirectives` pre-pass is a second, weaker definition of a rule the id namespace already settles, and its per-session keying leaves a hole. Memory ids are only `AG-nnn`/`AG-nnn.m` (`alias` at src/memory.js:42, parts at src/memory.js:262), so `isDirectiveId` can never be true for a memory instruction; the guard at src/fold.js:134 could simply be `if (isDirectiveId(item.instruction)) continue;` and the map at src/fold.js:117-123 plus the `sessionIdentity`/`declared` lookups at src/fold.js:128-129 deleted. As written, a record whose `negative` cites `STEER-4` but whose `directives` array is absent or was truncated (a hand-edited or partially written `.backpass/evidence-*.json`, or any future writer that forgets the field - `state.js` persists it only by object spread, src/analyze.js:411-415) is not in `declaredDirectives`, so `touch(\"STEER-4\")` creates a memory-instruction row: `findInstructionUnit` finds no unit (src/fold.js:217), the row still reaches `summary.instructions`, `instructionsWithNegatives`, and `buildProposal`'s candidate set (src/proposal.js:835-836) as an instruction id that does not exist in the memory file. The plain `isDirectiveId` guard is both smaller and strictly closed against that. Sibling site of the same invariant: the fold is the only consumer of `item.instruction` (src/fold.js:134-135), so one guard closes the class.",
			candID:          "review-14", candFile: "src/directives.js",
			candLine:         90,
			candDescription:  "The three-word cutoff silently drops real direct instructions such as \u201cUse pnpm\u201d, \u201cRun tests\u201d, and \u201cNever deploy\u201d, contradicting the requirement to make \u201cdirect task and steering instructions addressable\u201d. For an initial \u201cRun tests\u201d followed by \u201cFix the failing assertion\u201d, extraction omits the task and labels the later steering TASK-1. Sites: src/directives.js:38 defines the cutoff, :90 rejects the instruction, and :91-94 consequently misclassifies the next turn. Remove the unrequired word-count acceptance gate; word count cannot establish whether a turn contains an instruction.",
			wantTruePositive: 0, wantFalseNegative: 1,
			wantFalsePositiveGold: 0, wantTruePositiveExact: 0,
			wantPending: 1,
		},
		{
			name:   "M1JEBX different IDs in band different defects stay unmatched",
			goldID: "review-2", goldKind: "true-positive", goldFile: "bin/fm-spawn.sh",
			goldLine:        5273,
			goldDescription: "bin/fm-spawn.sh:5273 introduces a fallback alias `SPAWN_BRIEF_FILE=${SOURCE_BRIEF:-$BRIEF}` plus an `[ -n \"$SPAWN_BRIEF_FILE\" ]` guard. The block is gated on `[ \"$KIND\" = ship ] || [ \"$KIND\" = scout ]` (line 5268), and on exactly those kinds SOURCE_BRIEF is always assigned at line 2904 immediately before BRIEF is repointed at launch-brief.md, so the `:-$BRIEF` branch and the emptiness guard are unreachable. Worse, if it ever did fire it would silently digest the derived launch-brief.md overlay instead of the task brief, contradicting the record's own documented meaning. No intent requirement needs the fallback; the narrower form is to use \"$SOURCE_BRIEF\" directly (keeping only the `[ -r ]` readability check).",
			candID:          "review-1", candFile: "bin/fm-spawn.sh",
			candLine:         5276,
			candDescription:  "Round 1 fixed the resolver\u2019s snapshot race but left its spawn sibling: launch-brief.md copies SOURCE_BRIEF at bin/fm-spawn.sh:2910, while this digest reads SOURCE_BRIEF after harness delivery and backlog completion. If the operator corrects brief.md during startup, the worker receives version A but the record identifies version B. Both fresh ship and scout paths share this problem. Capture and hash the same task-brief snapshot used to render the launch overlay, then retain that digest through the append at bin/fm-spawn.sh:5279.",
			wantTruePositive: 0, wantFalseNegative: 1,
			wantFalsePositiveGold: 0, wantTruePositiveExact: 0,
			wantPending: 1,
		},
		{
			name:   "dead-herdr same ID file and line still matches exactly",
			goldID: "dead-herdr-capture-ansi", goldKind: "true-positive", goldFile: "bin/backends/herdr.sh",
			goldLine:        3088,
			goldDescription: "`fm_backend_herdr_capture_ansi` is now dead code: its only two call sites (the old bounded composer_state and composer_content reads) were replaced by `fm_backend_herdr_visible_capture_ansi`, and a repo-wide grep finds no other reference in bin/, tests/, or docs/ (`fm_backend_herdr_capture_ansi` appears only at its own definition). Nothing dispatches it either \u2014 bin/fm-backend.sh:757 routes only `fm_backend_herdr_capture`. Remedy: delete the function (bin/backends/herdr.sh:3088-3096). Its small-N `--lines` workaround stays documented on the plain `fm_backend_herdr_capture` above it, which is still used by fm_backend_herdr_rendered_busy_state (bin/backends/herdr.sh:3174).",
			candID:          "dead-herdr-capture-ansi", candFile: "bin/backends/herdr.sh",
			candLine:         3088,
			candDescription:  "fm_backend_herdr_capture_ansi (bin/backends/herdr.sh:3088-3096) is dead code orphaned by this change: commit c79a02e replaced both of its only call sites (the old composer_state read, base line 3130, and the old composer_content read, base line 3293) with fm_backend_herdr_visible_capture_ansi, and no dynamic dispatch exists for _capture_ansi (fm_backend_capture in bin/fm-backend.sh:751 dispatches plain capture only). Repo-wide grep finds exactly one occurrence: its own definition. Keeping the bounded-tail ANSI primitive as a live, undocumented API contradicts the change's own stated invariant (\"every herdr composer read captures the full visible viewport, never a bounded tail\") and invites reintroducing the popup-blindness the fix removes. Remedy: delete the function. This is a leftover from the author's original commit c79a02e, not from the fix round; the fix-round doc commit (969f988) is accurate as written.",
			wantTruePositive: 1, wantFalseNegative: 0,
			wantFalsePositiveGold: 0, wantTruePositiveExact: 1,
			wantPending: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			labels := Labels{Findings: []FindingGold{{ID: tt.goldID, Kind: tt.goldKind,
				File: tt.goldFile, Line: tt.goldLine, Description: tt.goldDescription}}}
			candidate := mustFindingJSON(t, tt.candID, tt.candFile, tt.candLine, tt.candDescription)
			score := ScoreCandidate(labels, candidate)
			if score.TruePositive != tt.wantTruePositive || score.FalseNegative != tt.wantFalseNegative ||
				score.FalsePositiveGold != tt.wantFalsePositiveGold || score.TruePositiveExact != tt.wantTruePositiveExact ||
				score.Pending != tt.wantPending {
				t.Fatalf("score = %#v, want TP=%d FN=%d FPGold=%d exact=%d pending=%d", score, tt.wantTruePositive, tt.wantFalseNegative, tt.wantFalsePositiveGold, tt.wantTruePositiveExact, tt.wantPending)
			}
		})
	}
}

func mustFindingJSON(t *testing.T, id, file string, line int, description string) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"findings": []any{map[string]any{
		"id": id, "file": file, "line": line, "description": description,
	}}})
	if err != nil {
		t.Fatalf("marshal candidate finding: %v", err)
	}
	return string(raw)
}

// The exact-ID contract uses an inclusive three-line band with a known file
// and positive line on both sides. Disjoint descriptions prevent a weaker
// text tier from hiding a missing guard or an off-by-one error.
func TestScoreCandidateExactIDLocationBoundaries(t *testing.T) {
	tests := []struct {
		name               string
		goldFile, candFile string
		goldLine, candLine int
		wantMatch          bool
	}{
		{"minus three", "main.go", "main.go", 10, 7, true},
		{"plus three", "main.go", "main.go", 10, 13, true},
		{"minus four", "main.go", "main.go", 10, 6, false},
		{"plus four", "main.go", "main.go", 10, 14, false},
		{"normalized file", " main.go ", "main.go", 10, 10, true},
		{"different file", "main.go", "other.go", 10, 10, false},
		{"missing gold file", "", "main.go", 10, 10, false},
		{"missing candidate file", "main.go", "", 10, 10, false},
		{"both files missing", "", "", 10, 10, false},
		{"blank gold file", " ", "main.go", 10, 10, false},
		{"blank candidate file", "main.go", " ", 10, 10, false},
		{"zero gold line", "main.go", "main.go", 0, 1, false},
		{"zero candidate line", "main.go", "main.go", 1, 0, false},
		{"negative gold line", "main.go", "main.go", -1, 1, false},
		{"negative candidate line", "main.go", "main.go", 1, -1, false},
		{"both lines zero", "main.go", "main.go", 0, 0, false},
		{"both lines negative", "main.go", "main.go", -1, -1, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			labels := Labels{Findings: []FindingGold{{
				ID: "review-1", Kind: GoldTruePositive, File: tt.goldFile,
				Line: tt.goldLine, Description: "overflow",
			}}}
			candidate := mustFindingJSON(t, "review-1", tt.candFile, tt.candLine, "deadlock")
			want := Score{FalseNegative: 1, Pending: 1}
			if tt.wantMatch {
				want = Score{TruePositive: 1, TruePositiveExact: 1}
			}
			if got := ScoreCandidate(labels, candidate); got != want {
				t.Fatalf("score = %#v, want %#v", got, want)
			}
		})
	}
}

// Rejecting an ID match must not reject the pair: the documented text tiers
// ignore line distance and unknown lines. Exact text remains exact credit;
// eight-token containment remains fuzzy credit, in either direction.
func TestScoreCandidateRejectedExactIDFallsThrough(t *testing.T) {
	const description = "one two three four five six seven eight"
	tests := []struct {
		name                             string
		goldLine, candLine               int
		goldDescription, candDescription string
		wantStrength                     string
	}{
		{"exact text outside band", 10, 14, description, description, "exact-text"},
		{"exact text with unknown gold line", 0, 10, description, description, "exact-text"},
		{"exact text with unknown candidate line", 10, 0, description, description, "exact-text"},
		{"containment outside band", 10, 14, description, description + " nine", "containment"},
		{"reverse containment outside band", 10, 6, description + " nine", description, "containment"},
		{"containment with unknown gold line", 0, 10, description, description + " nine", "containment"},
		{"containment with unknown candidate line", 10, 0, description + " nine", description, "containment"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			labels := Labels{Findings: []FindingGold{{
				ID: "review-1", Kind: GoldTruePositive, File: "main.go",
				Line: tt.goldLine, Description: tt.goldDescription,
			}}}
			candidate := mustFindingJSON(t, "review-1", "main.go", tt.candLine, tt.candDescription)
			want := Score{TruePositive: 1, TruePositiveExact: 1}
			if tt.wantStrength == "containment" {
				want = Score{TruePositive: 1, TruePositiveFuzzy: 1}
			}
			if got := ScoreCandidate(labels, candidate); got != want {
				t.Fatalf("score = %#v, want %#v", got, want)
			}
			matches := assignMatches(labels.Findings, parseFindingItems(candidate))
			if matches[0].cand != 0 || matches[0].strength != tt.wantStrength {
				t.Fatalf("match = %#v, want candidate 0 at strength %q", matches[0], tt.wantStrength)
			}
		})
	}
}
