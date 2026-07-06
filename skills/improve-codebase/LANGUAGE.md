# Language

Shared vocabulary for every suggestion this skill makes. Use these terms exactly — don't substitute "component," "service," "API," or "boundary." Consistent language is the whole point.

## Terms

**Module**
Anything with an interface and an implementation. Deliberately scale-agnostic — applies equally to a function, class, package, or tier-spanning slice.
_Avoid_: unit, component, service.

**Interface**
Everything a caller must know to use the module correctly. Includes the type signature, but also invariants, ordering constraints, error modes, required configuration, and performance characteristics.
_Avoid_: API, signature (too narrow — those refer only to the type-level surface).

**Implementation**
What's inside a module — its body of code. Distinct from **Adapter**: a thing can be a small adapter with a large implementation (a Postgres repo) or a large adapter with a small implementation (an in-memory fake). Reach for "adapter" when the seam is the topic; "implementation" otherwise.

**Depth**
Leverage at the interface — the amount of behaviour a caller (or test) can exercise per unit of interface they have to learn. A module is **deep** when a large amount of behaviour sits behind a small interface. A module is **shallow** when the interface is nearly as complex as the implementation.

**Seam** _(from Michael Feathers)_
A place where you can alter behaviour without editing in that place. The *location* at which a module's interface lives. Choosing where to put the seam is its own design decision, distinct from what goes behind it.
_Avoid_: boundary (overloaded with DDD's bounded context).

**Adapter**
A concrete thing that satisfies an interface at a seam. Describes *role* (what slot it fills), not substance (what's inside).

**Operational mechanics**
Reusable infrastructural behaviour that appears behind product flows: provider or SDK calls, command execution, queue operations, webhook plumbing, email/payment/sandbox/storage mechanics, readiness checks, retry loops, and auth-adjacent glue. Domain policy, state transitions, and business decisions are not operational mechanics.

**Shared-mechanics module**
A deep module that exposes a **shared operational interface** for repeated operational mechanics. It hides low-level mechanics while keeping caller-owned policy visible.
_Avoid_: service layer, shared service.

**Capability function**
A composable function exposed by a shared-mechanics module. It accepts explicit inputs and returns structured outputs.
_Avoid_: service function.

**Orchestration module**
Caller code that owns domain policy, state transitions, and when to use capability functions.
_Avoid_: action.

**Leverage**
What callers get from depth. More capability per unit of interface they have to learn. One implementation pays back across N call sites and M tests.

**Locality**
What maintainers get from depth. Change, bugs, knowledge, and verification concentrate at one place rather than spreading across callers. Fix once, fixed everywhere.

## Principles

- **Depth is a property of the interface, not the implementation.** A deep module can be internally composed of small, mockable, swappable parts — they just aren't part of the interface. A module can have **internal seams** (private to its implementation, used by its own tests) as well as the **external seam** at its interface.
- **The deletion test.** Imagine deleting the module. If complexity vanishes, the module wasn't hiding anything (it was a pass-through). If complexity reappears across N callers, the module was earning its keep.
- **The interface is the test surface.** Callers and tests cross the same seam. If you want to test *past* the interface, the module is probably the wrong shape.
- **One adapter means a hypothetical seam. Two adapters means a real one.** Don't introduce a seam unless something actually varies across it.
- **Operational-mechanics deepening needs 2+ callers.** Extract repeated operational mechanics only when the shared operational interface improves locality, testability, and changeability. Keep domain policy in orchestration modules.
- **Code Health labels are diagnostic labels.** Health findings may use fixed labels such as Cognitive Overload, Change Propagation, Knowledge Duplication, Accidental Complexity, Dependency Disorder, and Domain Model Distortion. Remedies must translate back into this skill's vocabulary: **module**, **interface**, **seam**, **adapter**, **depth**, **locality**, **leverage**, and **shared-mechanics module**.

## Relationships

- A **Module** has exactly one **Interface** (the surface it presents to callers and tests).
- **Depth** is a property of a **Module**, measured against its **Interface**.
- A **Seam** is where a **Module**'s **Interface** lives.
- An **Adapter** sits at a **Seam** and satisfies the **Interface**.
- A **Shared-mechanics module** exposes a **shared operational interface**.
- A **Capability function** belongs behind that interface.
- An **Orchestration module** calls capability functions while retaining policy and state decisions.
- **Depth** produces **Leverage** for callers and **Locality** for maintainers.

## Rejected framings

- **Depth as ratio of implementation-lines to interface-lines** (Ousterhout): rewards padding the implementation. We use depth-as-leverage instead.
- **"Interface" as the TypeScript `interface` keyword or a class's public methods**: too narrow — interface here includes every fact a caller must know.
- **"Boundary"**: overloaded with DDD's bounded context. Say **seam** or **interface**.
- **"Service layer" / "shared services"**: too vague and often becomes a dumping ground. Say **shared-mechanics module** or **shared operational interface**.
- **"Service function"**: say **capability function**.
- **"Action"**: say **orchestration module** when discussing architecture.
