package steps

import (
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

// testRunbookSections renders the Test step's repository guidance for both the
// repair and the evidence prompt: the operator's machine-local runbook, then
// the trusted runbook (inline test.instructions or test.instructions_file),
// then the trusted test.path_instructions blocks that match the change. With
// none of the trusted file-backed or path-scoped settings configured it makes
// no Git call and returns exactly the runbook text it returned before they
// existed. A trusted file that cannot be read, or guidance over the prompt
// budget, fails the step closed.
func testRunbookSections(sctx *pipeline.StepContext, baseSHA string) (string, error) {
	trusted, err := trustedTestInstructionsSection(sctx)
	if err != nil {
		return "", err
	}
	scoped, err := testPathInstructionsSection(sctx, baseSHA)
	if err != nil {
		return "", err
	}
	return repositoryTestInstructionsSection(sctx) + trusted + scoped, nil
}

func trustedTestInstructionsSection(sctx *pipeline.StepContext) (string, error) {
	if sctx.Config == nil {
		return "", nil
	}
	if instructions := strings.TrimSpace(sctx.Config.Test.Instructions); instructions != "" {
		return "\nRepository live-validation runbook (trusted, from the default branch):\n" +
			sanitizePromptMultilineText(instructions) + "\n", nil
	}
	name := sctx.Config.Test.InstructionsFile
	if name == "" {
		return "", nil
	}
	text, err := readTrustedInstructionsFile(sctx, "test.instructions_file", name)
	if err != nil {
		return "", err
	}
	return "\nRepository live-validation runbook (trusted, from " + promptPath(name) + " on the default branch):\n" + text + "\n", nil
}

// readTrustedInstructionsFile reads one instructions_file from the pinned
// trusted commit and returns it as the prompt will carry it. A file the prompt
// renderer would reduce to nothing fails like a missing one, so configured
// guidance never disappears without a word.
func readTrustedInstructionsFile(sctx *pipeline.StepContext, setting, name string) (string, error) {
	raw, err := readTrustedTreeFile(sctx.Ctx, sctx.WorkDir, sctx.Config.TrustedConfigSHA, setting, name, config.MaxTrustedInstructionsFileBytes)
	if err != nil {
		return "", err
	}
	text := sanitizePromptMultilineText(raw)
	if text == "" || config.RenderedInstructions(raw) == "" {
		return "", fmt.Errorf("%s %q is left empty once merge-conflict markers and whitespace are removed", setting, name)
	}
	return text, nil
}

// testPathInstructionsSection renders the trusted test.path_instructions
// blocks whose glob matches the change. Matching uses the COMPLETE changed-file
// list between the base and the current head, never the ignore_patterns
// subset, for the reason matchPathInstructions documents: ignore_patterns comes
// from the pushed branch and must not be able to drop a maintainer's rule.
// Every instructions_file is read, matched or not, so a broken entry fails the
// first run rather than the first run that happens to touch its path.
func testPathInstructionsSection(sctx *pipeline.StepContext, baseSHA string) (string, error) {
	if sctx.Config == nil || len(sctx.Config.Test.PathInstructions) == 0 {
		return "", nil
	}
	rules := make([]config.PathInstruction, 0, len(sctx.Config.Test.PathInstructions))
	for i, entry := range sctx.Config.Test.PathInstructions {
		text := entry.Instructions
		if entry.InstructionsFile != "" {
			read, err := readTrustedInstructionsFile(sctx, fmt.Sprintf("test.path_instructions[%d].instructions_file", i), entry.InstructionsFile)
			if err != nil {
				return "", err
			}
			text = read
		}
		rules = append(rules, config.PathInstruction{Path: entry.Path, Instructions: text})
	}
	if err := config.ValidateTestPathInstructionsBudget(rules); err != nil {
		return "", err
	}
	changedFiles, err := git.Run(sctx.Ctx, sctx.WorkDir, "diff", "--name-only", "-z", "--no-renames", baseSHA+".."+sctx.Run.HeadSHA)
	if err != nil {
		return "", fmt.Errorf("get changed files for test.path_instructions: %w", err)
	}
	matches := matchPathInstructions(changedPathList(changedFiles), rules)
	logStepPathInstructions(sctx.Log, "test", "trusted", matches)
	if len(matches.Blocks) == 0 {
		return "", nil
	}
	return "\n" + config.TestPathInstructionsHeading + "\n" + pathInstructionBlocksText(matches) + "\n", nil
}

// logUnknownTestKeys reports keys under test: that this version ignores. Only
// the step log carries it; the prompt is unchanged.
func logUnknownTestKeys(sctx *pipeline.StepContext) {
	if sctx.Config == nil || len(sctx.Config.Test.UnknownKeys) == 0 {
		return
	}
	sctx.Log(fmt.Sprintf("warning: ignoring unrecognized test setting(s), which have no effect: %s; check the spelling against the repository config reference", strings.Join(sctx.Config.Test.UnknownKeys, ", ")))
}
