# Bounded legacy settlement target trace

The diagnostic is disabled by default. It observes one private target on normal
`FlushLegacySettlements` pages, or a one-ID call through the existing
`DrainLegacySettlements` owner. It does not select a target, change a task,
change accounting or scheduling, or authorize an attempt.

Set `URN_LEGACY_SETTLEMENT_TRACE_FILE` to a local regular mode-0600 JSON file.
Install the complete file before the process's first traced page/call. The path
may be supplied through private operator configuration; never put target keys
in checked-in settings, source, a command line, fixtures, or metrics. The plan
is read once outside financial/admission ownership. An absent, invalid, or
unreadable plan disables tracing for that process generation.

The exact keys are `capture`, `target_digest`, `shard`, `starts`, `expires`, and
`max_pages`. `capture` is a fresh random 16-byte value encoded as 32 lowercase
hex characters. `target_digest` is lowercase hex SHA-256 of the ASCII capture,
a zero byte, and the target's raw 16-byte contract ID. The integer shard must
match the private target's ordinary hash shard. Both times are absolute RFC3339
UTC values, with a window of at most 30 minutes. `max_pages` is 1 through 256.
Each process generation consumes one slot per matching-shard invocation; an
explicit one-ID call must also match the target digest. Caps are per process,
not a fleet-wide count, and restarts cannot extend the absolute expiry.

No request or CLI grammar changed. The existing `contracts drain-legacy`
preview/apply protocol retains every existing custody and financial guard.
Its trace origin is `explicit_preview` or `explicit_apply`; this is not proof
that the automatic queue selected the target. `automatic_page` records target
selection only after the existing selector returns that exact ID. Neighbor
financial operations have no target observer. A page with `selected=0` is
only a finite observation of that invocation.

When enabled, the existing private result gains `trace`. It contains capture,
a random per-invocation run ID, start time, origin, whole input/result cursor
fingerprints, selected count, return/cause status, at most 48 typed events,
and explicit publication-drop and event-cap flags. Cursor fingerprints are
SHA-256 of ASCII capture, `\x00cursor\x00`, and Go JSON encoding of the complete
`LegacySettlementCursor` (including HeadAfter), or `null`. The existing private
result still owns the actual cursor; the trace exposes no raw row key. An
explicit owner has no queue cursor. The run ID joins each streamed event to
its final private result and, for an automatic invocation, to its exact
finished-task row and invocation arguments.

Events contain only fixed stage/state strings, event/attempt ordinals, client
monotonic microseconds since entry, and finite counts/durations. No raw SQL,
errors, IDs, amounts, tokens, or customer labels are emitted. SQLSTATE-derived
categories are deliberately finite. Unclassified or swallowed failures stay
`unavailable`; a returned function is not an external acknowledgement.

Publication uses one 64-slot process queue and a dedicated diagnostic logger.
Every enqueue is nonblocking. Serialization and log I/O occur on one separate
goroutine, outside financial ownership, with no shared application-log mutex.
A blocked or failed sink can lose events; `dropped`, sequence gaps, missing
final events, and `capped` require partial/unknown interpretation. The private
result keeps its bounded copy independently of publication. The publisher
stops after the plan expiry plus one minute; late joined work can be dropped.
Concurrent callbacks can acquire event ordinals or publish in a different order
from when they sampled their monotonic offsets. Consumers must not require
globally sorted offsets or log arrival; pair stages within their attempt and
retain sequence/drop information. An entered event remains observable before a held stage returns. Diagnostic
publication neither waits for the sink nor adds a financial callback.

`commit/confirmed_after_release` runs inside the existing admission cleanup
callback after the transaction owner has confirmed commit and released PG.
`commit/confirmed_tx_return` is observed only after that existing owner returns.
Neither timestamp claims to be the server's exact commit instant. A commit
acknowledges this transaction, including a no-op/busy transaction; only the
separate attempt result and durable poststate establish a financial transition.
The existing DbTiming option reports acquire/begin/commit-call/rollback-call
observations after the transaction owner unwinds. These counts include failed
calls and do not prove acknowledgement. Missing commit confirmation remains
unknown. An accounting-body return is still speculative until commit; rollback
or refusal does not become a completed settlement. Joined callback return
still does not prove a Redis write ACK. Cancellation, callback generation,
panic identity, transaction retries, and post ordering remain unchanged.

Qualification must run `^TestLegacyTargetTrace` normally and with the race
instrumenter, plus the existing legacy settlement/drain/admission controls
appropriate to the same source. The new controls use synthetic IDs only and
include actual PG held grants, body/commit refusal, accounting refusal and retry,
real blocked stream callbacks, blocked publication, and no-op replay. Native
execution and release remain separate from a source freeze.
