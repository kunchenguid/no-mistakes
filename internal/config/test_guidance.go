package config

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// TestPathInstruction is one glob-scoped block of live-validation guidance for
// the Test step. Path follows the same match rules as review.path_instructions
// (and ignore_patterns). Exactly one of Instructions and InstructionsFile is
// set: InstructionsFile is a literal repository-relative path the Test step
// reads from the pinned trusted default-branch commit, never from the pushed
// worktree.
type TestPathInstruction struct {
	Path             string `yaml:"path"`
	Instructions     string `yaml:"instructions"`
	InstructionsFile string `yaml:"instructions_file"`
}

// TestPathInstructionsHeading introduces the matched test.path_instructions
// blocks in the Test evidence and repair prompts. The blocks use the same
// labels as review.path_instructions (ReviewPathInstructions*Label), so one
// byte accounting (pathInstructionsSectionBytes) measures both sections.
const TestPathInstructionsHeading = "Repository live-validation instructions for the changed paths (trusted, from the default branch). Each block below applies only to the files listed under its path, and adds to the runbook above:"

// MaxTrustedInstructionsFileBytes caps one instructions_file read from the
// pinned trusted commit, the same 16 KiB cap pr.template applies. A larger
// file fails the Test step rather than being truncated silently.
const MaxTrustedInstructionsFileBytes = 16 * 1024

// TestPathInstructionsBytes returns the largest Test-prompt section these
// resolved entries can produce, measured exactly like
// ReviewPathInstructionsBytes. The entries carry the instruction text the
// prompt will render, with any instructions_file already read.
func TestPathInstructionsBytes(entries []PathInstruction) int {
	return pathInstructionsSectionBytes(TestPathInstructionsHeading, entries)
}

// ValidateTestPathInstructionsBudget applies the review.path_instructions
// entry and byte caps to test.path_instructions once every instructions_file
// has been read. Config parsing can only check inline entries, so the Test step
// calls this after its trusted reads and fails closed when it returns an error.
func ValidateTestPathInstructionsBudget(entries []PathInstruction) error {
	if len(entries) > MaxReviewPathInstructions {
		return fmt.Errorf("test.path_instructions has %d entries, at most %d are allowed", len(entries), MaxReviewPathInstructions)
	}
	if bytes := TestPathInstructionsBytes(entries); bytes > MaxReviewPathInstructionsBytes {
		return fmt.Errorf("test.path_instructions would add up to %d bytes to the test prompt, at most %d are allowed so the prompt stays within budget", bytes, MaxReviewPathInstructionsBytes)
	}
	return nil
}

// validateTestGuidance fails the repository config closed on test guidance the
// Test step could not honor deterministically. Like validateReviewRaw it also
// runs on the PUSHED copy: the trusted-copy read aborts every run whose
// default-branch config fails these checks, so a branch carrying an invalid
// block has to fail before it merges.
func validateTestGuidance(test TestRaw) error {
	if err := ValidateTrustedFilePath("test.instructions_file", test.InstructionsFile); err != nil {
		return err
	}
	if test.InstructionsFile != "" && strings.TrimSpace(test.Instructions) != "" {
		return fmt.Errorf("test.instructions and test.instructions_file are mutually exclusive; set one of them")
	}
	entries := test.PathInstructions
	if len(entries) > MaxReviewPathInstructions {
		return fmt.Errorf("test.path_instructions has %d entries, at most %d are allowed", len(entries), MaxReviewPathInstructions)
	}
	inline := make([]PathInstruction, 0, len(entries))
	for i, entry := range entries {
		path := strings.TrimSpace(entry.Path)
		if path == "" {
			return fmt.Errorf("test.path_instructions[%d].path must not be empty", i)
		}
		if err := validatePathInstructionGlob(path); err != nil {
			return fmt.Errorf("test.path_instructions[%d].path %q is not a valid glob: %w", i, path, err)
		}
		hasText := strings.TrimSpace(entry.Instructions) != ""
		hasFile := entry.InstructionsFile != ""
		switch {
		case hasText && hasFile:
			return fmt.Errorf("test.path_instructions[%d] sets both instructions and instructions_file (path %q); set one of them", i, path)
		case !hasText && !hasFile:
			return fmt.Errorf("test.path_instructions[%d] needs instructions or instructions_file (path %q)", i, path)
		case hasFile:
			if err := ValidateTrustedFilePath(fmt.Sprintf("test.path_instructions[%d].instructions_file", i), entry.InstructionsFile); err != nil {
				return err
			}
		default:
			if RenderedInstructions(entry.Instructions) == "" {
				return fmt.Errorf("test.path_instructions[%d].instructions for path %q is left empty once merge-conflict markers are removed; write the rule without <<<<<<<, =======, or >>>>>>>", i, path)
			}
			inline = append(inline, PathInstruction{Path: path, Instructions: entry.Instructions})
		}
	}
	// File-backed entries are measured after their trusted read, together with
	// these inline ones (ValidateTestPathInstructionsBudget).
	return ValidateTestPathInstructionsBudget(inline)
}

