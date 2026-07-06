# Deepening

How to deepen a cluster of shallow modules safely, given its dependencies. Assumes the vocabulary in [LANGUAGE.md](LANGUAGE.md) — **module**, **interface**, **seam**, **adapter**.

## Output contract

When reporting deepening findings, produce a standalone Deepening Report. Use finding IDs `D1`, `D2`, etc. Keep each finding separate when it has distinct files, dependency category, seam decision, test surface, source evidence, consequence, or remediation. For each finding, include: files, dependency category, depth/locality problem, over-engineering or weak-seam evidence when present, consequence, recommendation, confidence, evidence grade, safety, relationship notes, and validation. Do not merge findings only because they share a broad theme.

## Dependency categories

When assessing a candidate for deepening, classify its dependencies. The category determines how the deepened module is tested across its seam.

## Relationship to topology findings

Deepening does not own module topology, file-size, hierarchy, workspace-folder, related-file cluster, or package-home inventory. The dedicated [MODULE-TOPOLOGY.md](MODULE-TOPOLOGY.md) lens owns that structural evidence.

Use topology finding IDs as cross-references when they explain a depth, locality, seam, interface, leverage, or test-surface problem. Do not rerun the topology inventory inside Deepening.

Use Code Health finding or ledger IDs as cross-references when a depth/seam problem is also a legacy, unnecessary, speculative, compatibility-smell, or over-engineered cleanup candidate. Code Health and Cleanup owns the prove-or-delete disposition; Deepening supplies seam/depth evidence.

### 1. In-process

Pure computation, in-memory state, no I/O. Always deepenable — merge the modules and test through the new interface directly. No adapter needed.

### 2. Local-substitutable

Dependencies that have local test stand-ins (PGLite for Postgres, in-memory filesystem). Deepenable if the stand-in exists. The deepened module is tested with the stand-in running in the test suite. The seam is internal; no port at the module's external interface.

### 3. Remote but owned (Ports & Adapters)

Your own services across a network boundary (microservices, internal APIs). Define a **port** (interface) at the seam. The deep module owns the logic; the transport is injected as an **adapter**. Tests use an in-memory adapter. Production uses an HTTP/gRPC/queue adapter.

Recommendation shape: *"Define a port at the seam, implement an HTTP adapter for production and an in-memory adapter for testing, so the logic sits in one deep module even though it's deployed across a network."*

### 4. True external (Mock)

Third-party services (Stripe, Twilio, etc.) you don't control. The deepened module takes the external dependency as an injected port; tests provide a mock adapter.

## Seam discipline

- **One adapter means a hypothetical seam. Two adapters means a real one.** Don't introduce a port unless at least two adapters are justified (typically production + test). A single-adapter seam is just indirection.
- **Internal seams vs external seams.** A deep module can have internal seams (private to its implementation, used by its own tests) as well as the external seam at its interface. Don't expose internal seams through the interface just because tests use them.
- Flag speculative ports, adapters, options, flags, strategy objects, and helper seams that have no current caller value. If the abstraction exists only for imagined future flexibility, report it as an over-engineered shallow abstraction and cross-reference the Code Health prove-or-delete ledger.
- Flag pass-through interfaces that only rename a call, add mocking surface, or preserve an old facade without improving locality or leverage.

## Testing strategy: replace, don't layer

- Old unit tests on shallow modules become waste once tests at the deepened module's interface exist — delete them.
- Write new tests at the deepened module's interface. The **interface is the test surface**.
- Tests assert on observable outcomes through the interface, not internal state.
- Tests should survive internal refactors — they describe behaviour, not implementation. If a test has to change when the implementation changes, it's testing past the interface.
- Over-engineered test helper layers, broad fixtures, patch helpers, fake adapters, and mock scaffolds are deepening evidence when they expose the wrong seam. Simplify or delete the helper layer once current behavior is protected through the deeper interface.
