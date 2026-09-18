package steps

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
)

// These two tests drive the REVIEW STEP through a real adapter (a fake pi
// executable, whose JSON stream is protocol-identical to omp's) rather than a
// hand-rolled mockAgent. That is the layer the two measured production
// failures lived at: the adapter's output shape decided whether the step saw a
// usable review, a rejected one, or nothing at all. A mockAgent returning a
// canned error can prove the retry loop's bookkeeping but cannot prove the
// adapter now absorbs the shape before the step ever counts an attempt.

// reviewStepScriptFixtures writes a stateful fake pi binary that answers each
// invocation from the given scripts in order, so a test can prove how many
// turns the step actually spent and what each one returned.
func reviewStepScriptFixtures(t *testing.T, responses []string) string {
	t.Helper()
	dir := t.TempDir()
	counter := filepath.Join(dir, "invocations")
	// Every response goes through a file rather than a shell printf so a JSON
	// body's quoting is never mangled by the fixture itself.
	for i, body := range responses {
		name := filepath.Join(dir, "response-"+strconv.Itoa(i))
		if err := os.WriteFile(name, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	last := strconv.Itoa(len(responses) - 1)
	bin := filepath.Join(dir, "pi")
	script := `#!/bin/sh
cat > /dev/null
n=0
[ -f "` + counter + `" ] && n=$(cat "` + counter + `")
n=$((n + 1))
printf '%s' "$n" > "` + counter + `"
f="` + dir + `/response-$((n - 1))"
[ -f "$f" ] || f="` + dir + `/response-` + last + `"
printf '%s\n' '{"type":"session","id":"019ff2f3-5f31-744b-90b8-679074ff7681"}'
cat "$f"
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

func TestReviewStep_EmptyTurnIsAbsorbedByTheAdapterNotSpentAsAnAttempt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is Unix-only")
	}
	defer agent.WithFastBackoff()()
	dir, baseSHA, headSHA := setupGitRepo(t)

	// Turn 1 ends with a toolCall-only assistant message and no text part -
	// the shape measured live on three consecutive omp rounds. Turn 2 answers.
	bin := reviewStepScriptFixtures(t, []string{
		`{"type":"message_end","message":{"role":"assistant","content":[{"type":"toolCall","id":"c1","name":"bash"}]}}`,
		`{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":` + jsonString(cleanReviewJSON) + `}]}}`,
	})
	ag, err := agent.New("pi", bin, nil)
	if err != nil {
		t.Fatalf("new pi agent: %v", err)
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	outcome, err := (&ReviewStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("an empty turn is a harness outcome and must not fail the review: %v", err)
	}
	if outcome == nil {
		t.Fatal("expected a review outcome")
	}
	// The step's analyzer budget stays untouched: no rerun note was ever built,
	// because the adapter absorbed the empty turn with its own retry.
	for _, line := range logs {
		if strings.Contains(line, "rerunning the review") {
			t.Fatalf("the step spent an analyzer attempt on the empty turn: %q", logs)
		}
		if strings.Contains(line, "validate review analyzer findings after") {
			t.Fatalf("the step reported an exhausted analyzer bound: %q", logs)
		}
	}
}

func TestReviewStep_EnumRejectionSteersTheRerunWithTheAllowedValues(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is Unix-only")
	}
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	// The measured shape-2 answer: a complete, deliberate review whose
	// top-level risk_scope carries a finding's enum value. The adapter rejects
	// it and the step must hand the allowed values back so the rerun can
	// actually correct the answer instead of repeating it.
	enumViolating := `{"findings":[],"risk_level":"low","risk_rationale":"clean","risk_scope":"source"}`
	bin := reviewStepScriptFixtures(t, []string{
		`{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":` + jsonString(enumViolating) + `}]}}`,
		`{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":` + jsonString(cleanReviewJSON) + `}]}}`,
	})
	ag, err := agent.New("pi", bin, nil)
	if err != nil {
		t.Fatalf("new pi agent: %v", err)
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	var prompts []string
	recording := &promptRecordingAgent{inner: ag, prompts: &prompts}
	sctx.Agent = recording

	outcome, err := (&ReviewStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("a schema slip must rerun the review, not fail the step: %v", err)
	}
	if outcome == nil {
		t.Fatal("expected a review outcome")
	}
	if len(prompts) != 2 {
		t.Fatalf("review prompts = %d, want the rejected review plus exactly one rerun", len(prompts))
	}
	note, ok := strings.CutPrefix(prompts[1], prompts[0])
	if !ok {
		t.Fatalf("rerun prompt is not the original review prompt plus a note")
	}
	if !strings.Contains(note, "REJECTED") {
		t.Fatalf("rerun note = %q, want it to name the rejection", note)
	}
	// The steering content itself: the agent has to learn which values are legal.
	if !strings.Contains(note, `"source-or-external"`) {
		t.Fatalf("rerun note = %q, want the allowed risk_scope values so the rerun can correct the answer", note)
	}
	if !strings.Contains(note, "must match one of the allowed values") {
		t.Fatalf("rerun note = %q, want the enum violation quoted", note)
	}
}

// promptRecordingAgent records every prompt an adapter is asked to answer
// while delegating all real work to it, so a step test can inspect what the
// rerun actually told the agent.
type promptRecordingAgent struct {
	inner   agent.Agent
	prompts *[]string
}

func (a *promptRecordingAgent) Name() string { return a.inner.Name() }

func (a *promptRecordingAgent) Close() error { return a.inner.Close() }

func (a *promptRecordingAgent) Run(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
	*a.prompts = append(*a.prompts, opts.Prompt)
	return a.inner.Run(ctx, opts)
}

// jsonString renders s as a JSON string literal for embedding in a fixture
// JSON stream.
func jsonString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		case '\r':
			b.WriteString(`\r`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
