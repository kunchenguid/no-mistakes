package steps

import (
	"fmt"
	"slices"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/runenv"
)

func runRepositoryCommand(sctx *pipeline.StepContext, name, command string) (string, int, error) {
	override := sctx.Config.CommandOverrides[name]
	env := stepEnvironment(sctx)
	if len(override.Env) != 0 {
		env = (runenv.Overlay{Set: override.Env}).Apply(env)
		// A command's toolchain settings must not undo a pinned forge identity.
		if sctx.ForgeContext != nil {
			env = sctx.ForgeContext.Environment.Apply(env)
		}
	}
	if declaration := commandOverrideDeclaration(name, override, true); declaration != "" {
		sctx.Log(declaration)
	}
	return runShellCommandWithPriority(sctx.Ctx, sctx.WorkDir, env, command, override.Nice)
}

// commandOverrideDeclaration states every machine-local override applied to a
// command so a result produced under one is never presented as a plain run.
// Environment values appear only when withValues is set, for private logs.
func commandOverrideDeclaration(name string, override config.CommandOverride, withValues bool) string {
	var parts []string
	if len(override.Env) != 0 {
		keys := make([]string, 0, len(override.Env))
		for key := range override.Env {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		if withValues {
			for i, key := range keys {
				keys[i] = fmt.Sprintf("%s=%q", key, override.Env[key])
			}
		}
		parts = append(parts, "env "+strings.Join(keys, " "))
	}
	if override.Nice != 0 {
		parts = append(parts, fmt.Sprintf("nice %d", override.Nice))
	}
	if len(override.Additional) != 0 {
		checks := make([]string, len(override.Additional))
		for i, command := range override.Additional {
			checks[i] = fmt.Sprintf("%q", command)
		}
		parts = append(parts, "additional checks "+strings.Join(checks, ", "))
	}
	if len(parts) == 0 {
		return ""
	}
	return fmt.Sprintf("machine-local overrides applied to commands.%s: %s", name, strings.Join(parts, "; "))
}

// checkResult is one configured check's own outcome, so a machine-local
// check's failure is never attributed to the repository's command.
type checkResult struct {
	Command  string
	Local    bool
	ExitCode int
}

func (r checkResult) description(name string) string {
	if r.Local {
		return fmt.Sprintf("machine-local %s check failed with exit code %d: %s", name, r.ExitCode, r.Command)
	}
	return fmt.Sprintf("configured %s command failed with exit code %d", name, r.ExitCode)
}

func failedChecks(results []checkResult) []checkResult {
	var failed []checkResult
	for _, result := range results {
		if result.ExitCode != 0 {
			failed = append(failed, result)
		}
	}
	return failed
}

// runConfiguredChecks runs the repository command (when nonempty) followed by
// the operator's machine-local additional checks for name.
func runConfiguredChecks(sctx *pipeline.StepContext, name, command string) (string, []checkResult, error) {
	override := sctx.Config.CommandOverrides[name]
	checks := []checkResult{}
	if command != "" {
		checks = append(checks, checkResult{Command: command})
	}
	for _, additional := range override.Additional {
		checks = append(checks, checkResult{Command: additional, Local: true})
	}
	var output strings.Builder
	if declaration := commandOverrideDeclaration(name, override, false); declaration != "" {
		fmt.Fprintf(&output, "%s\n", declaration)
	}
	for i := range checks {
		if err := sctx.Ctx.Err(); err != nil {
			return output.String(), checks[:i], err
		}
		if checks[i].Local {
			sctx.Log(fmt.Sprintf("running machine-local %s check: %s", name, checks[i].Command))
			fmt.Fprintf(&output, "\nmachine-local %s check: %s\n", name, checks[i].Command)
		} else if len(override.Additional) > 0 {
			fmt.Fprintf(&output, "\nconfigured %s command: %s\n", name, checks[i].Command)
		}
		out, code, err := runRepositoryCommand(sctx, name, checks[i].Command)
		output.WriteString(out)
		if err != nil {
			return output.String(), checks[:i], err
		}
		checks[i].ExitCode = code
		if len(override.Additional) > 0 {
			fmt.Fprintf(&output, "\nexit code: %d\n", code)
		}
	}
	return output.String(), checks, nil
}
