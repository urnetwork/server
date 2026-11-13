# Evaluation and metadata ownership

The evaluator alone owns the original submission/cycle scoring deadline. Job and
host metadata RPCs use the live worker parent, including when an RPC starts just
before scoring expires. Their existing attempt/total budgets, shared call cap,
confirmed job/host expiry bounds, retry predicates and database fences remain
unchanged. A timely metadata ACK does not resume scoring or reset its clock.

Natural scoring expiry cancels the evaluator and joins that same evaluator's
cleanup. The existing detached failure-archive allowance is not restarted by a
tick. Metadata renewal can retain current ownership during that cleanup; it
cannot restore expired authority. A result already ready at each cleanup
dispatch probe is consumed before another public store call, including retries.
This is an observation boundary, not an atomic claim about later arrivals.

Parent cancellation/fencing, an actual metadata-own timeout or genuine store
failure remain fail-closed. Distributed integrity rejection stays separate; no
archive-verification bit is minted. A joined outcome past the scoring deadline
cannot succeed: terminal budget metadata preserves the immutable original-cause
artifact manifest and any actual archive-failure details. Natural expiry itself
does not emit a `heartbeat_failed` recovery producer or require an extra claim.

These rules do not retrospectively determine the cause of an earlier heartbeat
event. Returning from a remote evaluator also does not prove physical agent
cleanup beyond that evaluator's existing authenticated contract. Legacy database
commit-time semantics are unchanged; worker pre-dispatch deadline checks are not
a new atomic database-time cutoff.
