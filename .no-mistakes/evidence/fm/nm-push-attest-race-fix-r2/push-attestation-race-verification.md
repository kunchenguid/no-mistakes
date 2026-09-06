# Push attestation race verification

This runs the repository-owned `require-no-mistakes` verifier against the same new PR head in the two externally observable PR-body states around publication.

## Stale old attestation against new head

- Attested head: `76fac10000000000000000000000000000000000`
- PR head: `bbda691a00000000000000000000000000000000`
- Exit: `1`
- GitHub output: `exempt=false compliant=false exempt=false `

```text
::error::Pipeline attestation head_sha does not match the current PR head.

attestation.head_sha: 76fac10000000000000000000000000000000000
PR head: bbda691a00000000000000000000000000000000

A later push must not pass on an older attestation. Re-run 'git push no-mistakes' so the PR body attestation binds to the current head.

See CONTRIBUTING.md for setup and the full workflow.

PR author: test-user
Found no-mistakes signature in PR body.

```

## New head attested before publication

- Attested head: `bbda691a00000000000000000000000000000000`
- PR head: `bbda691a00000000000000000000000000000000`
- Exit: `0`
- GitHub output: `exempt=false compliant=true `

```text
Found no-mistakes signature in PR body.
Found structurally compliant pipeline step attestation.
::warning::PR-body attestation is author-editable and is not cryptographic proof that no-mistakes produced it.

```
