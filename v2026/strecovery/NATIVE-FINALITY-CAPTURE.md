# Bounded native finality capture

`strecovery capture-finality` obtains native headers and stored GRANDPA
justifications through the reviewed Subtensor chain RPC, then publishes a
private, pinned proof accepted by [`verify-finality`](NATIVE-FINALITY.md).
Capture is read-only on chain. It has no signing, submission, database or
checkpoint-approval port. No mainnet authority, deployed-node capability or
live runtime provenance is supplied by this implementation.

```text
strecovery capture-finality \
  --archive /private.example/archive.json \
  --collection /private.example/collection.json \
  --collection-sha256 sha256:<exact-file-digest> \
  --checkpoint /private.example/checkpoint.json \
  --checkpoint-sha256 sha256:<exact-file-digest> \
  --config /private.example/native-capture.json \
  --config-sha256 sha256:<exact-file-digest> \
  --capture-dir /private.example/native-capture \
  --timeout 10m
```

The directory must already exist, be owner-private and resolve through physical
path components. The command takes an exclusive nonblocking directory lock.
Every input file uses the existing private, singly linked, regular-file checks.
Pins cover exact input bytes; JSON rejects duplicate and unknown fields.

## Configuration and authority

The exact `urnetwork-native-finality-capture-config-v1` fields are:

| Field | Meaning |
| --- | --- |
| `schema` | `urnetwork-native-finality-capture-config-v1` |
| `collection_hash` | The existing collection's content seal |
| `checkpoint_hash` | The checkpoint's compact typed-JSON content digest |
| `source` | Explicit operator-selected observation label |
| `rpc_url` | Explicit owned HTTP(S) archive route, without credentials, query or fragment |
| `maximum_descendant_headers` | Nonnegative finite search distance after the original boundary; zero requests an exact-boundary certificate only |
| `retry_window_seconds` | 60–900 seconds per logical RPC read; zero selects 300 |

Select the route through the operator's independent owned-node approval process.
The tool pins that input but does not authenticate endpoint ownership or invent
an approval flag. A matching `chain_getBlockHash(0)` reply is only a genesis
consistency check. Complete checkpoint keys, consensus set ID, pending changes
and live authority state remain separately supplied trust inputs. Missing or
inconsistent checkpoint input is refused before journal or network access.
Runtime `Grandpa.CurrentSetId` and a node's reported authority list cannot
replace the independently admitted checkpoint contract.

Reports retain `unapproved_checkpoint_capture` and the verifier's
`unapproved_checkpoint_proof`. Checkpoint/genesis/runtime/finality admission,
canonical accounting, actual fees and spending authorization stay false.
Every actual gas fee stays null. The config pin, archive route and signatures
from supplied keys cannot approve their own inputs.

## Exact capture and descendant coverage

1. Replay the complete existing receipt collection and validate the checkpoint.
2. Bind the journal to the exact census and typed config, which includes both
   proof-input identities, route and search/retry bounds.
3. Walk `chain_getHeader(hash)` backward from the original collection boundary
   to the pinned checkpoint. Reconstruct complete canonical SCALE from all
   header fields and raw digest items; Blake2b-256 must equal each hash selector.
4. Walk that retained ancestry forward, discovering every scheduled GRANDPA
   change. Fetch `chain_getBlock(hash)` for each exact outgoing-set enactment
   and for the collection boundary. Keep the exact `FRNK` justification bytes.
5. If an ordinary boundary has no stored justification, discover at most the
   configured number of descendants with `chain_getBlockHash(number)` and exact
   header/block hash reads. Every successor must link to the retained path.
   Stop at the first certificate that verifies under the authenticated scheduled
   authority history. Missing outgoing enactment certificates cannot be skipped.
6. Run the complete offline v2 finality verifier, including every original
   receipt proof and the original boundary's Frontier digest, before publishing
   `proof.json`. The result contains its exact SHA256 file pin for offline use.

The v2 proof retains the collection boundary as an exact ancestor of the last
certificate. It does not recollect receipts, change the EVM/native boundary or
rewrite original signatures/history. Its `native_certified` report field names
the tip owning the next rolling authority state; `native_finalized` and
`native_state_root` retain the original boundary. V1 retains its exact-boundary
requirement. No latest-head reply or numbered lookup becomes finality authority.

## Durable partial evidence and bounds

The journal is create-only. Every published file is mode `0400`, file-synced,
atomically renamed without replacement, then directory-synced. Restart with the
same inputs preserves these objects:

| File | Retained meaning |
| --- | --- |
| `capture.json` | Original census/config association, including route and bounds |
| `request-NNNNN.json` | Exact request-body hash, synced **before** transport |
| `read-NNNNN.json` | Actual response-body bytes debited, including transient errors |
| `header-HASH.json` | Complete reconstructed SCALE header, rehashed on reuse |
| `certificate-HASH.json` | Exact stored GRANDPA bytes, verified again on reuse |
| `proof.json` | Complete offline-verified proof; absent during incomplete capture |

