package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/kunchenguid/no-mistakes/internal/shellenv"
)

// ompNeutralizationOverlay is the `--config` overlay that suppresses the target
// repository's project agent-instruction files. omp has no flag equivalent of
// pi's --no-context-files for those files; its documented kill-switch is a
// config overlay whose disabledExtensions list names each context file by
// extension id.
//
// The id form is `context-file:<user|project>:<basename>` and is keyed on the
// basename alone, so one `AGENTS.md` entry covers every AGENTS.md in the
// project walk-up as well as the copies under `.agent/`, `.agents/`, and
// `.omp/`. Each basename here was verified by a controlled experiment
// (internal/agent/omp_test.go reproduces it): with the file present and no
// overlay omp receives its contents in the system prompt, and with the overlay
// the same prompt's token count returns exactly to its file-free baseline.
//
// This overlay is NOT sufficient on its own. omp injects further
// project-controlled surfaces through separate capability providers whose
// extension ids are not `context-file:*`, so the overlay cannot name them:
//
//   - `.github/instructions/*.instructions.md` is loaded as a rule
//     (`rule:<basename>`), and its body reaches the model. Measured: with only
//     this overlay a planted `.instructions.md` still governed the turn.
//   - Project skill descriptions (`.github/skills`, `.agents/skills`,
//     `.omp/skills`, `.claude/skills` SKILL.md frontmatter) are listed in the
//     system prompt.
//
// buildArgs therefore also passes omp's own `--no-rules` and `--no-skills`
// kill-switches under the same opt-out, which is what makes those two surfaces
// inert (verified with the same token-accounting experiment). The overlay is
// still required: `--no-rules` does not suppress AGENTS.md/CLAUDE.md/
// copilot-instructions.md.
//
// Even together, none of this closes the project SETTINGS surface: a
// project-local `.omp/config.yml` is loaded with no extension id at all (so it
// cannot be named in disabledExtensions) and omp offers no flag to skip it.
// Measured: `.omp/config.yml` still changed the system prompt delivered to the
// agent under this overlay plus --no-rules plus --no-skills (3/3 trials,
// +472 input+cacheRead tokens), and a project `autoResume` setting also
// defeated the adapter's durable-start argv. That is why omp is not a verified
// neutralizer - see ompAgent.NeutralizesGateInstructions. Everything below is
// retained as defense in depth for a direct caller that sets the opt-out, not
// as a claim the gate relies on.
//
// Ordering matters and is why this must be the operator's only --config
// overlay: `disabledExtensions` REPLACES rather than merges when several
// overlays are supplied, so a later overlay would re-enable every file here.
// --config is reserved for omp in config.reservedAgentArgs for that reason.
const ompNeutralizationOverlay = `disabledExtensions:
  - context-file:project:AGENTS.md
  - context-file:project:CLAUDE.md
  - context-file:project:copilot-instructions.md
`

// ompAgent spawns the omp CLI for each invocation. omp speaks a JSON stream
// protocol identical to pi's --mode json (verified against omp v18: the same
// session header plus agent_start/turn_start/message_start/message_update/
// message_end/turn_end/agent_end events carrying the same assistantMessageEvent
// deltas and the same assistant message shape), so this adapter reuses pi's
// parser and text helpers and only owns what genuinely differs: argv shape,
// the suppression argv it carries under the project-settings opt-out, and the
// session header's identity form.
//
// omp differs from pi in three ways that matter here: it rejects pi's
// --no-context-files and --session-id flags outright, it suppresses project
// agent-instruction files through a --config overlay plus its own --no-rules and
// --no-skills kill-switches rather than one context-file flag, and its
// durable-start shape is the absence of a session flag rather than an explicit
// one.
type ompAgent struct {
	bin       string
	extraArgs []string
	// disableProjectSettings is the resolved, trusted-only opt-out. When true,
	// buildArgs launches omp with an overlay that disables the target repo's
	// project agent-instruction files plus the two flags covering the surfaces
	// the overlay cannot name (see ompNeutralizationOverlay).
	disableProjectSettings bool
	subprocessContext
}

