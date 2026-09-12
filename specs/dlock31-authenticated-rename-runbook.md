# DLOCK31 authenticated rename smoke

This opt-in, read-only test uses the authorized private disposable fixture; it
does not create, rename, delete, push, or alter the fixture or credentials.

```sh
DLOCK31_GITHUB_OLD=mwaddo/dlock31-disposable-rename-20260911-1849 DLOCK31_GITHUB_CANONICAL=mwaddo/dlock31-disposable-renamed-20260911-1849 go test -tags=e2e ./internal/scm/github -run "^TestDLOCK31AuthenticatedRename$" -count=1 -v
```

The selector must execute, not skip or report no matching tests.
