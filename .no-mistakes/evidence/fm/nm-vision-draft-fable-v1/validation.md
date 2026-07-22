# VISION.md validation evidence

Validated target `f93abb4d05cc01d795e88da4ca510055165391d3` against base `73e6c4ff487db4018149ab9d934bdf8a68e3d202`.

## End-user surface

The change adds [`VISION.md`](../../../../VISION.md) at the repository root.
The document presents the project vision under eight scannable sections: one gate, safety, human judgment, independent validation, evidence, users, and scope.

## Committed-scope evidence

```text
A       VISION.md
VISION.md | 62 ++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++++
1 file changed, 62 insertions(+)
```

The target is a direct child of the supplied base, the file mode is `100644`, and the commit subject is `docs: add project vision`.

## Acceptance checks

```text
PASS: target is a direct child of base
PASS: diff adds only root VISION.md (mode 100644)
PASS: commit subject uses docs: prefix
PASS: LF text, final newline, and zero em-dash bytes
PASS: every prose line ends as one sentence
PASS: all three required stances are present
PASS: no named vendor, model, or version lock-in terms detected
```

The three policy stances were checked against the document text:

1. Local, accountable-operator scope is explicit, while CI, orchestration, hosting, and team governance are excluded.
2. Model routing is inspectable and user-configured, and validating intelligence is never silently swapped.
3. Intent and judgment remain human-owned, with expanded automation requiring explicit opt-in rather than a silent default change.

## Evidence limitation

No separate copy of the captain-approved draft exists in the repository or supplied test inputs, so this run cannot independently perform the required byte-for-byte comparison against that source artifact.
The committed file itself is the only matching vision artifact in the worktree.
