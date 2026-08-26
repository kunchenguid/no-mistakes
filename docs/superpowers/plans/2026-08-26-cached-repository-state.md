# Cached Repository State Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make `no-mistakes status` always render cached, local-only repository branch evidence without adding a pipeline or network operation.

**Architecture:** `internal/cli/status.go` already obtains `branchsync.State` through `InspectCached`. Add one small presenter for that state and render it unconditionally after repository discovery. Include the rendered cached summary in the existing status telemetry fingerprint so sampled status events correspond to visible state.

**Tech Stack:** Go, Cobra, existing `branchsync.Service`, existing CLI test helpers.

---

## File structure

- Modify: `internal/cli/status.go` - render cached evidence and fingerprint it.
- Create: `internal/cli/status_test.go` - table tests for the pure cached-state presenter and fingerprint coverage.

### Task 1: Write the failing presenter test

**Files:**

- Create: `internal/cli/status_test.go`

- [ ] **Step 1: Define clean, dirty, and unavailable cases**

```go
func TestCachedBranchSummary(t *testing.T) {
    tests := []struct { name string; state branchsync.State; want string }{
        {"clean branch", branchsync.State{State: branchsync.StateSynchronized, Local: branchsync.LocalState{Branch: "feature/state", Head: "0123456789abcdef", Clean: true}}, "cached: feature/state 01234567 (clean; already synchronized with the pipeline-pushed head)"},
        {"dirty branch", branchsync.State{State: branchsync.StateDirty, Local: branchsync.LocalState{Branch: "feature/state", Head: "fedcba9876543210", Reason: "uncommitted changes"}}, "cached: feature/state fedcba98 (dirty: uncommitted changes; dirty)"},
        {"unavailable", branchsync.State{State: branchsync.StateAmbiguousContext}, "cached: unavailable (ambiguous context)"},
    }
    for _, tt := range tests { t.Run(tt.name, func(t *testing.T) {
        if got := cachedBranchSummary(tt.state); got != tt.want { t.Fatalf("cachedBranchSummary() = %q, want %q", got, tt.want) }
    }) }
}
```

- [ ] **Step 2: Verify red**

Run: `go test ./internal/cli -run '^TestCachedBranchSummary$'`

Expected: compile failure because `cachedBranchSummary` does not exist.

### Task 2: Add the local-only presenter and render it

**Files:**

- Modify: `internal/cli/status.go`
- Test: `internal/cli/status_test.go`

- [ ] **Step 1: Implement the presenter**

```go
func cachedBranchSummary(state branchsync.State) string {
    summary := humanSyncSummary(state)
    if state.Local.Branch == "" || state.Local.Head == "" { return "cached: unavailable (" + summary + ")" }
    head := state.Local.Head[:minLen(len(state.Local.Head), 8)]
    cleanliness := "clean"
    if !state.Local.Clean { cleanliness = "dirty"; if state.Local.Reason != "" { cleanliness += ": " + state.Local.Reason } }
    return fmt.Sprintf("cached: %s %s (%s; %s)", state.Local.Branch, head, cleanliness, summary)
}
```

- [ ] **Step 2: Replace the conditional cached-state rendering**

```go
syncState := (&branchsync.Service{DB: d, Repo: repo, WorkDir: "."}).InspectCached(cmd.Context())
cachedSummary := cachedBranchSummary(syncState)
fmt.Fprintf(w, "\n  %s  %s\n", sDim.Render("local state:"), cachedSummary)
```

- [ ] **Step 3: Verify green**

Run: `go test ./internal/cli -run '^TestCachedBranchSummary$'`

Expected: PASS.

### Task 3: Keep telemetry aligned with rendered evidence

**Files:**

- Modify: `internal/cli/status.go`
- Modify: `internal/cli/status_test.go`

- [ ] **Step 1: Add a failing fingerprint regression test**

```go
func TestStatusFingerprintIncludesCachedSummary(t *testing.T) {
    run := &db.Run{ID: "run-1", Branch: "feature/test", Status: "running", HeadSHA: "head-one"}
    before := statusFingerprint("repo", "running", run, "cached: main 01234567 (clean; synchronized)")
    after := statusFingerprint("repo", "running", run, "cached: main 89abcdef (dirty; dirty)")
    if before == after { t.Fatal("changing displayed cached evidence must change the status fingerprint") }
}
```

- [ ] **Step 2: Verify red**

Run: `go test ./internal/cli -run '^TestStatusFingerprintIncludesCachedSummary$'`

Expected: compile failure until `statusFingerprint` accepts the cached summary.

- [ ] **Step 3: Update signature and call site**

```go
fingerprint := statusFingerprint(repo.ID, daemonState, activeRun, cachedSummary)
```

Build the fingerprint from repository id, daemon state, cached summary, and
the existing active-run fields. Update the existing active-run-head test with
the fourth argument.

- [ ] **Step 4: Verify focused tests**

Run: `go test ./internal/cli -run 'Test(CachedBranchSummary|StatusFingerprint)'`

Expected: PASS.

### Task 4: Validate and prepare review

**Files:**

- Modify: `internal/cli/status.go`
- Create: `internal/cli/status_test.go`

- [ ] **Step 1: Add command-level status coverage**

Use `setupTestRepo`, `executeCmd("init")`, and `executeCmd("status")` to
assert the clean and a newly dirty worktree both retain `repo`, `daemon`, and
`no active run` output and always include `local state:  cached:`. The clean
case must include `(clean;`; the dirty case must include `(dirty:`.

Run: `go test ./internal/cli -run '^TestStatusAlwaysRendersCachedLocalState$' -count=1`

Expected: PASS.

- [ ] **Step 2: Format and run full gates**

Run: `gofmt -w internal/cli/status.go internal/cli/status_test.go && make lint && go test -race ./... && go build -o ./bin/no-mistakes ./cmd/no-mistakes`

Expected: each command exits 0.

- [ ] **Step 3: Inspect the review diff**

Run: `git diff --check && git diff -- internal/cli/status.go internal/cli/status_test.go`

Expected: no whitespace errors and no source file outside the planned scope.

- [ ] **Step 4: Commit after fresh evidence**

```bash
git add internal/cli/status.go internal/cli/status_test.go
git commit -m "feat(status): show cached repository state"
```