// resolveTestPathInstructions trims every entry and drops the ones left with
// no usable path or content, like resolvePathInstructions.
func resolveTestPathInstructions(entries []TestPathInstruction) []TestPathInstruction {
	out := make([]TestPathInstruction, 0, len(entries))
	for _, entry := range entries {
		trimmed := TestPathInstruction{
			Path:             strings.TrimSpace(entry.Path),
			Instructions:     strings.TrimSpace(entry.Instructions),
			InstructionsFile: strings.TrimSpace(entry.InstructionsFile),
		}
		if trimmed.Path == "" || (trimmed.InstructionsFile == "" && RenderedInstructions(trimmed.Instructions) == "") {
			continue
		}
		out = append(out, trimmed)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func copyTestPathInstructions(entries []TestPathInstruction) []TestPathInstruction {
	if len(entries) == 0 {
		return nil
	}
	return append([]TestPathInstruction(nil), entries...)
}

// maxUnknownTestKeys bounds how many unknown keys one config reports, so a
// pushed config cannot flood the step log.
const maxUnknownTestKeys = 16

// unknownTestKeys returns the keys under test: that TestRaw does not declare,
// as dotted paths. Repository parsing stays lenient about unknown keys, because
// rejecting one would fail every run of a repository whose trusted
// default-branch config already carries it; the Test step reports them instead.
func unknownTestKeys(data []byte) ([]string, error) {
	var probe struct {
		Test yaml.Node `yaml:"test"`
	}
	if err := yaml.Unmarshal(data, &probe); err != nil {
		return nil, err
	}
	if probe.Test.IsZero() {
		return nil, nil
	}
	keys := unknownYAMLKeys(&probe.Test, reflect.TypeOf(TestRaw{}), "test", 0)
	if len(keys) > maxUnknownTestKeys {
		keys = append(keys[:maxUnknownTestKeys], fmt.Sprintf("+%d more", len(keys)-maxUnknownTestKeys))
	}
	return keys, nil
}

// unknownYAMLKeys walks node against the yaml tags of t and returns the mapping
// keys no field declares. It follows nested structs, pointers and slices, which
// covers every type TestRaw holds.
func unknownYAMLKeys(node *yaml.Node, t reflect.Type, prefix string, depth int) []string {
	if depth > 8 {
		return nil
	}
	for node.Kind == yaml.AliasNode && node.Alias != nil {
		node = node.Alias
	}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	var out []string
	switch t.Kind() {
	case reflect.Struct:
		if node.Kind != yaml.MappingNode {
			return nil
		}
		fields := make(map[string]reflect.Type, t.NumField())
		for i := 0; i < t.NumField(); i++ {
			field := t.Field(i)
			if !field.IsExported() {
				continue
			}
			name, _, _ := strings.Cut(field.Tag.Get("yaml"), ",")
			if name == "-" {
				continue
			}
			if name == "" {
				name = strings.ToLower(field.Name)
			}
			fields[name] = field.Type
		}
		for _, entry := range effectiveMappingEntries(node, 0) {
			key := entry.key
			fieldType, ok := fields[key]
			if !ok {
				out = append(out, prefix+"."+displayYAMLKey(key))
				continue
			}
			out = append(out, unknownYAMLKeys(entry.value, fieldType, prefix+"."+key, depth+1)...)
		}
	case reflect.Slice:
		if node.Kind != yaml.SequenceNode {
			return nil
		}
		for i, item := range node.Content {
			out = append(out, unknownYAMLKeys(item, t.Elem(), fmt.Sprintf("%s[%d]", prefix, i), depth+1)...)
		}
	}
	return out
}

// mappingEntry is one key of a mapping as the decoder sees it once merge keys
// are applied.
type mappingEntry struct {
	key   string
	value *yaml.Node
}

// effectiveMappingEntries returns the keys a mapping decodes with, applying
// YAML merge keys (<<) the way gopkg.in/yaml.v3 does: the mapping's own keys
// win over merged ones, and among several merge sources the first one that
// sets a key wins. A merged mapping's own merge keys are applied recursively.
// Each key appears once, with the value node the decoder actually uses, so an
// unknown key that arrives only through an anchor is still reported while an
// explicitly overridden merged value is not inspected.
func effectiveMappingEntries(node *yaml.Node, depth int) []mappingEntry {
	for node.Kind == yaml.AliasNode && node.Alias != nil {
		node = node.Alias
	}
	if node.Kind != yaml.MappingNode || depth > 8 {
		return nil
	}
	var entries []mappingEntry
	seen := map[string]bool{}
	var merges []*yaml.Node
	for i := 0; i+1 < len(node.Content); i += 2 {
		key, value := node.Content[i], node.Content[i+1]
		if isYAMLMergeKey(key) {
			merges = append(merges, value)
			continue
		}
		if seen[key.Value] {
			continue
		}
		seen[key.Value] = true
		entries = append(entries, mappingEntry{key: key.Value, value: value})
	}
	for _, merge := range merges {
		for merge.Kind == yaml.AliasNode && merge.Alias != nil {
			merge = merge.Alias
		}
		sources := []*yaml.Node{merge}
		if merge.Kind == yaml.SequenceNode {
			sources = merge.Content
		}
		for _, source := range sources {
			for _, entry := range effectiveMappingEntries(source, depth+1) {
				if seen[entry.key] {
					continue
				}
				seen[entry.key] = true
				entries = append(entries, entry)
			}
		}
	}
	return entries
}

// isYAMLMergeKey matches the merge key the way gopkg.in/yaml.v3 does: a plain
// << scalar, not a quoted "<<" string key.
func isYAMLMergeKey(node *yaml.Node) bool {
	if node.Kind != yaml.ScalarNode || node.Value != "<<" {
		return false
	}
	switch node.Tag {
	case "", "!", "!!merge", "tag:yaml.org,2002:merge":
		return true
	}
	return false
}

// displayYAMLKey keeps an unknown key printable and short in a log line.
func displayYAMLKey(key string) string {
	const maxKeyBytes = 64
	if len(key) > maxKeyBytes {
		key = key[:maxKeyBytes] + "..."
	}
	if strings.IndexFunc(key, func(r rune) bool { return !strconv.IsGraphic(r) || r == ' ' }) >= 0 {
		return strconv.QuoteToGraphic(key)
	}
	return key
}

// labeledUnknownTestKeys combines both copies' unknown test keys for the run,
// naming which copy carries each one.
func labeledUnknownTestKeys(pushed, trusted *RepoConfig) []string {
	inTrusted := map[string]bool{}
	var out []string
	if trusted != nil {
		for _, key := range trusted.UnknownTestKeys {
			inTrusted[key] = true
		}
	}
	inPushed := map[string]bool{}
	if pushed != nil {
		for _, key := range pushed.UnknownTestKeys {
			inPushed[key] = true
		}
	}
	if trusted != nil {
		for _, key := range trusted.UnknownTestKeys {
			if inPushed[key] {
				out = append(out, key+" (default-branch and pushed .no-mistakes.yaml)")
			} else {
				out = append(out, key+" (default-branch .no-mistakes.yaml)")
			}
		}
	}
	if pushed != nil {
		for _, key := range pushed.UnknownTestKeys {
			if !inTrusted[key] {
				out = append(out, key+" (pushed .no-mistakes.yaml)")
			}
		}
	}
	return out
}