The request hash binds method, parameters and monotonically increasing JSON-RPC
ID; the immutable context binds its route. An interrupted reservation without a
completion consumes a full 16 MiB plus the one-byte over-limit detector. It is
never refunded on restart. Completions release only unused reserved bytes.
Across all restarts in one journal, at most 32768 requests and 128 MiB of response
bodies are admitted. Each response is capped at 16 MiB, with one additional byte
read only to detect excess. There is no reset flag. A gap or orphaned completion
refuses recovery. Unpublished temporary files remain unreferenced evidence;
the command never deletes prior evidence to recover space or budget.

The original header interval plus the selected descendant window must advance
fewer than 4096 blocks from the checkpoint. Checkpoint/path/vote ancestry share
the verifier's 4096-header, 64-certificate, 1024-authority/vote and 16 MiB encoded
proof bounds. Headers are at most 256 KiB and justifications at most 4 MiB.
The final proof file is at most 16 MiB. Directory census is bounded too.

The CLI deadline is 60 seconds to 15 minutes (default 10 minutes); the API caps
every invocation at 15 minutes even if its caller supplies no deadline. Connect
and TLS setup time out after 15 seconds; requests after 30 seconds. Transport
failures and HTTP 408/429/502/503/504 retry the identical method/selector at
one-second intervals within the pinned per-read retry window and shared limits.
Redirects, environment proxies and automatic compression are disabled.
Malformed/ambiguous replies, RPC errors and integrity contradictions are terminal.

A missing stored certificate is not cached as a positive result or evidence of
non-finality. It is refreshed on restart. Complete headers and nonempty
certificates are retained and replayed; rejected certificate bytes stay available
for diagnosis. Failed reads retain their reservations/byte debits, not raw error
bodies. Cancellation or any failure emits no success JSON and publishes no
partial `proof.json`; earlier partial evidence survives. A completed restart
replays the retained proof entirely offline and preserves its inode and pin.

## Exact node capability dependency

The [18-file source manifest](native-capture-reviewed-sources.tsv) pins Subtensor
`67dcf7f791dc495064c293f080a0702cb433e51e` and its locked SDK
`cacb4310f20c7cac83eb3ccd8ed5a5ad4212608a`. Source review establishes:

- SDK `client/service/src/builder.rs` installs the standard chain RPC.
  `chain_getHeader(hash)` serializes all native header fields, and each digest
  item serializes its complete SCALE bytes as hex. The collector reconstructs
  only this canonical reviewed header codec and verifies its hash.
- SDK `client/service/src/client/client.rs` implements `chain_getBlock(hash)`
  with stored header, **body** and justifications. Pruned bodies make the entire
  block unavailable even when a header exists. The selected archive must retain
  bodies at certificate targets as well as native headers and justifications.
- `SignedBlock.justifications` is an optional `Justifications` newtype over
  `Vec<(ConsensusEngineId, Vec<u8>)>`. Its plain Serde representation is an array
  of pairs of numeric byte arrays, including `[70,82,78,75]` for `FRNK`; it is not
  a base64 or arbitrary hex-string response. Duplicate engines, non-u8 elements,
  truncated tuples and malformed certificates refuse capture.
- Subtensor configures a 512-block justification generation period. SDK
  `environment.rs` persists periodic certificates and always requires one at
  scheduled authority enactment. Many individual finalized blocks therefore
  legitimately have no stored justification. A bounded later certificate plus
  complete ancestry closes that ordinary case without moving the collection.
- The reviewed Subtensor RPC builder and its Aura/Babe extensions do not install
  `grandpa_proveFinality`. The SDK implementation of that optional RPC can return
  a later set-ending or best certificate, and does not approve starting authority
  state. Capture does not assume it exists or use it as a fallback.

If there is no stored usable certificate within the explicit window, no block
body at its target, missing outgoing enactment evidence, an unsupported authority
transition, unavailable historical ancestry or an exhausted bound, capture stops
with partial evidence preserved. Restore/qualify that archive capability, retry
the same capture when missing data arrives, or independently select a new wider
bounded capture in a fresh directory while retaining the old evidence. A fresh
independently approved checkpoint is required across unsupported consensus
transitions; no RPC observation supplies it. A 512-block setting is not a promise
that a particular live node exposes all required historical data within 512
successors or within the command's time/byte budget.

Owned-node deployment qualification, independent checkpoint/genesis/runtime
admission, native state and account nonce proofs, runtime-qualified debit/refund
attribution, service adoption, release composition and live custody/restart
remain MG-03/PF-03 dependencies. Source compilation and deterministic fixtures
do not close those production gates.
