# SDK packed-prefix proof control

This isolated helper tests the proposed automatic parent-trie refill against
the pinned SDK, `cacb4310f20c7cac83eb3ccd8ed5a5ad4212608a`. It is not the
production feed, an RPC capability check, or approval of a chain/runtime.
It adds no changes to `native-storage-oracle`, its 18 layout vectors, or the
populated 4096-provider capacity example on its parent commit.

The helper constructs real populated LayoutV0 and LayoutV1 tries. A
`sp_state_machine::TrieBackend` starts with an empty proof cache. Its actual
`TrieBackendStorage::get` misses carry `(hash, packed prefix)` plus the active
top/child operation. The helper computes exactly the proposed probe key:

1. Strip the exact active child keyspace, when present.
2. Retain every complete byte and append the optional high-padded nibble.
3. Refuse nonzero low nibble bits and keys over the declared bound.
4. Generate a raw SDK Recorder proof for that key at the original parent or
   authenticated child root. Values returned while constructing the proof are
   discarded; only proof nodes can be admitted.
5. Require the reply's parent/child identity and demanded raw-node hash before
   publishing any nodes. The original SDK resumes the same operation.

The synthetic proof source is a complete in-memory trie, standing in for the
read-only proof endpoint. It does not stand in for the backend verdict:
storage values, absence, iterator end, and post-write roots all come from the
real SDK backend. RPC method availability, Go/Rust IPC, signed authority and
actual runtime execution need their own production qualification.

The 15 top-level roots cover even and odd prefixes, compressed partials,
separate value nodes, point absence, next-key gaps/end, raw iteration across
missing branches, untouched-sibling compaction, two child keyspaces with the
same relative keys and different values, child deletion, and root updates.
Every child scope first obtains the child root through a top-level SDK read.
The fallback controls reach SDK unchanged-parent and default-child tuples
after missing proof; a separate sticky error must still refuse output.
Complete no-op, same-value and empty-child operations remain positive cases.
Per-request transcripts retain the original prefix, proposed key, demanded
hash, proof count/bytes and admission result. A missing demanded hash is an
explicit hypothesis counterexample and must not be waived or reclassified.

The cache bounds are 512 requests, 4096 distinct nodes and 4 MiB of distinct
raw data. No network, external input, sleeps, clocks, or credentials are used.
These small fixtures do not establish the populated-provider byte ceiling;
that remains the separate parent capacity example.

## Dependency and execution boundary

The relative `polkadot-sdk` path must resolve to an authenticated exact SDK
checkout at the revision above. The new lockfile retains the legacy oracle's
locked versions and adds nine exact dependency blocks from the existing
runtime-metadata-probe graph for `sp-state-machine` and its backtrace closure.
It was assembled and checked for unambiguous package edges without running
Cargo. `cargo metadata --locked --offline` is still a required first phase;
any lock disagreement is a preparation failure, not a test result.

Compilation and execution belong to the independent qualifier after explicit
finite resource admission. The planned positive command is:

```text
cargo test --locked --offline --manifest-path tools/native-prefix-probe/Cargo.toml --lib tests::sdk_prefix_ -- --nocapture
```

The qualifier must observe exactly all 15 declared roots, not merely exit 0.
It must bind source bytes, SDK revision, dependency graph, actual compiled
test ELF and raw transcripts. Operative copied-source controls need a fresh
package compile and distinct artifact readback; sharing a warm target does
not itself prove copied source was rebuilt. Preserve warm dependencies and
existing receipt-bound executables. No compilation or behavioral execution
is claimed by this source-only handoff.
