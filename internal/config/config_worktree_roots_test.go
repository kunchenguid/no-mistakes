package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeWorktreeRootsConfig writes a global config whose worktree_roots entries
// are absolute paths on this platform, and quotes them so a Windows drive
// letter is not read as a YAML mapping.
func writeWorktreeRootsConfig(t *testing.T, entries map[string]string) string {
	t.Helper()
	var b strings.Builder
	b.WriteString("worktree_roots:\n")
	for checkout, root := range entries {
		b.WriteString("  " + yamlPath(checkout) + ": " + yamlPath(root) + "\n")
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// yamlPath quotes a path for YAML: a Windows path must not be read as a
// mapping on its drive-letter colon, and its separators must survive as
// literal backslashes.
func yamlPath(path string) string {
	return `"` + strings.ReplaceAll(path, `\`, `\\`) + `"`
}

func TestLoadGlobal_WorktreeRoots(t *testing.T) {
	dir := t.TempDir()
	entries := map[string]string{
		filepath.Join(dir, "src", "repo-a"): filepath.Join(dir, "runs", "repo-a"),
		filepath.Join(dir, "src", "repo-b"): filepath.Join(dir, "runs", "repo-b"),
	}
	path := writeWorktreeRootsConfig(t, entries)

	cfg, err := LoadGlobal(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for checkout, root := range entries {
		if got := cfg.WorktreeRoots[checkout]; got != root {
			t.Errorf("worktree_roots[%q] = %q, want %q", checkout, got, root)
		}
	}
}

func TestLoadGlobal_WorktreeRootsDefaultEmpty(t *testing.T) {
	cfg, err := LoadGlobal(filepath.Join(t.TempDir(), "missing.yaml"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.WorktreeRoots) != 0 {
		t.Errorf("worktree_roots = %v, want empty", cfg.WorktreeRoots)
	}
}

// A relative path would resolve against whatever working directory the daemon
// happens to have, so neither side of an entry may be one.
func TestLoadGlobal_WorktreeRootsRejectsRelativePaths(t *testing.T) {
	dir := t.TempDir()
	cases := map[string]map[string]string{
		"relative root":     {filepath.Join(dir, "src", "repo-a"): filepath.Join("..", "runs")},
		"relative checkout": {filepath.Join("src", "repo-a"): filepath.Join(dir, "runs")},
	}
	for name, entries := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := LoadGlobal(writeWorktreeRootsConfig(t, entries))
			if err == nil {
				t.Fatal("expected error for a relative path")
			}
			if !strings.Contains(err.Error(), "absolute") {
				t.Errorf("error = %v, want it to explain the absolute-path requirement", err)
			}
		})
	}
}

func TestLoadGlobal_WorktreeRootsRejectsEmptyValues(t *testing.T) {
	dir := t.TempDir()
	cases := map[string]string{
		"empty root":     "worktree_roots:\n  " + yamlPath(filepath.Join(dir, "src")) + ": \"\"\n",
		"empty checkout": "worktree_roots:\n  \"\": " + yamlPath(filepath.Join(dir, "runs")) + "\n",
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadGlobal(path); err == nil {
				t.Fatal("expected error for empty worktree_roots entry")
			}
		})
	}
}

// Two checkouts sharing one root is the destructive case: cleanup and eject
// identify a run worktree by its position in a root, so each repository would
// treat the other's runs as its own.
func TestLoadGlobal_WorktreeRootsRejectsSharedRoot(t *testing.T) {
	dir := t.TempDir()
	shared := filepath.Join(dir, "runs")
	_, err := LoadGlobal(writeWorktreeRootsConfig(t, map[string]string{
		filepath.Join(dir, "src", "repo-a"): shared,
		filepath.Join(dir, "src", "repo-b"): shared,
	}))
	if err == nil {
		t.Fatal("expected error for two checkouts sharing a worktree root")
	}
	if !strings.Contains(err.Error(), "already used by") {
		t.Errorf("error = %v, want it to report the shared root", err)
	}
}

// Two spellings of one checkout would pick an arbitrary winner, so they are
// rejected instead of silently resolved.
func TestLoadGlobal_WorktreeRootsRejectsDuplicateCheckout(t *testing.T) {
	dir := t.TempDir()
	checkout := filepath.Join(dir, "src", "repo-a")
	if err := os.MkdirAll(checkout, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := LoadGlobal(writeWorktreeRootsConfig(t, map[string]string{
		checkout: filepath.Join(dir, "runs-a"),
		checkout + string(filepath.Separator) + ".": filepath.Join(dir, "runs-b"),
	}))
	if err == nil {
		t.Fatal("expected error for two spellings of one checkout")
	}
	if !strings.Contains(err.Error(), "same checkout") {
		t.Errorf("error = %v, want it to report the duplicate checkout", err)
	}
}

func TestLoadGlobal_WorktreeRootsRejectsRootInsideItsCheckout(t *testing.T) {
	dir := t.TempDir()
	checkout := filepath.Join(dir, "src", "repo-a")
	_, err := LoadGlobal(writeWorktreeRootsConfig(t, map[string]string{checkout: checkout}))
	if err == nil {
		t.Fatal("expected error for a root equal to its checkout")
	}
	if !strings.Contains(err.Error(), "checkout itself") {
		t.Errorf("error = %v, want it to report the self-placement", err)
	}
}

// The global config stays strict about unknown keys, so a misspelled setting
// is reported instead of silently ignored.
func TestLoadGlobal_RejectsMisspelledWorktreeRootsKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := "worktree_root:\n  " + yamlPath(filepath.Join(dir, "src")) + ": " + yamlPath(filepath.Join(dir, "runs")) + "\n"
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := LoadGlobal(path)
	if err == nil {
		t.Fatal("expected error for unknown key")
	}
	if !strings.Contains(err.Error(), "worktree_root") {
		t.Errorf("error = %v, want it to name the unknown key", err)
	}
}

