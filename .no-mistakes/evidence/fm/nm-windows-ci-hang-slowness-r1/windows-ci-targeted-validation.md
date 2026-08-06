# Windows CI targeted validation

Validated on 2026-08-02 from target commit `16b76dffee270f013db24c68665b220ff28549cb`.

## Historical end-user reproduction

`gh-axi run view 30740993935 --job 91482255805 --log` showed:

- workflow: `CI`
- job: `test (windows-latest)`
- result: `cancelled`
- `Test on Windows`: `cancelled`
- `Build`: `skipped`
- the log continued reporting normally completed packages before GitHub emitted `The operation was canceled` at the 25-minute job cap

This directly reproduces the pre-fix user experience: the Windows test process made progress, but the job-level timeout cancelled it before the build step.

## Changed workflow contract

```text
$ go test . -run 'TestCIWorkflow_Windows(TestsRunWithScanExclusions|HangSurfacesAsGoTimeoutNotJobCancellation)$' -count=1
ok  github.com/kunchenguid/no-mistakes  0.374s
```

The workflow was parsed into its typed YAML model. The checks establish that the Windows-only PowerShell step semantically invokes both Defender exclusion modes before `go test`, and that the Go test timeout remains below the 25-minute GitHub job timeout.

## Process-heavy package after the change

```text
$ /usr/bin/time -p go test -race ./internal/branchsync -count=2 -parallel=4
ok  github.com/kunchenguid/no-mistakes/internal/branchsync  54.660s
real 55.05
user 62.52
sys 95.62
```

The two complete race-enabled executions passed at four-way parallelism, representative of the GitHub-hosted Windows runner's effective test concurrency. A final default-concurrency diagnostic also passed:

```text
$ go test -race -v -timeout=75s ./internal/branchsync -count=1
PASS
ok  github.com/kunchenguid/no-mistakes/internal/branchsync  24.762s
```

The focused case implicated by one earlier resource-contention failure passed ten consecutive race-enabled repetitions.

## Remaining acceptance evidence

At validation time, both of these returned zero results:

```text
$ gh-axi pr list --head fm/nm-windows-ci-hang-slowness-r1
count: 0

$ gh-axi run list --branch fm/nm-windows-ci-hang-slowness-r1
count: 0
```

Therefore no honest post-fix Windows elapsed time can be reported yet. The branch's first GitHub `test (windows-latest)` result must supply the required after-runtime comparison against the unchanged 25-minute cap.
