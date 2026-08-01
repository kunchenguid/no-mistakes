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

// reviewSkillPluginWalkDepth bounds how deep under .claude/plugins a skill
// directory is looked for. The marketplace cache nests
// cache/<marketplace>/<plugin>/<version>/skills/<name>, which is six
// components; the bound leaves headroom without walking an arbitrary tree.
const reviewSkillPluginWalkDepth = 8

// reviewSkillDirs enumerates the directories a skill named name can occupy:
// the bases skill.Install writes (.claude/skills, .agents/skills) plus the
// Claude Code plugin layouts, since a plugin-provided skill is invoked as
// `plugin:<name>` and is just as valid a resolution. Plugins are found by a
// bounded walk rather than fixed globs because the nesting between .claude/
// plugins and skills/<name> varies by install source (a linked plugin sits at
// plugins/<plugin>/skills/<name>, a marketplace one at
// plugins/cache/<marketplace>/<plugin>/<version>/skills/<name>).
func reviewSkillDirs(root, name string) []string {
	dirs := make([]string, 0, len(skill.InstallBases)+2)
	for _, base := range skill.InstallBases {
		dirs = append(dirs, filepath.Join(root, base, name))
	}
	return append(dirs, reviewSkillPluginDirs(root, name)...)
}

// reviewSkillPluginDirs walks .claude/plugins for any skills/<name> directory,
// treating an unreadable subtree as absent. Directories are resolved with Stat
// rather than the walk entry's own type, so a plugin (or a marketplace cache)
// installed as a symlink resolves the way the shipped layouts do; the depth
// bound is what keeps that from following a cycle forever, and a match is
// taken before the bound applies so a skill sitting exactly at it still counts.
func reviewSkillPluginDirs(root, name string) []string {
	return appendReviewSkillPluginDirs(nil, filepath.Join(root, ".claude", "plugins"), name, 1)
}

func appendReviewSkillPluginDirs(dirs []string, dir, name string, depth int) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return dirs
	}
	for _, entry := range entries {
		path := filepath.Join(dir, entry.Name())
		info, err := os.Stat(path)
		if err != nil || !info.IsDir() {
			continue
		}
		if entry.Name() == name && filepath.Base(dir) == "skills" {
			dirs = append(dirs, path)
			continue
		}
		if depth < reviewSkillPluginWalkDepth {
			dirs = appendReviewSkillPluginDirs(dirs, path, name, depth+1)
		}
	}
	return dirs
}

// reviewSkillJudgeable reports whether this machine has a skill library at all.
// This is not a general escape hatch: `no-mistakes init` creates the install
// bases, so on any initialized machine it is true and the mandate applies
// unconditionally. It only covers a host that has never run init, where no
// skill directory exists to read an absence from.
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
// The mandate is deliberately hard and has no config opt-out: on an initialized
// machine, a missing comprehensive-code-review fails every review step until it
// is installed.
func assertReviewSkillInstalled(workDir string, agentName string) error {
	if !reviewSkillJudgeable(workDir) || reviewSkillInstalled(workDir, requiredReviewSkill) {
		return nil
	}
	return fmt.Errorf(
		"required review skill %q is not installed for agent %s; install it under one of %v, in the repository or in your home directory",
		requiredReviewSkill, agentName, skill.InstallBases)
}
