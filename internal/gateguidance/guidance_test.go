package gateguidance

import "testing"

func TestPromptBoundarySnapshot(t *testing.T) {
	const want = `Gate-step phase boundary:
- You are the document phase inside an already active no-mistakes run. Inspect, fix, and return only this assigned phase.
- Never invoke no-mistakes init, axi run, rerun, respond, sync, abort, eject, or directly push a gate. Never initialize or control another pipeline.
- Delivery requirements in user intent remain authoritative acceptance context for evaluating this change. Do not personally execute other validation, push, PR, or CI phases; the outer executor alone owns every phase other than this assigned one.
- When this phase is complete, return its requested structured result to the outer executor.

`
	if got := PromptBoundary("document"); got != want {
		t.Fatalf("prompt boundary snapshot drifted:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestSkillBoundarySnapshot(t *testing.T) {
	const want = `
## Active validation-step boundary

A no-mistakes validation-step agent is already inside an active outer run. It
must inspect, fix, and return only its assigned phase. It must never initialize,
start, reattach, rerun, respond to, synchronize, abort, eject, or directly push
a no-mistakes pipeline. Delivery requirements in user intent remain
acceptance context, but the outer executor alone performs the other validation,
push, PR, and CI phases.

` + "`NO_MISTAKES_GATE`" + ` is fast diagnostic evidence, not authorization by
itself. The runtime combines managed Git identity with authenticated process
ancestry. If a pipeline-control command returns
` + "`error.code: nested_gate_context`" + `, stop immediately and
return control to the outer executor. Safe inspection remains available through
` + "`no-mistakes axi status`" + `, ` + "`no-mistakes axi logs`" + `, help, and
` + "`no-mistakes doctor`" + `.
`
	if SkillBoundary != want {
		t.Fatalf("skill boundary snapshot drifted:\n--- got ---\n%s\n--- want ---\n%s", SkillBoundary, want)
	}
}
