# CI Workflow Generation

The `no-mistakes ci-workflow` command auto-generates `.github/workflows/ci.yml` from your `.no-mistakes.yaml` configuration, ensuring that GitHub checks align with the gate's canonical commands.

## Why This Matters

When a new repo is initialized with `no-mistakes init`, the gate is ready to validate pushes locally — but GitHub has no CI workflow to register checks for. The gate's `ci` step sits idle (or times out) waiting for status checks that never appear.

This command generates those checks automatically from the source of truth: `.no-mistakes.yaml`.

## Quick Start

After initializing your repo with `no-mistakes init`:

```bash
no-mistakes ci-workflow
```

This creates `.github/workflows/ci.yml` with steps that mirror your configured lint and test commands.

Commit and push the workflow file:

```bash
git add .github/workflows/ci.yml
git commit -m 'ci: register workflow'
git push no-mistakes main
```

## Example

### Before

A freshly initialized repo:

```bash
$ ls -la .github/workflows/
ls: .github/workflows/: No such file or directory

$ cat .no-mistakes.yaml
commands:
  lint: "go vet ./... && gofmt -l ."
  test: "go test ./... -race"
```

Running `no-mistakes ci-workflow`:

```bash
$ no-mistakes ci-workflow
  ✓  CI workflow generated
      .github/workflows/ci.yml

  Commit and push this file to enable GitHub checks:
  git add .github/workflows/ci.yml && git commit -m 'ci: register workflow'
```

### After

GitHub Actions now registers real checks:

```yaml
name: CI

on:
  push:
    branches: [main]
  pull_request:

permissions:
  contents: read

concurrency:
  group: ci-${{ github.ref }}
  cancel-in-progress: true

jobs:
  build-test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4

      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
          check-latest: true

      - name: Lint
        run: |
          go vet ./... && gofmt -l .

      - name: Test
        run: |
          go test ./... -race
```

When you push through the no-mistakes gate or open a PR, GitHub now has real checks to gate on. The gate's `ci` step completes successfully instead of timing out.

**Note:** The `push` branch is resolved from your repository's default branch (not hardcoded to `main`). The `pull_request` trigger runs on all PRs. Commands are placed in YAML block scalars so special characters and multi-line commands stay intact.

## Options

### Force Overwrite

If you need to regenerate the workflow (e.g., after changing `.no-mistakes.yaml`), use `--force`:

```bash
no-mistakes ci-workflow --force
```

## Notes

- The generated workflow is Go-focused (uses `setup-go` action). Non-Go repos can adapt the template as needed.
- Commands are inserted verbatim from `.no-mistakes.yaml`, so ensure they're portable to GitHub's Ubuntu runner.
- The workflow runs on `push: main` and all `pull_request` events.
