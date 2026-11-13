# Versioned client registration

`POST /network/register-client-v1` binds one authenticated network principal and exact creation request to one server-issued client/device. The legacy `/network/auth-client` route keeps its existing behavior.

The versioned request accepts only the exact JSON field names `schema`, `registration_id`, `scope_sha256`, `description`, and `device_spec`, each with a string value. Unknown names, case or Unicode-fold aliases, duplicate decoded names, null values, and trailing data are rejected before the request object changes or any allocation runs. Ordinary JSON escapes that decode to an exact field name remain valid; an escaped duplicate is still a duplicate. Required ownership values are also checked by the allocation boundary. This restriction does not change the legacy route.

The new migration must be applied before advertising this route. Registration allocation and its durable request/scope binding commit in the same transaction. Concurrent duplicates serialize on the network's transaction-owned advisory lock; read-committed isolation lets the waiter observe the preceding allocation. The binding survives client or device deletion, so replay cannot recreate a revoked identity. A changed payload, principal, role authority, request ID, or scope is refused.

Clients must persist their opaque request and stable scope before the first request, replay only that exact request after an unknown reply, and durably install the returned identity before publishing credentials. An older server's missing versioned route is an explicit unsupported capability; clients must not fall back to legacy creation. Deploy migration and server support before enabling fresh registration in approved validator configuration.

Qualification is pending. The candidate has compile-only checks; the six new `TestNetworkClientRegistration` roots require normal/race qualification against real isolated PostgreSQL/Redis. Because the shared `AuthNetworkClient` allocation body is refactored, qualification must include existing legacy client, principal, limit, onboarding and controller roots, followed by a composed full `./model` normal run. Previous full-model results do not qualify this changed body. No live migration or deployment is part of this change.

The original RepeatableRead causal control passed the concurrent identity test
in both modes. That is preserved as an invalid causal claim, not a production
dedup failure: the shared server.Tx owner retries integrity/rollback failures.
The expanded concurrent root now observes real device-insert attempts through
an isolated test-owned PostgreSQL sequence/trigger. Sequence advances survive
rollback, so ReadCommitted must complete with one actual allocation attempt;
the unchanged RepeatableRead control can recover final identity while still
exposing its discarded allocation work. Real Tx retry behavior stays enabled.
This fixture successor is pending independent normal/race qualification.

The SDK route integration successor adds two external `api_test` roots using
the actual `Routes()` table, JWT/session authentication, controller and database
transaction, with the cumulative endpoint-pinned SDK/Connect dependencies. It
breaks a physical HTTP response only after checking the real committed binding,
then requires exact-request recovery of that same client/device. A newly opened
SDK and renewed network bearer must retain that identity. Completed request and
principal conflicts, and revocation through the actual SDK remove-client route,
must preserve the binding without another allocation or legacy fallback.

This is a test-only composition change. Its two roots need their own isolated
normal/race qualification and an actual-route-removal control; it does not
repeat or replace the full model qualification of the earlier changed allocation
body. Successful local SDK HTTP fixtures alone did not cover this route/session/
controller/database seam. Author compilation is not behavioral qualification.
