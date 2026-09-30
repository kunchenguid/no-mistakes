package steps

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/config"
)

// pathInstructionBlock is one trusted rule that matched the change, together
// with the files it matched. The reviewer reads the scope and the rule as one
// unit, so no block can be mistaken for a repository-wide instruction.
type pathInstructionBlock struct {
	Path         string
	Instructions string
	Files        []string
}

// pathInstructionMatches is the outcome of selecting trusted rules for a run.
// The non-matching and dropped sets exist so the step can say which rules it
// applied and which it did not; a rule that matched nothing and a rule that was
// discarded must not look the same in the log. IgnoredIDs names the blocks
// every matched file of which sits outside the reviewable set: their glob
// fired, but only on paths the ignore lists leave skipped, so the prompt must
// not claim them.
type pathInstructionMatches struct {
	Blocks       []pathInstructionBlock
	IgnoredIDs   []string
	UnmatchedIDs []string
	DuplicateIDs []string
	UnusableIDs  []string
}

// matchPathInstructions selects the trusted rules whose glob matches at least
// one changed path, in config order.
//
// changed is the COMPLETE changed-file set, never the ignore-filtered subset:
// ignore_patterns comes from the pushed branch, so filtering here would let a
// contributor delete a maintainer's rule from the review of their own branch by
// adding that rule's glob to their ignore list. Which files the reviewer is
// asked to look at is a separate question, answered by reviewablePaths.
//
// Globs follow the same rules as ignore_patterns (matchIgnorePattern), so a
// maintainer has one path-matching model to learn. Two entries with the same
// path and the same instructions collapse to one block; the same instruction
// text under two different globs stays two blocks, because each block states
// the scope it was selected for.
func matchPathInstructions(changed []string, rules []config.PathInstruction) pathInstructionMatches {
	var out pathInstructionMatches
	if len(rules) == 0 {
		return out
	}
	seen := make(map[string]bool, len(rules))
	for _, rule := range rules {
		pattern := strings.TrimSpace(rule.Path)
		instructions := sanitizePromptMultilineText(rule.Instructions)
		if pattern == "" || instructions == "" {
			out.UnusableIDs = append(out.UnusableIDs, pathInstructionID(rule.Path))
			continue
		}
		key := pattern + "\x00" + instructions
		if seen[key] {
			out.DuplicateIDs = append(out.DuplicateIDs, pattern)
			continue
		}
		seen[key] = true
		files := matchingChangedPaths(changed, pattern)
		if len(files) == 0 {
			out.UnmatchedIDs = append(out.UnmatchedIDs, pattern)
			continue
		}
		out.Blocks = append(out.Blocks, pathInstructionBlock{
			Path:         pattern,
			Instructions: instructions,
			Files:        files,
		})
	}
	return out
}

// pathInstructionID names an entry in a log line when its path is unusable.
func pathInstructionID(path string) string {
	if trimmed := strings.TrimSpace(path); trimmed != "" {
		return trimmed
	}
	return "(no path)"
}

func matchingChangedPaths(changed []string, pattern string) []string {
	var files []string
	for _, path := range changed {
		if matchIgnorePattern(path, pattern) {
			files = append(files, path)
		}
	}
	return files
}

// scopePathInstructionMatches narrows the selected blocks to the files the
// run's coverage contract actually holds the reviewer to: a matched file that
// is not in the reviewable set - skipped by pushed and trusted ignores alike
// - must not be listed under the rule, or a reviewer that reports it in
// reviewed_paths parks the round on out-of-scope coverage. A file the trusted
// ignore list excludes but the pushed list does not stays in reviewable as a
// survivor, so the rule still claims it; only files dropped from the set
// entirely are hidden. A block left with no files is dropped from the prompt
// (its glob matched, but every file it claimed is skipped) and reported
// through IgnoredIDs so it does not read as an unmatched rule. The returned
// value shares every untouched field with the input, and it is computed only
// after the union settles the reviewable set, so the prompt and the log
// consume exactly what the coverage contract claims.
func scopePathInstructionMatches(matches pathInstructionMatches, reviewable []string) pathInstructionMatches {
	held := make(map[string]bool, len(reviewable))
	for _, path := range reviewable {
		held[path] = true
	}
	out := matches
	out.Blocks = nil
	for _, block := range matches.Blocks {
		files := make([]string, 0, len(block.Files))
		for _, file := range block.Files {
			if held[file] {
				files = append(files, file)
			}
		}
		if len(files) == 0 {
			out.IgnoredIDs = append(out.IgnoredIDs, block.Path)
			continue
		}
		out.Blocks = append(out.Blocks, pathInstructionBlock{
			Path:         block.Path,
			Instructions: block.Instructions,
			Files:        files,
		})
	}
	return out
}

