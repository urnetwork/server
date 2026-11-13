# Original request closure

`POST /verify/original/close` accepts the canonical signed
`protocol.ProviderAttemptRequestClosure` from the original request VPK. The
validator seals its epoch request cut and drains its sends before consenting to
closure. Scope must match the independently configured Server deployment. The
original SEED or EXTEND signature and separate closure signature must both verify.

Under the same request lock as migration 773, Server returns the exact committed
original receipt if one exists. Otherwise migration 775 retains the original
closure and a signed `closed_unreceived` receipt atomically. The response has
exactly one of `original` or `closed_unreceived`. Exact retry returns the first
receipt, including its historical server key. A changed closure for a fenced
request refuses; rotation and directory retirement do not mint a new receipt.

A second wire lock and database insert guards cover rolling assignment writers.
After closure, both the ordinary SEED/EXTEND path and a direct legacy original
insert refuse before publishing exposure. Closing an extension never removes an
earlier assignment or its still-pending provider exposure. SQL cannot retain both
an original assignment and an unreceived tombstone for the same scoped request.

Each API route set admits at most four operations, with an 8 KiB canonical body,
60-second native body deadline and 300-second total owner. Existing source-IP and
VPK hard rates also apply. No new private key, nonce or money authority is created.

The receipt proves a permanent fence for one request. Independent registry,
request-cut membership, window completeness and original provider eligibility
remain mandatory consumer checks. SQL absence and this endpoint never select the
global verifier population. Missing historical requests remain unknown unless
their original VPK actually consents to this prospective execution fence.

Migration 775 appends after 774. The uninstalled 773 correction uses the actual
original `trail.ClientId` JSON field in its backfill and insert trigger; the wire
grammar and all original signed bytes remain unchanged.
