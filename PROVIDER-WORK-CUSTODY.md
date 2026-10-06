# Signed provider work custody

Migration 771 retains canonical SDK boundary requests, cuts, independently signed
complete owner rosters, and exact artifact companions. These are immutable original
inputs, not a global census inferred from the set of rows currently in SQL.

Provision `provider_work.yml` in the API and payout worker's independently owned
vault configuration:

```yaml
schema: urnetwork-provider-work-custody-v1
domain_hash: <64 lower-case hexadecimal characters>
request_public_key: <independently approved Ed25519 public key, 64 hex characters>
authority_signer: <independently approved whole-work roster EVM signer>
client_key_root_signer: <independently approved client-key registration EVM signer>
```

The domain is the exact `ClientKeyHistoryDomain.Digest()` used by the provider
launch. The roster signer must differ from the payout artifact publisher. The
Server receives no new private signing key through this configuration. Absent
configuration keeps historical whole-work evidence unknown; malformed or unreadable
configured authority is an error, never permission to select a body key.

The independent owner signs canonical `protocol.OriginalWorkRequest` bytes for
each expected SDK generation and each start/end boundary, then posts them to
`POST /provider-work/v1/requests`. Capture permission lasts at most one hour.
Only exact retries may reenter an already retained boundary. Public SDK polling is
`GET /provider-work/v1/requests?domain=<hex32>&client=<hex16>&generation=<hex16>&key=<hex32>`.
At most eight outstanding requests are admitted for an exact SDK identity.

The SDK's owned worker posts `OriginalWorkCutSubmission` JSON to
`POST /provider-work/v1/cuts`; the request must already exist. The response is
`OriginalWorkCutReceipt` with both original SHA-256 hashes. A lost response may
be retried after capture permission expires. Changing a cut or changing the request
id for a retained domain/client/generation/epoch/kind cannot reset custody.
Original canonical bytes remain available from `GET /provider-work/v1/requests/<hex32>`
and `GET /provider-work/v1/cuts/<hex32>`.

The separately approved complete SDK roster is canonical root-signed
`payoutartifact.WholeWorkAuthority`, posted to `POST /provider-work/v1/authorities`.
There is one immutable original for each domain and epoch. It explicitly names
every SDK owner, its network, generation and key; `PriorContracts` is an explicit
independently reconciled prior-work checkpoint, and is an empty non-null list for
the first prospective window. The production Frontier clock profile is
`frontier-legacy-rlp15-milliseconds`. An operator cannot construct a roster by
enumerating its database and re-signing the observed set.

## Production roster signer

