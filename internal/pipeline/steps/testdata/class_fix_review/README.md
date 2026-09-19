# Class-fix review evaluation fixtures

These are development-only qualitative fixtures for two prompt rules that
together make one review-fix round close a defect class instead of one site of
it: the reviewer's class-once rule (a defect finding enumerates, in that same
finding, every other site in the changed code where the same invariant is
violated or must hold) and the fixer's invariant-complete rule (state the
invariant, enumerate every sibling site, fix all of them in this round with the
same small correction or at one shared boundary, never as machinery). Their
qualitative agent evaluation is not run in CI and does not claim deterministic
coverage; regular tests only verify that the unified diffs remain applicable.
Present each diff to the existing Review pass in a temporary repository,
together with the intent below as `--intent`, and compare the returned findings
with the expectation. To evaluate the fixer, select the finding at the gate
with `no-mistakes axi respond --action fix` and compare the fix round's diff
with the expected fix.

The set is modeled on a measured review-fix spiral (SSHHIP PR #462, 17 review
passes, 11 of them follow-ons of the preceding fix): the same `__proto__` map
fixed in one file and reported in the second, a response validated one field
per round, and a clamp fixed on one axis and reported on the other.

| Fixture | Intent | Expected review behavior | Expected fix behavior |
| --- | --- | --- | --- |
| `proto-map-in-two-files.diff` | Commands register by the name the user types, and a user's config file may alias one name to another. Names and aliases are user input. | One defect finding anchored at the `handlers[name] = handler` assignment in `src/registry.js`, describing that a user-typed name such as `__proto__` or `constructor` indexes the object's prototype chain, and listing `src/aliases.js` (`aliases[name] = target` and the `aliases[name] \|\| name` read) in that same finding as sibling sites of the same invariant, each as file:line. Not two findings across two rounds, and not a finding about the registry alone. | One round replaces both plain-object maps with the same small correction (`Object.create(null)` or `Map`) at both sites, and leaves `names()`/`all()` working for the ordinary path. Not a `hasOwnProperty` guard or a reserved-name denylist in front of the reported assignment only. |
| `partial-response-validation.diff` | Parse the agent's hello response and dial the endpoint it reports. | One defect finding anchored at the `h.ID == ""` check in `transport/hello.go`, stating the invariant (every field the response consumes is validated before it is used) and listing, in that same finding, every consumed field still unvalidated: `Host` (empty host dials the local machine) and `Port` (zero or out-of-range port). Not a finding about `Port` now and `Host` after the next fix. | One round validates `Host` and `Port` beside the existing `ID` check in `parseHello`, the one shared boundary every consumer of the hello passes through. Not a check inside `endpoint`, and not a validation framework, options struct, or per-field validator type. |
| `two-axis-clamp.diff` | Resize and move the pane to what the user asked for, keeping it inside the terminal. | One defect finding anchored at the `Resize` bound in `pane/pane.go`, stating the invariant (a size or position stays inside the terminal on both axes, in both operations) and listing, in that same finding, every sibling site where it must hold: the missing lower bound on `Cols` and `Rows` in `Resize`, and the missing `Y` bounds in `Move`, which clamps `X` on both sides and `Y` on neither. Not one axis or one function per round. | One round closes every listed site with the same small correction (a two-sided clamp on each axis, or one shared `clamp` helper used at all of them). Not a bounds-checking layer, a `Validate` method, or an error-returning API the intent did not ask for. |

A class-once finding is acceptable only when it anchors to one primary site and
names every sibling site inside the same description. A fix round is acceptable
only when it closes every listed site (or the one shared boundary that closes
all of them) with the same small correction and leaves the ordinary path of
every changed function intact; a round that closes the reported site alone, or
that adds handling, state, or a subsystem to manage the symptoms, fails the
fixture.
