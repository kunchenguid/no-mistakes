package steps

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"

	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/shellenv"
)

// runCustomGateCommand binds the shell to the existing step ownership record
// before releasing its first instruction, including on a post-fix retry.
func runCustomGateCommand(sctx *pipeline.StepContext, command, outputDir string) (output string, exitCode int, err error) {
	if sctx.DB == nil || sctx.StepResultID == "" {
		return runStepShellCommand(sctx, command)
	}
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(sctx.Ctx, "cmd.exe", "/c", "set /p NM_GATE_READY= || exit 1 & "+command)
	} else {
		cmd = exec.CommandContext(sctx.Ctx, "sh", "-c", "read -r nm_gate_ready || exit 1\n"+shellenv.OwnOOMScoreScript(command))
	}
	shellenv.ConfigureCooperativeShellCommand(cmd)
	cmd.Dir = sctx.WorkDir
	cmd.Env = stepEnvironment(sctx)
	input, release, err := os.Pipe()
	if err != nil {
		return "", -1, err
	}
	defer input.Close()
	defer release.Close()
	cmd.Stdin = input
	// A file preserves bounded process-tree cleanup even if a child inherits
	// stdout and outlives the shell; no pipe reader can hold cmd.Wait open.
	out, err := os.CreateTemp(outputDir, "command-output-")
	if err != nil {
		return "", -1, err
	}
	defer os.Remove(out.Name())
	defer out.Close()
	cmd.Stdout, cmd.Stderr = out, out
	oomBase, oomOK := shellenv.OOMKillBaseline()
	if err := shellenv.StartShellCommand(cmd); err != nil {
		return "", -1, err
	}
	defer shellenv.TerminateShellCommandGroup(cmd)
	pid := cmd.Process.Pid
	if err := sctx.DB.SetStepAgentActivity(sctx.StepResultID, "custom gate command started", &pid); err != nil {
		shellenv.TerminateShellCommandGroup(cmd)
		_ = cmd.Wait()
		return "", -1, fmt.Errorf("record gate command ownership: %w", err)
	}
	defer func() {
		err = errors.Join(err, sctx.DB.SetStepAgentActivity(sctx.StepResultID, "custom gate command exited", nil))
	}()
	_, releaseErr := release.Write([]byte("ready\n"))
	_ = release.Close()
	if releaseErr != nil {
		shellenv.TerminateShellCommandGroup(cmd)
		_ = cmd.Wait()
		return "", -1, releaseErr
	}
	waitErr := shellenv.AttributeOOMKill(oomBase, oomOK, cmd.Wait())
	shellenv.TerminateShellCommandGroup(cmd)
	data, readErr := os.ReadFile(out.Name())
	if readErr != nil {
		return "", -1, readErr
	}
	if waitErr != nil {
		if ee, ok := waitErr.(*exec.ExitError); ok {
			return string(data), ee.ExitCode(), nil
		}
		return string(data), -1, waitErr
	}
	return string(data), 0, nil
}
