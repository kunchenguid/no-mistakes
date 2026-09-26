# Vision

`no-mistakes` exists so that one deliberate push means a change was independently validated before anyone else sees it.
It serves the individual developer - increasingly an operator of many coding agents - who produces changes faster than they can hand-validate, and it turns a rough local branch into a clean, evidence-backed PR while their attention goes elsewhere.
It owns exactly one thing: the gate between a local branch and the configured push target.

## One gate, one meaning

The gate is opt-in and explicit: a named remote you push to on purpose, never a rewired `origin` or a trap door in normal Git behavior.
Pushing through the gate is the consent boundary: it authorizes that run to validate, apply reviewable fixes, push the branch, and raise the PR, and nothing else implies that consent.
"Passed the gate" must mean at least the same thing in every repo: the core pipeline's shape and order stay fixed, and a repository may add checks on top but never remove, reorder, or dilute the core.
Customization is welcome exactly as far as it strengthens what a pass means; no durable configuration, and no classifier's guess about a change's riskiness, may quietly weaken it.
A person may explicitly skip steps for one run; a standing rule may never skip them on anyone's behalf.
A gate that cannot run completely refuses loudly with guidance; it never degrades silently into a weaker check.
Efficiency never buys itself a skipped check: when a shortcut fails, the gate falls back to the slower correct path instead of skipping the validation.
Cost is a user-visible property of the gate, not an implementation detail: pipeline latency and total token spend are already among the first things users raise, so any design that clearly adds significant end-to-end latency or token consumption must be opt-in, or confined to the extremely rare cases where it is genuinely necessary.
The default path buys the strongest guarantee it can at a price a user will keep paying; a stronger guarantee that costs another full pass over the change is offered, never imposed.

## Never lose work

The first law is that the tool must not lose people's code: not the author's commits, not out-of-band commits on the target, not fixes the pipeline itself created.
When safety facts cannot be verified against fresh authoritative state, refuse the operation and surface a finding; a refused push is annoying, a lost commit is unforgivable.
Force is never blind: every history-rewriting update must be anchored to what the run actually observed, and a failed verification fails closed.
Work the pipeline holds must always have a safe path back into the user's custody, even after a crash, a cancellation, or a terminal run.
Destructive lifecycle operations require explicit intent, protect other live work by default, and leave an attribution trail.

## Judgment stays human, mechanics do not

The human owns intent, judgment calls, and the merge decision; by explicit opt-in the tool may perform a merge the human already decided to make, and convenience never becomes judgment transfer.
Findings separate what is objectively fixable from what is genuinely the author's call, and an unclassifiable finding fails closed to the human.
Every judgment has exactly one owner: when two mechanisms would judge the same question, they are reconciled into one, never stacked on top of each other.
Changes that would contradict the author's stated intent park for a decision; they are never auto-resolved.
Unattended operation is real and useful, but it is always explicit consent for a bounded scope, never a quiet default.
Automation of judgment may expand only by explicit user opt-in as trust is earned, never by a silent default flip.
The ambition is to shrink human attention per change toward the few decisions that genuinely need a human, not to remove the human from decisions that are theirs.

## The pipeline runs forward

Validation is a straight line: each step runs once in its fixed order, takes what every earlier step produced, and hands its result forward; a run never sends itself back through a step it has completed.
A step may iterate on its own findings inside its own bounded fix budget; that loop stays inside the step and is the only loop the gate contains by default.
A later step is kept from undoing earlier work by what it is told, not by what runs after it: every step that can change the tree receives the run's record so far, above all every human decision, with the instruction to respect it, and its own gate parks when it cannot comply.
A decision a human makes at any gate is a fact of the run from then on: every later step sees it, treats it as outranking the original intent and any test, fix, or tidy-up that contradicts it, and never reverses it; only a later human ruling on the same concern can.
Each step answers for its own edits at its own gate - a fix Review wrote is rereviewed by Review, a fix Test wrote is retested by Test, a documentation or lint edit is judged by the step that made it - and no step's edit is grounds to send the run back to Review.
Neither a deterministic boundary on what a step may touch nor a rigid guard bolted on after it belongs in the gate: code is unstructured data arranged differently in every repository, so such rules fail where an instruction adapts and bandage poor agent judgment instead of fixing it; when a step still errs despite what it was told, the answer is better context and instruction for that step and a PR that shows the human what it did.
Re-running completed steps on a changed tree has no honest end, because steps that edit by nature edit on every pass; a design that sends a run backward to re-check a later step's work is refused as a default and as a remedy, however bounded or rare it claims to be.
A second pass through the line exists only as an explicit opt-in the user turns on, admitted case by case and never as a pattern; it buys a stronger guarantee the user chose to pay for, never the cure for a later step's defect.
After publication, a CI repair whose continuity with the reviewed head cannot be proven is a new change, not an increment on a validated one: it enters the line again under the CI budget, and nothing before publication qualifies.