// InspectGlobalConfigMapping answers questions the parsed configuration cannot:
// whether the document already carries a top-level key, and whether an entry can
// be added under it. Both empty forms of a key parse to nothing, so a caller that
// must not write a second one - YAML rejects a duplicate top-level key, and the
// daemon then refuses to start - has to ask the document.
func TestInspectGlobalConfigMappingPresence(t *testing.T) {
	present := map[string]string{
		"an entry":                    "worktree_roots:\n  " + yamlPath(filepath.Join(string(filepath.Separator), "src", "repo")) + ": " + yamlPath(filepath.Join(string(filepath.Separator), "work", "runs")) + "\n",
		"a key with no value":         "worktree_roots:\n",
		"a key set to an empty map":   "worktree_roots: {}\n",
		"a key among other settings":  "log_level: info\nworktree_roots: {}\nsession_reuse: true\n",
		"a document YAML cannot read": "worktree_roots:\n  /src/repo: /work/runs\n\t bad indentation\n",
	}
	for name, contents := range present {
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
		if !InspectGlobalConfigMapping(path, "worktree_roots").Present {
			t.Errorf("%s: reported absent, want present", name)
		}
	}

	absent := map[string]string{
		"an empty document":        "",
		"other settings only":      "log_level: info\n",
		"a commented-out key":      "# worktree_roots:\n#   /src/repo: /work/runs\n",
		"an indented mention":      "some_block:\n  worktree_roots: {}\n",
		"a value that mentions it": "log_level: worktree_roots:\n",
	}
	for name, contents := range absent {
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
		if InspectGlobalConfigMapping(path, "worktree_roots").Present {
			t.Errorf("%s: reported present, want absent", name)
		}
	}

	if InspectGlobalConfigMapping(filepath.Join(t.TempDir(), "missing.yaml"), "worktree_roots").Present {
		t.Error("a config that does not exist reported the key as present")
	}
}

// The empty forms are exactly the ones the parsed value cannot distinguish from
// an absent key, which is why InspectGlobalConfigMapping reads the document.
func TestEmptyWorktreeRootsFormsParseToNoEntries(t *testing.T) {
	for name, contents := range map[string]string{
		"a key with no value":       "worktree_roots:\n",
		"a key set to an empty map": "worktree_roots: {}\n",
	} {
		cfg, err := LoadGlobalFromBytes([]byte(contents))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(cfg.WorktreeRoots) != 0 {
			t.Errorf("%s: parsed %d entries, want none", name, len(cfg.WorktreeRoots))
		}
	}
}