func (a *ompAgent) Name() string { return "omp" }

// SupportsSessionResume reports omp's durable-session capability: JSON mode
// emits a session header carrying its UUID, and `--session <uuid>` reopens that
// exact session. Verified with an ambiguity control - two sessions were created
// with distinct codewords and the OLDER one was resumed, returning its own id
// and its own codeword, so --session is a true selector rather than a flag
// silently ignored in favour of the most recent session.
func (a *ompAgent) SupportsSessionResume() bool { return true }

func (a *ompAgent) ReportsAgentAttempts() bool { return true }

// NeutralizesGateInstructions deliberately fails closed, like grok's. omp's
// context-file overlay plus `--no-rules` and `--no-skills` genuinely close the
// three provider surfaces the overlay and the two flags can name, and the
// adapter keeps passing them as defense in depth. They do not close every
// project-controlled surface: a project-local `.omp/config.yml` carries no
// extension id, so `disabledExtensions` cannot name it, and omp offers no flag
// that skips it. Measured under the adapter's maximal suppression argv (overlay
// + `--no-rules` + `--no-skills`): a project `.omp/config.yml` still changed the
// system prompt delivered to the agent (3/3 trials, +472 input+cacheRead
// tokens), and a project `autoResume` setting defeated the durable-start argv by
// resuming sessions the adapter did not select. No Mistakes must not claim
// verified disable_project_settings support for a harness whose project settings
// surface it cannot close, so the gate refuses omp under the opt-out rather than
// advertising a suppression it cannot demonstrate.
func (a *ompAgent) NeutralizesGateInstructions() bool {
	return false
}

// suppressionApplies reports whether this invocation carries the
// defend-in-depth suppression argv: the opt-out is on and the overlay will be
// passed. It gates buildArgs only and is deliberately NOT the neutralization
// verdict - NeutralizesGateInstructions fails closed because no combination of
// these keys closes omp's project SETTINGS surface.
func (a *ompAgent) suppressionApplies() bool {
	return a.disableProjectSettings && !ompUserSetConfigOverlay(a.extraArgs)
}

func (a *ompAgent) Run(ctx context.Context, opts RunOpts) (*Result, error) {
	return runWithRetry(ctx, "omp", opts, claudeMaxRetries, classifyTransient, nil, func() (*Result, error) {
		return a.runOnce(ctx, opts)
	})
}

func (a *ompAgent) Close() error { return nil }