## Independent, adversarial validation

Validation runs in a fresh context against the actual branch, never inside the authoring session, because an author is biased toward believing its own work is correct.
Validation must not hold the author hostage: runs happen in disposable isolation so the working tree stays untouched and the next task can start immediately.
Reviewer and fixer are separate roles with separate memory; the reviewer never inherits the fixer's rationale, and every review pass covers the complete change.
The pipeline's own fixes are author code: a change the gate wrote is held to the same standard as a change the author wrote, judged at the gate of the step that wrote it, and a reviewer never certifies its own prescription.
The author's intent, with its provenance, is part of what review checks the diff against; agent confidence is not evidence.
The pushed branch is untrusted input: nothing on it may choose what executes with the owner's credentials, and gate agents never adopt the identity or instructions of the code under validation.

## Evidence over confidence

Every verdict must be traceable to something inspectable: findings, executed tests, gathered evidence, and the history of what was fixed and how many attempts it took.
A verdict is attributable: every run records the exact tool build and configuration that produced it, so a surprising outcome can always be traced to the software that made it.
Evidence stays attached to the change without contaminating it: artifacts are durable and PR-visible, but the shipped branch's history is the author's change and nothing else.
The gate does not define compliance: external systems do, and the gate's duty is to publish facts sufficient for any of them to derive its own verdict. An external repository policy may explicitly exempt a class of pull requests it does not route through no-mistakes, but that policy bypass must stay distinguishable from evidence that the gate passed and must never skip a step inside a no-mistakes run.
The PR a run raises is written for a reviewer who was not there: what changed, what was checked, what the risks are, and what the pipeline had to fix.
Run state must honestly distinguish working, parked waiting on a human, and dead; a stall that looks alive is a lie.
Failure is a first-class outcome: loud, attributed, explained, and followed by a next action.
Detailed operational data stays under the user's sole control: it may follow the same operator across their own machines, but anything that leaves their custody stays minimal and never becomes a scoreboard.

## Humans and agents are both first-class users

The same gate serves a person at a terminal, a coding agent driving it programmatically, and a supervisor watching many runs; those surfaces share the same approval semantics and earn the same trust.
The gate is agent-agnostic: it must work with whichever supported coding agent the user prefers and keep working when one vendor's tool fails.
Model and effort choices belong to the user; any routing stays inspectable and user-configured, and the gate never silently swaps the intelligence doing the validation.
No forge, host, or provider is privileged in the product's identity; breadth still follows real users with real problems rather than promising parity ahead of demand.

## Scope and evaluation

no-mistakes is a local tool for the person whose credentials and accountability are on the line; runs happen on their machine, under their identity, at their initiative.
It is not a CI system, not an agent orchestrator, not a code host, and not a team-governance platform; CI stays the shared outer gate, and merge policy belongs to the provider.
Where a repository genuinely has no outer gate, the inner gate may take on more of that duty by the user's explicit choice.
The gate assumes as little as possible about what a repository contains: code or not, a change is a change, and the gate's question is always whether it is safe to share.
Every change to this repository must pass through its own gate; dogfooding is the first calibration loop, and field incidents become regression tests before they become memories.
A change aligns when it catches more real mistakes earlier, cuts wall-clock or babysitting without moving judgment away from the human, serves the individual operator across everything they own, makes a human decision visible to every step that comes after it, or strengthens a refusal path.
Changes should be resisted when they weaken what a pass means, trade data safety for convenience, send a run back through steps it has completed except by explicit opt-in, spend deep complexity on a niche workflow, freeze one vendor or model into the product's identity, or grow the gate into an always-on service the user did not ask for.
