# Vision

`no-mistakes` exists so that one deliberate push means a change was independently validated before anyone else sees it.
It serves the individual developer - increasingly an operator of many coding agents - who produces changes faster than they can hand-validate, and it turns a rough local branch into a clean, evidence-backed PR while their attention goes elsewhere.
It owns exactly one thing: the gate between a local branch and the configured push target.

## One gate, one meaning

The gate is opt-in and explicit: a named remote you push to on purpose, never a rewired `origin` or a trap door in normal Git behavior.
Pushing through the gate is the consent boundary: it authorizes that run to validate, apply reviewable fixes, push the branch, and raise the PR, and nothing else implies that consent.
"Passed the gate" must mean the same thing in every repo, so the pipeline's shape and order stay fixed and are not a configuration surface.
A person may explicitly skip steps for one run; durable configuration must never be able to quietly weaken what a pass means.
A gate that cannot run completely refuses loudly with guidance; it never degrades silently into a weaker check.
Efficiency never buys itself a skipped check: when a shortcut fails, the gate falls back to the slower correct path instead of skipping the validation.

## Never lose work

The first law is that the tool must not lose people's code: not the author's commits, not out-of-band commits on the target, not fixes the pipeline itself created.
When safety facts cannot be verified against fresh authoritative state, refuse the operation and surface a finding; a refused push is annoying, a lost commit is unforgivable.
Force is never blind: every history-rewriting update must be anchored to what the run actually observed, and a failed verification fails closed.
Work the pipeline holds must always have a safe path back into the user's custody, even after a crash, a cancellation, or a terminal run.
Destructive lifecycle operations require explicit intent, protect other live work by default, and leave an attribution trail.

## Judgment stays human, mechanics do not

The human owns intent, judgment calls, and the merge; the tool owns mechanical validation and objective fixes.
Findings separate what is objectively fixable from what is genuinely the author's call, and an unclassifiable finding fails closed to the human.
Changes that would contradict the author's stated intent park for a decision; they are never auto-resolved.
Unattended operation is real and useful, but it is always explicit consent for a bounded scope, never a quiet default.
Automation of judgment may expand only by explicit user opt-in as trust is earned, never by a silent default flip.
The ambition is to shrink human attention per change toward the few decisions that genuinely need a human, not to remove the human from decisions that are theirs.

## Independent, adversarial validation

Validation runs in a fresh context against the actual branch, never inside the authoring session, because an author is biased toward believing its own work is correct.
Validation must not hold the author hostage: runs happen in disposable isolation so the working tree stays untouched and the next task can start immediately.
Reviewer and fixer are separate roles with separate memory; the reviewer never inherits the fixer's rationale, and every review pass covers the complete change.
The author's intent, with its provenance, is part of what review checks the diff against; agent confidence is not evidence.
The pushed branch is untrusted input: nothing on it may choose what executes with the owner's credentials, and gate agents never adopt the identity or instructions of the code under validation.

## Evidence over confidence

Every verdict must be traceable to something inspectable: findings, executed tests, gathered evidence, and the history of what was fixed and how many attempts it took.
The PR a run raises is written for a reviewer who was not there: what changed, what was checked, what the risks are, and what the pipeline had to fix.
Run state must honestly distinguish working, parked waiting on a human, and dead; a stall that looks alive is a lie.
Failure is a first-class outcome: loud, attributed, explained, and followed by a next action.
Detailed operational data stays on the user's machine; anything that leaves it stays minimal and never becomes a scoreboard.

## Humans and agents are both first-class users

The same gate serves a person at a terminal, a coding agent driving it programmatically, and a supervisor watching many runs; those surfaces share the same approval semantics and earn the same trust.
The gate is agent-agnostic: it must work with whichever supported coding agent the user prefers and keep working when one vendor's tool fails.
Model and effort choices belong to the user; any routing stays inspectable and user-configured, and the gate never silently swaps the intelligence doing the validation.
Host and platform breadth follows real users with real problems; parity with every provider is not promised ahead of demand.

## Scope and evaluation

no-mistakes is a local tool for the person whose credentials and accountability are on the line; runs happen on their machine, under their identity, at their initiative.
It is not a CI system, not an agent orchestrator, not a code host, and not a team-governance platform; CI stays the shared outer gate, and merge policy belongs to the provider.
Every change to this repository must pass through its own gate; dogfooding is the first calibration loop, and field incidents become regression tests before they become memories.
A change aligns when it catches more real mistakes earlier, cuts wall-clock or babysitting without moving judgment away from the human, or strengthens a refusal path.
Changes should be resisted when they weaken what a pass means, trade data safety for convenience, freeze one vendor or model into the product's identity, or grow the gate into an always-on service the user did not ask for.