func (a *ompAgent) runOnce(ctx context.Context, opts RunOpts) (*Result, error) {
	if opts.Session != nil && opts.Session.ID != "" && !isOmpSessionID(opts.Session.ID) {
		// omp accepts an id prefix or a session-file path for its session
		// selector. no-mistakes persists only the full UUID omp minted, so
		// corrupt local metadata cannot turn a recovery attempt into an
		// arbitrary session selection.
		return nil, fmt.Errorf("invalid omp session identity")
	}
	overlayPath, removeOverlay, err := a.writeNeutralizationOverlay()
	if err != nil {
		return nil, err
	}
	defer removeOverlay()

	args := a.buildArgs(opts.Session, overlayPath)
	cmd := exec.CommandContext(ctx, a.bin, args...)
	cmd.Dir = opts.CWD
	cmd.Env = a.gitSafeEnv(opts.CWD, opts.Env)
	shellenv.ConfigureShellCommand(cmd)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("omp stdin pipe: %w", err)
	}

	started, err := startNativeAgentCommand(cmd, nativeAgentActivityObserver(opts, "omp"))
	if err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("omp start: %w", err)
	}
	defer started.closePipes()
	pid := started.pid()
	emitAgentStarted(opts, "omp", pid)

	prompt := buildPiPrompt(opts.Prompt, opts.JSONSchema)
	stdinErrCh := writeNativeAgentStdin(stdin, prompt)

	var stderrBuf []byte
	var stderrWG sync.WaitGroup
	stderrWG.Add(1)
	go func() {
		defer stderrWG.Done()
		stderrBuf, _ = io.ReadAll(started.stderr)
	}()

	pp := &piParser{onChunk: opts.OnChunk}
	if err := pp.parse(ctx, started.stdout); err != nil {
		err = started.waitAfterParseError(err)
		stderrWG.Wait()
		err = errors.Join(err, ompStdinError(<-stdinErrCh))
		retErr := fmt.Errorf("omp parse events: %w", err)
		emitAgentExited(opts, "omp", pid, retErr)
		return failedResult(pp.usage, pp.sessionID), retErr
	}

	waitErr := started.wait()
	stderrWG.Wait()
	stdinErr := ompStdinError(<-stdinErrCh)
	stderr := strings.TrimSpace(string(stderrBuf))
	if waitErr != nil {
		if stderr != "" {
			retErr := fmt.Errorf("omp exited: %w: %s", errors.Join(waitErr, stdinErr), stderr)
			emitAgentExited(opts, "omp", pid, retErr)
			return failedResult(pp.usage, pp.sessionID), retErr
		}
		retErr := fmt.Errorf("omp exited: %w", errors.Join(waitErr, stdinErr))
		emitAgentExited(opts, "omp", pid, retErr)
		return failedResult(pp.usage, pp.sessionID), retErr
	}
	if stdinErr != nil {
		if stderr != "" {
			stdinErr = fmt.Errorf("%w: %s", stdinErr, stderr)
		}
		emitAgentExited(opts, "omp", pid, stdinErr)
		return failedResult(pp.usage, pp.sessionID), stdinErr
	}

	if pp.assistantError != "" {
		retErr := fmt.Errorf("omp reported error: %s", pp.assistantError)
		emitAgentExited(opts, "omp", pid, retErr)
		return failedResult(pp.usage, pp.sessionID), retErr
	}

	text := pp.finalText()
	res, err := finalizeTextResult("omp", text, opts.JSONSchema, pp.usage)
	if res != nil {
		res.Model = pp.model
		res.ModelProvider = pp.provider
	}
	if err == nil && opts.Session != nil {
		// Whichever session omp reports is the one this turn ran in, so record
		// it even when that is not the session we asked to resume - a
		// replacement is exactly what the caller needs to see.
		res.SessionID = pp.sessionID
		switch {
		case pp.sessionID == "":
			// A durable invocation without omp's session header cannot be
			// resumed safely on a later fixer turn.
			err = fmt.Errorf("omp did not report a session identity")
		case opts.Session.ID != "" && pp.sessionID != opts.Session.ID:
			err = fmt.Errorf("omp did not confirm the requested session")
		default:
			res.Resumed = opts.Session.ID != ""
			// omp's agent_end carries only the messages this invocation
			// generated, including after a resume, so usage is not cumulative.
			res.SessionUsageCumulative = false
		}
	}
	emitAgentExited(opts, "omp", pid, err)
	return res, err
}

func ompStdinError(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("omp stdin: %w", err)
}