The epoch roster producer lives in the **SN repository**, at
`cli/payoutroster` with reusable implementation in `payoutroster`. Its operator
handoff is [SN's payout roster guide](../sn/mainnet/PAYOUT-ROSTER.md).

Run it under the independently approved roster authority's custody, separately
from the payout artifact worker. Its `vault/main/payout_roster.yml` configuration
names a dedicated private key file and pins the public authority, artifact and
client-key registration signers plus the exact deployment domain. The Server's
`provider_work.yml` keeps only the matching approved public inputs; it does not
receive the roster private key.

The producer prepares a canonical request from an explicitly complete owner and
provider inventory with original enrollment, registration and wallet-consent
histories. When network-consent heads are present, it emits roster v2 with
`network_wallets`. It retains unmapped providers rather than omitting them, and
uses the shared consent rules without changing install-consent precedence.
Observed usage or a SQL owner enumeration is not a complete population.

Use `prepare` for review, `sign` for local retained signing, `once` for retained
signing and publication, or `run` for a reviewed request inbox. The producer
retains the reviewed request and exact signed roster before posting to
`POST /provider-work/v1/authorities`; transient failures retry the identical
bytes under a default 300-second operation budget. A verified digest receipt is
retained before completed queue work is retired. Preserve that state on restart;
never produce a replacement roster for an already retained domain and epoch.
See the guide for exact flags, file formats, queue retirement and recovery.

Payout production reads its separate window in the same SQL statement as credited
usage, including a zero-credit window. Before signing, it joins every approved
owner's exact start/end request and cut. It retains exact committed Frontier RLP15
headers recovered from the original public projection; rendered hash echoes or a
runtime base fee do not replace the committed preimage. Full semantic verification
runs on the signed artifact before publication. Missing original roster, cuts,
window or header evidence remains unknown; returned contradictory evidence is
refused.

After publication, the original component is available from
`GET /provider-work/v1/windows?domain=<hex32>&epoch=<uint64>&artifact=<hex32>&authority=<hex32>`.
The final authority selector is optional; if supplied it must match exactly.
The artifact selector is mandatory. A historical artifact with no retained complete
component returns 404 instead of being reconstructed from current SQL. Receipt
and component retrieval grant no independent native-finality or provider-eligibility
authority.

All public bodies use one canonical JSON encoding, fixed byte limits and four
active operations per API route-set owner. Body acquisition has a 60-second
deadline; each complete operation has a 300-second owner. Cancellation joins the
original body close. Unavailable I/O returns 503, capacity returns 429, unknown
originals return 404, and actual immutable-byte contradictions return 409. The SDK
retains its original outbox across transient errors; the Server never requests a
fresh SDK signature to recover an uncertain receipt.
# SDK startup enrollment

Prospective payout production loads the independently signed whole-work
authority once, before querying provider reliability, wallets or fleet bindings.
Its explicit `expected_providers` roster adds zero-usage rows for idle providers;
observed usage outside that roster or under a different network is refused.
Zero rows do not create exposure, eligibility, a wallet or a payout leaf.

When that prospective authority is present, missing original cuts, window clock
or predecessor evidence leaves payout production pending before publication.
The later delivery of the same originals can finish the same payout. An already
published historical artifact still uses its original immutable retry path.

Prior-contract exclusions require original predecessor artifacts from the same
domain and their exact whole-work witnesses. Every dependency is independently
verified again under the configured root authorities. The new authority's prior
entries are lookup hints and must match the resulting original checkpoints in
every field. The operation traverses at most 64 distinct prior windows and
64 MiB of original artifact/witness bytes; exceeding either bound returns a
capacity refusal, without accepting a partial predecessor graph. Missing
predecessors remain unknown. This acquisition runs for both payout publication
and public companion retrieval; no current SQL row supplies a verified verdict.

The SDK sends its exact canonical `OriginalWorkOwnerEnrollment` to public
`POST /provider-work/v1/owners` before polling capture requests. The 4 KiB signed
statement binds domain, client, SDK generation and public key. Its receipt is
`OriginalWorkOwnerReceipt`, containing the SHA-256 hash of those exact bytes.

First admission requires the existing authenticated client and its current,
independently signed client-key registration under `client_key_root_signer` in
`provider_work.yml`. A missing registration returns 503 so normal asynchronous
key registration can finish. A foreign key or contradictory registration is
refused. No private key is accepted by this route. There is a lifetime allowance
of 1,024 retained SDK generations per client across domains; a new policy,
request, signature or process does not renew it. Exact retained retries remain
valid after key rotation or client cleanup, with identical receipts. The
registration bytes used for first admission are retained alongside the SDK
statement. Migration 774 makes those rows append-only, including TRUNCATE.

`GET /provider-work/v1/owners?domain=<lowerhex32>&client=<lowerhex16>&key=<lowerhex32>`
returns `urnetwork-sdk-whole-work-owner-index-v1` with `owners`, an array of
original signed statement bytes. The index is bounded by the lifetime allowance
and is never truncated. Adding `generation=<lowerhex16>` selects one exact raw
statement, or 404 when missing. All selectors must occur exactly once, with no
other query parameters. The shared four-operation route owner and 300-second
operation deadline apply; uploads retain the native 60-second read deadline.

This index proves possession and retained admission for each listed tuple. It
does not identify a current generation or assert a complete SDK population. The
independent request approver and signed whole-work authority must explicitly
choose each original tuple. Missing historical enrollment remains unknown.
