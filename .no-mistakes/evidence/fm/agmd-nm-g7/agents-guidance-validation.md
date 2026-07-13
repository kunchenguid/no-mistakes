# AGENTS.md reader-facing validation

Validated target commit `d8f6439da0f500aeb57e5fee1a62ac754eaed529` against base `493fc69d62b3d6b841080233230ce90c5fcf7b6e`.

An agent opening the project's guidance now sees a durable source-of-truth reference for the Go version:

```markdown
**Environment**

- Go version: see `go.mod`
```

The referenced authoritative file currently declares:

```go
go 1.25.0
```

At the end of the same guidance file, the canonical self-governance preamble is present verbatim:

```markdown
## Maintaining this file

Keep this file for knowledge useful to almost every future agent session in this project.
Do not repeat what the codebase already shows; point to the authoritative file or command instead.
Prefer rewriting or pruning existing entries over appending new ones.
When updating this file, preserve this bar for all agents and keep entries concise.
```

The commit changes no other file and has no whitespace errors.
