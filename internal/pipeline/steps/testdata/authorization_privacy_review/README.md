# Authorization/privacy review evaluation fixtures

These are development-only qualitative fixtures for the Review prompt. They are
not run in CI and do not claim deterministic security coverage. Present each
unified diff to the existing Review pass in a temporary repository, together
with the repository policy below, and compare the returned findings with the
expectation.

| Fixture | Repository policy | Expected review behavior |
| --- | --- | --- |
| `route-auth-shared-mutation.diff` | Users may change only their own email; administrators may change any user's email. | Finding: the authenticated route checks ownership, but the shared exported mutation remains callable without actor context. Name the alternate call path and unauthorized mutation. |
| `missing-tenant-filter.diff` | Invoices are private to their tenant. | Finding: the query accepts tenant context but does not constrain by tenant, exposing another tenant's invoices. |
| `public-projection-leak.diff` | `InternalNotes` is staff-only; profile responses are public. | Finding: the public serializer projects the staff-only field. |
| `secondary-log-disclosure.diff` | Password-reset tokens are secret and must not be logged. | Finding: the new log statement discloses the token to log readers. |
| `shared-boundary-authorized.diff` | Users may change only their own email; administrators may change any user's email. | No authorization finding: every caller reaches the service-owned authorization check. |
| `intentionally-public-data.diff` | Display names are intentionally public. | No privacy finding: the response exposes only data explicitly defined as public. |

A positive finding is acceptable only when it identifies the concrete reachable
operation or disclosure, protected resource or field, missing control, and
impact. The negative fixtures should not produce findings merely because a
particular middleware, helper name, or auth-related test is absent.
