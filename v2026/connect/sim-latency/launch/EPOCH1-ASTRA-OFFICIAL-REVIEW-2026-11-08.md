# Official epoch-1 winner review

Astra max approved elected submission
`01a1a440-fd2e-538a-365c-f4b23eed4739` at 2026-11-08 22:06:58 UTC.
The operator explicitly selected this reviewer for epoch 1 and conditionally
approved the winner if the review approved it. That condition is satisfied.
All six criteria pass; traffic preservation and production benefit pass.

The authenticated patch changes 19 existing `connect` files. Its principal
mechanisms are bounded provider-return contention scheduling, less redundant
replay, physical-frame coalescing, capped cold-connection parallelism, and
desynchronized discovery refresh. It changes no frozen scoring instrument.
The reviewed metric is failure-penalized p95 full-request duration, not merely
the FindProviders diagnostic. The recorded median is 7,062.416 ms versus
19,556.1126 ms baseline, approximately 63.9% lower; this review did not rerun
the benchmark or turn the unaccepted calibration into accepted calibration.

Independent source reconstruction verified the submitted preimages and
postimages against the frozen checkpoint. The held scheduler path preserves
the already-read bytes and continues the inner buffer loop before assigning
sequence numbers. No deliberate traffic suppression, benchmark detection,
forged measurement, protected-instrument tampering, or credential escape was
identified. SDK/provider callers establish a real production path.

Non-blocking advisories remain explicit: the 256 KiB flight/lead limit can
limit high-bandwidth/high-RTT throughput; bounded bulk-flow yielding is a QoS
tradeoff, not a claim of universal per-flow improvement; lowering an already
grown framer limit does not immediately shrink existing storage, but the added
production caller configures it once before starting its writer. These were
not retroactively converted into epoch-1 disqualification criteria.

The ten partial Qwen context uncertainties are preserved with the original
observations. Astra's independent full-patch context addresses those questions;
the interrupted Qwen/Gemma review is not declared complete or approved.

Exact verdict/findings/authorization digests, protected versions, retention,
and pending control-plane work are in
`EPOCH1-ASTRA-OFFICIAL-APPROVAL-2026-11-08.json`. The official importer uses a
separate exact operator-configured grant; it must not masquerade as native
Snow custody, renew an expired owner, or rewrite any score or election.
Until import and release are verified, do not describe the API as finalized,
rewards as released, source as promoted, or epoch 2 as open.
