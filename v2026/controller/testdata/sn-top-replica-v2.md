# Synthetic head replay fixture

`sn-top-replica-v2.json.gz` contains only generated test identities and public
evidence from the existing `sn/validator` M8 trail engine, real disk ledger,
typed stream writer, cut sealer and full replay. Its keys are synthetic public
keys; it contains no production captures, addresses or credentials.

It has two complete eight-hop trails over disjoint provider populations. The
signed egress cursor is sequence 9, so the first trail's seven non-seed prefixes
are outside the measurement. One provider in the second trail has moved to
generation 2 in the signed current census. Its historical generation-1 prefix
is excluded. The expected result is six prefixes for generation 1 and an empty
set for the still-live generation 2. The signed global score is exactly 6.

The compressed JSON contains the signed v2 summary, all typed immutable
metadata/record/proof objects, server public keys and the expected prefix sets.
Server tests publish those exact bytes through the real local blob store and
exercise complete policy-aware replay, delayed receipt, missing and corrupt
proof objects, and unknown server signing keys.

Regenerate from the sibling `sn` repository:

```sh
SN_HEAD_REPLICA_FIXTURE_UPDATE=1 go test ./validator -run '^TestHeadSummaryReplayFixture$' -count=1
```

The generator is `validator/head_summary_replay_fixture_test.go`. It writes only
this fixture at its repository-relative path; routine test runs do not rewrite
it. Generated signatures and IDs can change on regeneration, while the tested
attribution and expected counts stay fixed.
