# Operational Mechanics Deepening Pass

Required pass for `improve-codebase` audits. This lens owns operational-duplication analysis inside the standalone package. Each distinct repeated mechanic must be reported separately and cross-referenced into candidate evidence, seam placement, remedy safety, and migration shape.

## Trigger patterns

Look for:

- Duplicated operational logic across 2+ workflows or orchestration modules.
- Repeated provider/SDK calls.
- Repeated sandbox, email, payment, readiness, command-execution, queue, webhook, storage, auth-adjacent, or integration mechanics.
- Bug fixes that would need to be copied across multiple workflows.
- Orchestration modules that mix domain decisions with low-level mechanics.
- New features sharing mechanics with existing flows.
- User phrases such as "service layer", "shared services", "action vs service", "where should this logic live?", "copy-pasted provider calls", or "same workflow mechanics".

Translate user wording into this skill's vocabulary:

- "service layer" -> "shared-mechanics module"
- "service function" -> "capability function"
- "action" -> "orchestration module"
- "service extraction" -> "operational-mechanics deepening"
- "shared services" -> "shared operational interface"

## Decision rules

Recommend operational-mechanics deepening only when all are true:

- At least two callers need the same mechanic.
- The repeated logic is operational or infrastructural, not domain-specific policy.
- The extracted module can expose explicit inputs and structured outputs.
- Extraction improves locality, testability, and changeability.
- Callers can keep domain policy, state transitions, auth/policy checks, and error classification visible.

Avoid extraction when any are true:

- There is only one caller.
- The logic is primarily domain policy.
- The proposed module would become a god module.
- The extraction hides state transitions, auth/policy checks, or business decisions.
- The result would make debugging harder.
- The proposed interface is nearly as complex as the copied implementation.

Use the deletion test: if deleting the proposed shared-mechanics module would force the same operational complexity back into 2+ callers, it may be earning its keep. If deletion only removes indirection, keep the logic local.

## Good extraction

Good operational-mechanics deepening:

- Moves repeated SDK/provider mechanics behind a small shared operational interface.
- Exposes capability functions with explicit params and structured returns.
- Keeps orchestration modules responsible for "when" and "why".
- Keeps state transitions and policy checks at the caller.
- Creates a focused test surface for the mechanic.

Example shape:

```ts
// shared-mechanics module
export async function sendTransactionalEmail(params: {
  to: string;
  templateId: string;
  variables: Record<string, string>;
}): Promise<{ providerMessageId: string }> {
  return emailProvider.send(params);
}

// orchestration module
if (order.isPaid && user.emailOptIn) {
  await sendTransactionalEmail({
    to: user.email,
    templateId: "order-receipt",
    variables: { orderId: order.id },
  });
}
```

## Bad extraction

Bad operational-mechanics deepening:

- Creates a god module that owns several unrelated mechanics.
- Moves domain state transitions into the shared module.
- Hides auth or policy checks.
- Accepts vague objects or reaches into global state instead of explicit params.
- Returns unstructured booleans or swallowed errors.
- Extracts one-off logic with only one caller.

Example anti-shape:

```ts
// bad: hides policy, state, provider mechanics, and control flow together
await processEntireOrder(userId, orderId, requestContext);
```

## Migration approach

1. Identify duplicated mechanics across 2+ callers.
2. Design the smallest shared operational interface.
3. Keep orchestration/domain policy at the caller.
4. Migrate one caller.
5. Typecheck, lint, and test.
6. Migrate remaining callers.
7. Delete duplicated code.

Prefer one mechanic at a time. Do not refactor every caller in one move unless the repo already has strong tests and the interface is obvious.

## Finding report card

Use finding IDs `O1`, `O2`, etc. for each distinct repeated-mechanic finding. Do not merge different mechanics into one generalized candidate just because they share callers, providers, or a likely destination module:

```md
### O<N>. <repeated mechanic or shared-mechanics module name>

- Files:
- Repeated mechanic:
- Current callers:
- Why this is operational, not domain policy:
- Proposed shared operational interface:
- Caller-owned policy/state that must stay outside:
- Structured outputs:
- Locality/leverage gain:
- Test surface:
- Confidence:
- Evidence grade:
- Safety:
- Relationship notes:
- Migration:
- Avoid if:
- Validation:
```
