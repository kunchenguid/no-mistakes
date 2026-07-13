# Durable Basecamp References — End-to-End Evidence

Validation card: <https://app.basecamp.com/3594299/buckets/46150003/card_tables/cards/10077092945>

## 1. Canonical `app.basecamp.com` URL detected from intent

Extracted refs: cardID="10077092945" url="https://app.basecamp.com/3594299/buckets/46150003/card_tables/cards/10077092945"

Rendered PR section:

```markdown
## Basecamp

- [Basecamp card 10077092945](https://app.basecamp.com/3594299/buckets/46150003/card_tables/cards/10077092945)
```

## 2. Legacy `3.basecamp.com` URL from commit metadata (kept verbatim, still linked)

Extracted refs: cardID="10077092945" url="https://3.basecamp.com/3594299/buckets/46150003/card_tables/cards/10077092945"

Rendered PR section:

```markdown
## Basecamp

- [Basecamp card 10077092945](https://3.basecamp.com/3594299/buckets/46150003/card_tables/cards/10077092945)
```

## 3. Bare card IDs (`BC#<id>` / `Basecamp card <id>`) stay visible, emit non-blocking warnings

Rendered PR section:

```markdown
## Basecamp

- Basecamp card 10077092945 — canonical URL not provided
- Basecamp card 20099990000 — canonical URL not provided
```

Emitted PR findings (non-blocking):

- severity=`warning` action=`no-op` — Basecamp card 10077092945 has no canonical URL; the PR body includes an unlinked reference.
- severity=`warning` action=`no-op` — Basecamp card 20099990000 has no canonical URL; the PR body includes an unlinked reference.

## 4. Idempotent regeneration: dedupe by card ID, intent URL beats legacy commit URL

Same card referenced 3 ways (intent canonical + commit legacy + bare) collapses to one entry, canonical wins:

Refs: 1 entry, cardID="10077092945" url="https://app.basecamp.com/3594299/buckets/46150003/card_tables/cards/10077092945"

Rendered PR section:

```markdown
## Basecamp

- [Basecamp card 10077092945](https://app.basecamp.com/3594299/buckets/46150003/card_tables/cards/10077092945)
```

## 5. Body-limit preservation: Basecamp survives, Testing/old Pipeline shed first

Final body length = 63136 bytes (GitHub safety cap = 63488 bytes).

Basecamp section present under cap: true
Section order Intent < Basecamp < What Changed: true

## 6. Example assembled PR body (section layout)

```markdown
## Intent

Keep the Basecamp card visible under body limits.

## Basecamp

- [Basecamp card 10077092945](https://app.basecamp.com/3594299/buckets/46150003/card_tables/cards/10077092945)

## What Changed

- preserve durable Basecamp context

## Risk Assessment

✅ Low: PR metadata only

## Pipeline

_(pipeline history — truncated in this excerpt)_

```
