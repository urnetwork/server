# Job heartbeat owner diagnostics

The optional `heartbeat_owner` handback field is diagnostic-only. It is attached
only after the job heartbeat retry operation returns an error and the worker
has joined evaluator cleanup. Host/supervisor operations retain their original
nil-observer path. SQL, retry predicates, settings, timeouts, call caps, leases,
and evaluation clocks are unchanged. Predicates see original store errors;
only final handback wraps them, preserving error text, `Is`/`As` causes and
`error_sha256`. The complete new event payload truthfully hashes its added
metadata; historical payloads are not rewritten.

Schema1 boundary values identify the actual selected return branch:

| Value | Boundary |
| --- | --- |
| 0 | Unknown/not returned |
| 1 | Parent stopped before dispatch |
| 2 | Confirmed authority expired before dispatch |
| 3 | Total budget stopped before dispatch |
| 4 | Original lock budget stopped before dispatch |
| 5 | Attempt refused before store dispatch |
| 6 | Store error returned without another attempt |
| 7 | Parent stopped after store return |
| 8 | Confirmed authority expired after store return |
| 9 | Store returned nil, owner refused late attempt acknowledgement |
| 10 | Total budget stopped after store error |
| 11 | Lock budget stopped after store error |
| 12 | Retry wait returned an error |
| 13 | Parent stopped during retry wait |
| 14 | Timely successful owner return (not emitted as a failure) |

`last_store_outcome` is0 not called,1 pending,2 returned nil,3 returned error.
`store_calls` is cumulative within this one owner invocation. Attempt entry and
return budgets belong to the **last dispatched store call**, not a later
dispatch refused before reaching the store. Parent/total return budgets are
sampled before deferred owner cancellation. Budgets are concurrent observations;
the enum names the selected branch, not a decision inferred from those samples.

`completed_store_database` is optional and separate from the existing
`heartbeat_database` error-proof field. A nil store return does not itself prove
COMMIT, and absent database metadata never proves no COMMIT. Captures are copied
only after store unwind, bounded by the source step/ordinal array, and omitted
if strings/shape are unknown. If capture count exceeds one, only the last
snapshot is retained: it is not a complete public-call or transaction graph.
No snapshot, category, enum or persisted event grants retry or lease authority.

Slices, return-budget pointers and optional pool-return statistics are detached
on capture and projection; late publication after sink closure is ignored.
No SQL, arguments, error text/object, identifiers, token, or context values enter
the owner fields. A pool's `Idle` release observation is not proof of physical
reinsertion or destructor completion: ordinary maximum-lifetime disposal can
also occur after an acknowledged rollback. This field never upgrades that
observation into stronger custody or replay authority.
