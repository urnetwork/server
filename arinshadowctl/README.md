This local dry run does not qualify Main coverage. It does not contact Main,
activate a policy, or publish an MMDB. The runtime hook remains off unless a
reviewed caller explicitly installs a recorder.

Provide immutable active/candidate MMDB paths and SHA256 pins, a census JSON
path and SHA256 pin, and a recent RFC3339 cutover. The census includes every
required bucket (including empty buckets) and each provider's opaque token,
live public connection keys, memberships, existing serving supply and base
quality/speed gates. Each provider also needs a `lookups` map keyed by its
connections, containing the current durable `Epoch`, `At`, `Risk`, `NonQuality`
and `Verified` facts from that same census. Every boolean is required, including
false. A connection key alone cannot join an old observer fact to a replaced
database fact. Base gates exclude the ARIN decision but retain the other native
bucket gates; `active_quality` and `active_speed` describe native membership,
not HTTP success or providers borrowed by a Quality request. Feed ephemeral
NDJSON observations on stdin with
`connection`, `address`, and `active` lookup facts. Do not save this input.
Only aggregate bucket counts reach stdout; generic errors omit input values.

The input and census each have a 32 MiB bound, records a 4096-byte bound, and
observations a capacity and 90-second source-lookup freshness bound. Receipt
time never rejuvenates a lookup. Country bucket names are lowercase two-letter
codes, plus `all`; arbitrary provider/operator labels are rejected. Incomplete or failed
lookups cannot qualify provider quality. Risk vetoes remain independent of
subscriber classification. All live connections must have fresh exact-active-
epoch lookups whose time and flags exactly match the current census. Duplicate
identities, inconsistent serving flags and unlisted bucket memberships fail.
Missing or mismatched observations increment `quality_indeterminate` and
`speed_indeterminate`; they are not counted as confirmed policy removals.
`quality_removed` compares only fully observed providers. All counts from an
incomplete census are scoped observations, never population estimates.

Use an external process deadline for stdin and source transport. The tool
checks its 90-second cutover at input and finalization but cannot interrupt an
arbitrary blocking `io.Reader`. It holds two immutable MMDB byte copies, each
bounded at 512 MiB, plus bounded connection facts. Those file bounds do not
establish an acceptable production memory or latency budget. The observer has
no startup/config activation path, and its mutex/lookup cost still needs an
explicit canary budget before installation on a serving process.

Actual rollout evidence still requires an independently attested policy-two
MMDB, source-owned runtime capture, complete current live-provider census and
bucket/base-gate evidence, source identities, and the CLASSIFICATION.md stage
and supply review. Raw addresses cannot be recovered from stored keyed hashes.
The CLI always reports `actual_main_coverage=false`.

Keep the candidate outside the active resource path. Current serving code
already consumes `arin_risk` and `arin_non_quality`; publishing policy-two MMDB
bytes changes exclusions even while `subscriber_quality_policy_version` is
unset. A shadow canary must retain active resource bytes and use the separate
candidate reader. A sample or an empty/near-empty affirmative catalog does not
authorize activation.
