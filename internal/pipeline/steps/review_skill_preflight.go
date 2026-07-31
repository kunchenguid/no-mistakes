package steps

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/skill"
)

// agentDisplayName names the configured agent for operator-facing errors.
func agentDisplayName(sctx *pipeline.StepContext) string {
	if sctx == nil || sctx.Agent == nil {
		return "unknown"
	}
	return sctx.Agent.Name()
}

// reviewSkillHomeDir resolves the user-level skill root. It is a variable so
// tests can pin it instead of reading the developer's real home directory.
var reviewSkillHomeDir = os.UserHomeDir

// reviewSkillSearchRoots returns the roots a skill install may live under, in
// the order a reader would look: the run's worktree (project-level skills)
// and the user's home (user-level skills).
func reviewSkillSearchRoots(workDir string) []string {
	var roots []string
	if workDir != "" {
		roots = append(roots, workDir)
	}
	if home, err := reviewSkillHomeDir(); err == nil && home != "" {
		roots = append(roots, home)
	}
	return roots
}

// reviewSkillDirs enumerates the directories a skill named name can occupy:
// the bases skill.Install writes (.claude/skills, .agents/skills) plus the
// Claude Code plugin layout, since a plugin-provided skill is invoked as
// `plugin:<name>` and is just as valid a resolution.
func reviewSkillDirs(root, name string) []string {
	dirs := make([]string, 0, len(skill.InstallBases)+2)
	for _, base := range skill.InstallBases {
		dirs = append(dirs, filepath.Join(root, base, name))
	}
	for _, pattern := range []string{
		filepath.Join(root, ".claude", "plugins", "*", "skills", name),
		filepath.Join(root, ".claude", "plugins", "*", "*", "skills", name),
	} {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			continue
		}
		dirs = append(dirs, matches...)
	}
	return dirs
}

// reviewSkillJudgeable reports whether this machine has a skill library at all.
// Where no skill base directory exists the absence of one skill proves nothing
// (an agent may resolve skills from somewhere this check cannot see), so the
// preflight stays silent rather than failing a run over a guess.
func reviewSkillJudgeable(workDir string) bool {
	for _, root := range reviewSkillSearchRoots(workDir) {
		for _, base := range skill.InstallBases {
			if info, err := os.Stat(filepath.Join(root, base)); err == nil && info.IsDir() {
				return true
			}
		}
	}
	return false
}

func reviewSkillInstalled(workDir, name string) bool {
	for _, root := range reviewSkillSearchRoots(workDir) {
		for _, dir := range reviewSkillDirs(root, name) {
			if info, err := os.Stat(filepath.Join(dir, skill.SkillFile)); err == nil && !info.IsDir() {
				return true
			}
		}
	}
	return false
}

// assertReviewSkillInstalled fails the review before its first turn when the
// mandated skill is not installed. Without it the step still fails, but only
// after two full review turns and with a message about the agent ignoring the
// prompt, which reads as an agent problem rather than the setup problem it is.
func assertReviewSkillInstalled(workDir string, agentName string) error {
	if !reviewSkillJudgeable(workDir) || reviewSkillInstalled(workDir, requiredReviewSkill) {
		return nil
	}
	return fmt.Errorf(
		"required review skill %q is not installed for agent %s; install it under one of %v, in the repository or in your home directory",
		requiredReviewSkill, agentName, skill.InstallBases)
}
