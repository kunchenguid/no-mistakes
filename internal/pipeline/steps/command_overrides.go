package steps

import (
	"fmt"
	"strings"

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
	return runShellCommandWithPriority(sctx.Ctx, sctx.WorkDir, env, command, override.Nice)
}

func configuredCheckCommands(sctx *pipeline.StepContext, name, command string) []string {
	var commands []string
	if command != "" {
		commands = append(commands, command)
	}
	return append(commands, sctx.Config.CommandOverrides[name].Additional...)
}

func runConfiguredChecks(sctx *pipeline.StepContext, name string, commands []string) (string, int, error) {
	var output strings.Builder
	firstFailure := 0
	for i, command := range commands {
		if err := sctx.Ctx.Err(); err != nil {
			return output.String(), -1, err
		}
		if len(commands) > 1 || len(sctx.Config.CommandOverrides[name].Additional) > 0 {
			sctx.Log(fmt.Sprintf("running %s check %d: %s", name, i+1, command))
			fmt.Fprintf(&output, "\n%s check %d: %s\n", name, i+1, command)
		}
		out, code, err := runRepositoryCommand(sctx, name, command)
		output.WriteString(out)
		if err != nil {
			return output.String(), -1, err
		}
		if code != 0 && firstFailure == 0 {
			firstFailure = code
		}
		if len(sctx.Config.CommandOverrides[name].Additional) > 0 {
			fmt.Fprintf(&output, "\nexit code: %d\n", code)
		}
	}
	return output.String(), firstFailure, nil
}