// writeNeutralizationOverlay materializes ompNeutralizationOverlay when the
// repo opted out of project settings AND the operator did not pin their own
// --config, returning the overlay's absolute path and a removal function. A
// repo that did not opt out writes nothing and gets an empty path, so buildArgs
// adds no --config and omp loads project instruction files exactly as before.
// An operator-pinned --config also yields no file, because that overlay is the
// one omp will actually read (later overlays replace ours) and a temp file
// nothing references would be dead weight.
//
// The overlay lives in a temp file rather than the target checkout: the
// pipeline validates a worktree that must stay clean, and omp resolves
// --config against its cwd, so the path is absolute.
func (a *ompAgent) writeNeutralizationOverlay() (string, func(), error) {
	if !a.disableProjectSettings || ompUserSetConfigOverlay(a.extraArgs) {
		return "", func() {}, nil
	}
	f, err := os.CreateTemp("", "no-mistakes-omp-neutralize-*.yml")
	if err != nil {
		return "", nil, fmt.Errorf("omp neutralization overlay: %w", err)
	}
	path := f.Name()
	remove := func() { _ = os.Remove(path) }
	if _, err := f.WriteString(ompNeutralizationOverlay); err != nil {
		_ = f.Close()
		remove()
		return "", nil, fmt.Errorf("omp neutralization overlay: %w", err)
	}
	if err := f.Close(); err != nil {
		remove()
		return "", nil, fmt.Errorf("omp neutralization overlay: %w", err)
	}
	return path, remove, nil
}

// buildArgs returns the omp argv for one invocation. A nil session is an
// intentionally cold step; an empty SessionRef starts a durable session; a
// populated SessionRef resumes the UUID that no-mistakes previously recorded.
//
// overlayPath is the neutralization overlay from writeNeutralizationOverlay
// (empty when the repo did not opt out). It is placed FIRST so it cannot be
// consumed as the value of a preceding user flag, matching pi's placement of
// its equivalent suppression flag. omp's durable-start shape is the absence of
// a session flag: unlike pi, it has no "start a new named session" flag, and
// --no-session is reserved for the intentional cold step.
func (a *ompAgent) buildArgs(session *SessionRef, overlayPath string) []string {
	args := make([]string, 0, len(a.extraArgs)+7)
	// Project-settings opt-out (trusted-only; see config.DisableProjectSettings):
	// suppress the target repo's AGENTS.md/CLAUDE.md/copilot-instructions.md so an
	// agent-orchestration target cannot install a fleet-captain identity on the
	// gate agent. This is defense in depth, not a neutralization claim: the
	// adapter reports false (see NeutralizesGateInstructions) because omp's
	// project settings surface cannot be closed. Skipped when the operator pinned
	// their own --config, whose overlay would replace ours.
	if overlayPath != "" && !ompUserSetConfigOverlay(a.extraArgs) {
		args = append(args, "--config", overlayPath)
	}
	// The overlay cannot name omp's other two project-controlled surfaces: a
	// `.github/instructions/*.instructions.md` file is a rule (`rule:<basename>`)
	// and a project SKILL.md is listed as a skill. omp's own kill-switches cover
	// them, and both are monotonic - omp has no flag that re-enables rules or
	// skills, so an operator cannot defeat them by pinning one. Neither flag
	// suppresses the context files, which is why the overlay stays required.
	if a.suppressionApplies() {
		args = append(args, "--no-rules", "--no-skills")
	}
	args = append(args, a.extraArgs...)
	args = append(args, "--mode", "json")
	switch {
	case session == nil:
		args = append(args, "--no-session")
	case session.ID != "":
		args = append(args, "--session", session.ID)
	}
	return args
}

// isOmpSessionID accepts only the full canonical UUID omp emits in its JSON
// session header. It shares pi's validator because both emit the same canonical
// 8-4-4-4-12 form (verified against real omp session ids). omp's CLI also
// accepts an id prefix or a session path for its selector; no-mistakes must
// never resume an ambiguous session, so only the full UUID is admitted.
func isOmpSessionID(id string) bool { return isPiSessionID(id) }

// ompUserSetConfigOverlay reports whether extraArgs pin a --config overlay, in
// which case buildArgs does not add its own and neutralization cannot be
// claimed. Handles both `--config <path>` and `--config=<path>`.
func ompUserSetConfigOverlay(extraArgs []string) bool {
	for _, arg := range extraArgs {
		if arg == "--config" || strings.HasPrefix(arg, "--config=") {
			return true
		}
	}
	return false
}
