This local dry run does not qualify Main coverage. It does not contact Main,
activate a policy, or publish an MMDB. The runtime hook remains off unless a
reviewed caller explicitly installs a recorder.

Provide immutable active/candidate MMDB paths and SHA256 pins, a census JSON
path and SHA256 pin, and a recent RFC3339 cutover. The census includes every
required bucket (including empty buckets) and each provider's opaque token,
live public connection keys, memberships, existing serving supply and base
quality/speed gates. Feed ephemeral NDJSON observations on stdin with
`connection`, `address`, and `active` lookup facts. Do not save this input.
Only aggregate bucket counts reach stdout; generic errors omit input values.

The input and census each have a 32 MiB bound, records a 4096-byte bound, and
observations a capacity and 90-second freshness bound. Incomplete or failed
lookups cannot qualify provider quality. Risk vetoes remain independent of
subscriber classification. All live connections must have fresh exact-active-
epoch lookups. Duplicate identities and unlisted bucket memberships fail.

Actual rollout evidence still requires an independently attested policy-two
MMDB, source-owned runtime capture, complete current live-provider census and
bucket/base-gate evidence, source identities, and the CLASSIFICATION.md stage
and supply review. Raw addresses cannot be recovered from stored keyed hashes.
The CLI always reports `actual_main_coverage=false`.
