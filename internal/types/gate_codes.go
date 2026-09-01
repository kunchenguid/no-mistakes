package types

// FixRoundsExhaustedCode prefixes the error a gate response receives when the
// parked review step has spent its whole review.max_fix_rounds budget and the
// response asked for another fix. It is the machine-readable part of the
// message (the same convention as the nested_gate_context code): a driving
// agent matches on it and falls back to approve, skip, or abort.
const FixRoundsExhaustedCode = "fix_rounds_exhausted"