// TestInspectGlobalConfigMappingSeparatesUnreadEntriesFromNone pins the one
// distinction a caller that rewrites the key depends on: a document nobody could
// parse yields no entries because none were READ, which is not the same answer as
// a key that has none. Reported as the same shape, a rewrite built from the
// inspection silently drops every entry the file holds.
func TestInspectGlobalConfigMappingSeparatesUnreadEntriesFromNone(t *testing.T) {
	entries := "worktree_roots:\n" +
		"  " + yamlPath(filepath.Join(string(filepath.Separator), "src", "a")) + ": " + yamlPath(filepath.Join(string(filepath.Separator), "work", "a-runs")) + "\n" +
		"  " + yamlPath(filepath.Join(string(filepath.Separator), "src", "b")) + ": " + yamlPath(filepath.Join(string(filepath.Separator), "work", "b-runs")) + "\n"

	unparseable := entries + "log_level: \"info\n"
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(unparseable), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadGlobalFromBytes([]byte(unparseable)); err == nil {
		t.Fatal("fixture parses; it must be the document YAML cannot read")
	}
	got := InspectGlobalConfigMapping(path, "worktree_roots")
	if !got.Present {
		t.Fatal("the key is written at the start of a line, but was reported absent")
	}
	if !got.Unparsed {
		t.Error("a document that could not be parsed reported its entries as read")
	}
	if len(got.Entries) != 0 {
		t.Errorf("Entries = %+v, want none read from a document that does not parse", got.Entries)
	}

	// The same key in a document that parses, with the entries actually read.
	parsed := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(parsed, []byte(entries), 0o644); err != nil {
		t.Fatal(err)
	}
	if shape := InspectGlobalConfigMapping(parsed, "worktree_roots"); shape.Unparsed || len(shape.Entries) != 2 {
		t.Errorf("a parseable document reported Unparsed=%v with %d entries, want false and 2", shape.Unparsed, len(shape.Entries))
	}

	// The column its entries sit at is read from the lines themselves, because a
	// caller offering a line at another one leaves a document YAML rejects even
	// after the operator repairs the fault this one has.
	if got.EntryIndent != 2 {
		t.Errorf("EntryIndent = %d, want the 2 its unread entries are written at", got.EntryIndent)
	}
	indented := strings.Replace(unparseable, "\n  ", "\n      ", -1)
	deep := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(deep, []byte(indented), 0o644); err != nil {
		t.Fatal(err)
	}
	if shape := InspectGlobalConfigMapping(deep, "worktree_roots"); shape.EntryIndent != 6 {
		t.Errorf("EntryIndent = %d, want the 6 the document uses", shape.EntryIndent)
	}

	// A key whose lines show nothing to sit beside reports no column at all,
	// rather than one a caller would then guess with.
	bare := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(bare, []byte("worktree_roots:\nlog_level: \"info\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if shape := InspectGlobalConfigMapping(bare, "worktree_roots"); shape.EntryIndent != 0 {
		t.Errorf("EntryIndent = %d for a key with no entry lines, want 0", shape.EntryIndent)
	}

	// An absent key in an unreadable document is still unread, not a key known
	// to be missing.
	broken := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(broken, []byte("log_level: \"info\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if shape := InspectGlobalConfigMapping(broken, "worktree_roots"); !shape.Unparsed {
		t.Error("a document that could not be parsed reported itself as read")
	}
}

// TestInspectGlobalConfigMappingReportsWhatCanBeAppended pins the shape half of
// the inspection against the YAML a caller has to produce. AppendableBlock says
// the value is a block mapping one more entry line can go into; after a flow
// mapping it cannot, and the document then stops loading at all - a daemon that
// refuses to start. Every appendable claim is checked with the loader, because a
// wrong one is exactly that failure.
func TestInspectGlobalConfigMappingReportsWhatCanBeAppended(t *testing.T) {
	checkout := yamlPath(filepath.Join(string(filepath.Separator), "src", "repo"))
	root := yamlPath(filepath.Join(string(filepath.Separator), "work", "runs"))
	entry := "  " + checkout + ": " + root + "\n"

	for name, tc := range map[string]struct {
		contents        string
		appendableBlock bool
		appendLoads     bool
		line            string
		entries         []GlobalConfigMappingEntry
	}{
		"a block mapping": {
			contents:        "worktree_roots:\n  /src/other: /work/other-runs\n",
			appendableBlock: true,
			appendLoads:     true,
			entries:         []GlobalConfigMappingEntry{{Key: "/src/other", Value: "/work/other-runs"}},
		},
		"an empty flow mapping": {
			contents: "worktree_roots: {}\n",
			line:     "worktree_roots: {}",
		},
		"a flow mapping with an entry": {
			contents: "worktree_roots: {/src/other: /work/other-runs}\n",
			line:     "worktree_roots: {/src/other: /work/other-runs}",
			entries:  []GlobalConfigMappingEntry{{Key: "/src/other", Value: "/work/other-runs"}},
		},
		// A valueless key has no block to append into either, so it is reported
		// with the flow shapes and takes the same replacement; appending happens
		// to parse, which is why the field is about the document rather than
		// about what YAML tolerates.
		"a key with no value": {
			contents:    "worktree_roots:\n",
			appendLoads: true,
			line:        "worktree_roots:",
		},
	} {
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte(tc.contents), 0o644); err != nil {
			t.Fatal(err)
		}
		got := InspectGlobalConfigMapping(path, "worktree_roots")
		if !got.Present {
			t.Fatalf("%s: reported absent", name)
		}
		if got.AppendableBlock != tc.appendableBlock {
			t.Errorf("%s: AppendableBlock = %v, want %v", name, got.AppendableBlock, tc.appendableBlock)
		}
		if got.Line != tc.line {
			t.Errorf("%s: Line = %q, want %q", name, got.Line, tc.line)
		}
		if len(got.Entries) != len(tc.entries) {
			t.Fatalf("%s: Entries = %+v, want %+v", name, got.Entries, tc.entries)
		}
		for i, want := range tc.entries {
			if got.Entries[i] != want {
				t.Errorf("%s: Entries[%d] = %+v, want %+v", name, i, got.Entries[i], want)
			}
		}

		// An appendable claim has to hold against the loader, and a flow mapping
		// has to be the failure it is reported as.
		_, err := LoadGlobalFromBytes([]byte(tc.contents + entry))
		if tc.appendableBlock && err != nil {
			t.Errorf("%s: reported appendable, but appending an entry does not load: %v", name, err)
		}
		if !tc.appendLoads && err == nil {
			t.Errorf("%s: appending an entry loads, so this document needs no replacement", name)
		}
	}
}