// matchedFilesSummary renders one block's file list, bounded by
// config.ReviewPathInstructionsMaxFilesBytes so a broad glob cannot push the
// section past the size the config was validated against. Truncation keeps
// git's order and states how many files it left out.
func matchedFilesSummary(files []string) string {
	const sep = ", "
	var rendered []string
	shown := 0
	used := 0
	for i, file := range files {
		file = promptPath(file)
		add := len(file)
		if shown > 0 {
			add += len(sep)
		}
		// Keep room for the remaining-count suffix in case this is the last
		// file that fits.
		reserve := 0
		if remaining := len(files) - i - 1; remaining > 0 {
			reserve = len(sep) + len(fmt.Sprintf("+%d more", remaining))
		}
		if used+add+reserve > config.ReviewPathInstructionsMaxFilesBytes {
			break
		}
		shown++
		used += add
		rendered = append(rendered, file)
	}
	summary := strings.Join(rendered, sep)
	if dropped := len(files) - shown; dropped > 0 {
		if summary != "" {
			summary += sep
		}
		summary += fmt.Sprintf("+%d more", dropped)
	}
	return summary
}

func promptPath(path string) string {
	if strings.IndexFunc(path, func(r rune) bool { return !strconv.IsGraphic(r) }) >= 0 {
		return strconv.QuoteToGraphic(path)
	}
	return path
}

// reviewPathInstructionsSection renders the matched blocks for the review
// prompt. No configured rules, or none whose glob matches, returns the empty
// string, which leaves the prompt byte for byte what it is without this
// setting. The labels come from internal/config so the size the config was
// validated against measures this exact output.
func reviewPathInstructionsSection(matches pathInstructionMatches) string {
	if len(matches.Blocks) == 0 {
		return ""
	}
	rendered := make([]string, 0, len(matches.Blocks))
	for _, block := range matches.Blocks {
		rendered = append(rendered,
			config.ReviewPathInstructionsPathLabel+block.Path+"\n"+
				config.ReviewPathInstructionsFilesLabel+matchedFilesSummary(block.Files)+"\n"+
				config.ReviewPathInstructionsRulesLabel+"\n"+
				block.Instructions)
	}
	return "\n\n" + config.ReviewPathInstructionsHeading + "\n" + strings.Join(rendered, "\n\n")
}

// logPathInstructions reports which trusted rules steered this review and which
// did not, so a rule that never fires is visible in the step log instead of
// looking identical to a rule that has no effect.
func logPathInstructions(log func(string), matches pathInstructionMatches) {
	if log == nil {
		return
	}
	if len(matches.Blocks) > 0 {
		scopes := make([]string, 0, len(matches.Blocks))
		for _, block := range matches.Blocks {
			scopes = append(scopes, fmt.Sprintf("%s (%d file(s))", block.Path, len(block.Files)))
		}
		log(fmt.Sprintf("applied %d trusted review instruction block(s) for changed paths: %s", len(matches.Blocks), strings.Join(scopes, "; ")))
	}
	if len(matches.UnmatchedIDs) > 0 {
		log(fmt.Sprintf("%d trusted review instruction rule(s) matched no changed path: %s", len(matches.UnmatchedIDs), strings.Join(matches.UnmatchedIDs, ", ")))
	}
	if len(matches.IgnoredIDs) > 0 {
		log(fmt.Sprintf("%d trusted review instruction rule(s) matched only paths the ignore lists exclude: %s", len(matches.IgnoredIDs), strings.Join(matches.IgnoredIDs, ", ")))
	}
	if len(matches.DuplicateIDs) > 0 {
		log(fmt.Sprintf("skipped %d duplicate trusted review instruction rule(s): %s", len(matches.DuplicateIDs), strings.Join(matches.DuplicateIDs, ", ")))
	}
	if len(matches.UnusableIDs) > 0 {
		log(fmt.Sprintf("skipped %d trusted review instruction rule(s) with no usable path or instructions: %s", len(matches.UnusableIDs), strings.Join(matches.UnusableIDs, ", ")))
	}
}
