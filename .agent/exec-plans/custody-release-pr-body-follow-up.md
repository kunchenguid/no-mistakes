# Custody release: PR-body follow-up

Include this disclosure in the existing PR body's Risk and Rollout section, preserving its generated Pipeline section.

Durable published-release provenance is deferred under the recorded R13 decision. The [custody release/reconcile CLI reference](../../docs/src/content/docs/reference/cli.md#no-mistakes-custody-release--reconcile) owns the user-facing limitation and next-run guidance; disclose that limitation in the PR body.

A separately authorized follow-up must durably distinguish a completed published release from ordinary custody recovery and failed release attempts before changing cached inspection or synchronization classification. Preserve historical push provenance and test successful releases, refusals after archival, ordinary recovery, retries and changed evidence across restart.

The executable regression `TestReleasePublishedRefusalDoesNotCreditOrdinaryRecoveryAsPublished` covers the refusal followed by ordinary recovery for both release and reconcile. `TestReleasePublishedClassificationKeepsHistoricalPushProvenance` covers successful command responses at equal, ancestor, descendant and divergent published replacements without crediting later cached inspection as proof of release completion.
