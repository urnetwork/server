# Client close and prompt contract settlement (CLIENTCLOSE)

**Status, 2026-10-28.**
- **Hotfix 1, deployed 07:45–12:45Z:** the removal path, close requests, and settling at the sender's final after a receiver checkpoint. Its services run 2026.10.28-outerwerld builds on top of release 1083160450; the deploy record is under Rollout below.
- **Hotfix 2, ready to deploy:** settling unused contracts at the sender's zero final.
- **Library and SDK:** the `CloseSequences` handler is on connect main and reaches providers with the next SDK release.

## Summary

Transfer contracts reserve grant balance in Redis when they are created and release it when they settle. Most contracts did not settle until the expiry sweep, hours after their 60-minute deadline, because:

- **Unused contracts never settle on reports.** 81% of contracts past their deadline had a zero-byte final from the sender and no report from the receiver, which never saw them.
- **The receiver only checkpoints the contract it is on.** When the receiver checkpoints before the sender's final, the reports alone never settle the contract.
- **The sweep runs one payer at a time.** It closes about 30 ms per contract for a payer, so a large payer falls behind.

The reservations piled up until the hot proxy network's grants filled, which caused the Redis storms of 10-24, 10-26 and 10-27/28. Raising its grants to 256 is the last step that remains, because the grant selection reads at most 256 grants.

This design settles both kinds of contract on the report path, the moment the sender's final arrives, using the same amounts the sweep would. It also adds the owner's close mechanism:

- **A client removed through the API is closed promptly everywhere:** its transports, its proxy devices, and the contracts its peers still hold with it. Its resident ends at its idle ttl (see the forward lookup flood under Rollout).
- **The platform can ask any client to close contracts now,** with a `CloseSequences` message.

## Background

- **Lifecycle.** A source client (the sender) creates a contract with a destination (the provider, or receiver).
  - The contract carries a sealed deadline (`model.DefaultContractExpiration`, 60 minutes). The deadline only stops new debits; neither client closes a contract when it passes.
  - Each party reports `CloseContract`, either as a checkpoint or as a final.
  - The contract settles when both parties have reported a final (`contractClosesMaySettle`), or under owner decision B (2026-10-10) when a receiver report arrives after the sender's final and covers it.
  - Otherwise the expiry sweep settles the contract after its deadline, using the counts on hand: the mean of two counts, or the sole count present.
- **Reservations.**
  - A contract's escrow reserves its transfer byte count in Redis for a 24 h lease (`model/subscription_redis_admission.go`).
  - Settlement on reports leaves the grant debit to the debit worker.
  - The sweep's deadline close instead locks the payer's grant, at about 30 ms per close, so one payer's closes run one at a time (`subscription_close_turns.go`).
- **The connect library.**
  - A receive sequence that exits (120 s idle, a gap, cancel or shutdown) sends finals for the contracts it has moved past, but only a checkpoint for its current contract, because the sender may resume it.
  - A send sequence that exits (300 s idle, close or cancel) sends finals for every contract it holds, including a zero final for each contract it fetched ahead but never used.
  - There was no message that could tell a peer to close.
- **Client removal (`RemoveNetworkClient`, about 30–70 per second on main).** It wrote revocation records and nothing else:
  - the client's transports closed at the next authorization recheck, within 60 s, with close code 4005;
  - its resident stayed until its idle watcher cancelled it, 5–20 minutes later;
  - a proxy device kept serving until its 90-minute idle timeout;
  - its contracts waited for the sweep.

  Most removals are hosted window identities that a multi-client retires right after `CloseAndWait`.

## What the data showed (main, 2026-10-28)

The sample was 20,000 contracts still open about 95 minutes after creation, created within a single minute:

| Sender's report | Receiver's report | Contracts | Share | Reserved | Used |
|---|---|---|---|---|---|
| final, 0 bytes | none | 16,164 | 81% | 277 GiB | 0 |
| final, over 0 bytes | none | 487 | 2% | 5.8 GiB | 313 MiB |
| final | checkpoint | 2,892 | 14% | | |
| none | checkpoint | 293 | 1.5% | | |
| none | none | 236 | 1.2% | | |

- **Unused contracts dominate.** The client fetches contracts ahead and discards them unused. At about 17.6 MiB reserved apiece, one minute of them held 277 GiB of grant balance until the sweep reached them.
- **Grant fill.**
  - About 935 contracts were created per second, at an average of 8.6 MiB each.
  - The sweep closed about 808 per second, below the rate of creation.
  - The hot network built up 15.2M open contracts.
  - Its grants filled in about 8 h at 64 and began filling again 5 h after the step to 128.

## Owner decisions, 2026-10-28

- "The expiration time of a contract should be 60 minutes. I think the root issue is that we don't have a close message that a client can send the provider to close the sequence now, so each client waits for the provider to close on a read sequence time out. The close message could be automatically generated by server/connect when a client is removed, for any client where there exists an open contract. I think that might help cycle balance faster. We could have a general client_close_model that follows the redis notification of for example stream_model"
- "The resident should also close immediately when the client is removed". Withdrawn the same day ("Hotfix connect"), after canceling residents at removal flooded redis with forward lookups.
- "server/proxy should also close devices when the client is removed"
- Removal means API removal only: "A disconnected client will unfortunately have to be time outs."
- A sender's final that arrives after the receiver's checkpoint settles the contract immediately, with the same mean the sweep uses ("Settle immediately").
- A sender's zero-byte final, with no report from the receiver, settles the unused contract immediately ("Settle at once").

## Design

### 1. Settling on the sender's final (server only)

Both rules run in the sender's report, under the contract lock, and settle through the ordinary path. The `contractCheckpointCompletion` value from `applyLockedContractCloseReportInTx` selects them:

| On the sender's final, the receiver has | Result |
|---|---|
| A final | Settles on two finals (unchanged). |
| A checkpoint, within 16 MiB of the sender's count either way (`contractSenderFinalAfterCheckpoint`) | The checkpoint is completed as a zero final, so the receiver's count stands, and the contract settles. Payment is the mean of the two counts, as in the sweep. |
| A checkpoint that differs by more than 16 MiB | Left open for the sweep (unchanged). |
| No report, the sender used 0 bytes, and the report is identified (`contractSenderZeroFinalUnreported`) | A zero final is recorded for the receiver, the contract settles with 0, and the reservation is freed. The sweep would also settle a sender-only contract with the sender's count, here 0. |
| No report, and the sender used some bytes | Left open. The receiver is asked for its final (section 3), and the sweep remains the backstop. |

- **Shared guards:** no outcome yet, no dispute, expiry not started (`usage_unverified`), a sender count that is not a legacy lower bound, and an unscoped settlement.
- **Failure:** if settlement fails, the completed receiver final rolls back with it (`contractCoveredCheckpointAbort`).
- **Late reports:** a receiver report that arrives after either settlement gets the benign "already settled" error that late reports already get once the sweep has settled their contract.
- **Scope of the zero rule:** it applies only to identified reports (those with a `report_id`). Every current connect client sends one, including the hosted proxy windows. The legacy id-less path keeps the two-final rule, which leaves very old clients and the many legacy test fixtures unchanged.
- **Unchanged:** two finals settle, decision B still applies, and the sweep handles the rest.

### 2. `CloseSequences`, the platform's request that a client close now

- **Message:** connect message type 36, `CloseSequences { repeated bytes contract_ids = 1; repeated bytes peer_ids = 2; }`. Only the platform sends it (source `ControlId`), so it needs no contract or peer session. Older clients ignore the unknown type.
- **Handler:** the client handler (`transfer_close_sequences.go`) records the ids on the control goroutine and applies them on one worker, contracts before peers.

| The listed contract is | The client |
|---|---|
| A live receive sequence's earlier contract | Sends a final with that sequence's counts. The sequence continues. |
| A live receive sequence's current contract | Ends the sequence. Its exit sends a final, not a checkpoint. |
| Held by a live send sequence, or queued for one | Cancels that send sequence. Its exit sends the finals, and a queued contract closes with (0, 0). |
| Not held by any sequence | Sends a final of (0, 0). Its counts were reported earlier, and the server turns an earlier checkpoint into a final with the same count. |

For each listed peer, the client ends every receive sequence from that peer (finalizing their current contracts too), cancels every send sequence to it, and closes the contracts queued for it.

This is the owner's close message, with one change: the platform sends it, not the sender. When the sender's sequence ends at 300 s, the receiver's sequence has usually exited at 120 s, so a message on the sender's own sequence would find nothing to close. The sender's final close report already tells the platform the sender is done.

### 3. `client_close_model`: Redis keys and keyspace notifications

The model follows the stream hop set (`{clientId}s2_c_hops`). Main enables the keyspace event classes `Kg$sx`.

- **`{clientId}c_removed`**
  - A string with a 2 h TTL, set after an API removal commits.
  - Every connect and proxy process subscribes to `__keyspace@<db>__:{*}c_removed` and acts only on a `set` event.
- **`{clientId}c_close`**
  - A set of pending close items, `c:<contractId>` and `p:<peerId>`. Each add renews its 15-minute TTL.
  - Only the process hosting the client's resident consumes it, through a per-resident listener.
  - The listener reads on each `sadd` event, once when it registers, and once more after each resubscribe. Each read pops up to 1024 items (SPOP, so each item has exactly one taker) and sends them as one `CloseSequences`.
- **Producers:**
  - The report path adds `c:<contract>` to the receiver's set when a used sender's final commits and the receiver has not reported.
  - A removal adds its contracts (section 4).
- **Best effort:**
  - A lost write or event leaves its contract to the sweep.
  - Transports keep the 60 s authorization recheck as their backstop.
  - Residents and devices keep their idle timeouts.
  - Proxy devices also re-check their clients' flags after each resubscribe.

### 4. API removal

`deactivateLockedNetworkClientsInTx` handles every API removal path: single, batch, task, prober and session. After the transaction commits, it:

1. Sets `{X}c_removed` for each removed client X.
2. Queues a bounded background read of X's open contracts, up to 4096 in each direction, using the unresolved-pair indexes. Each counterparty P gets `c:<contract>` items plus `p:<X>` in `{P}c_close`. A full queue drops the read and leaves those contracts to the sweep.

**Each connect process** that sees X's flag:
- Closes X's transports on that process with the typed authorization close (code 4005), which is what the lease recheck would send.
- Leaves X's resident to end at its idle ttl: five minutes without activity. A transport that registers later fails its lease's final admission check.
- Hotfix 1 also canceled the resident at once. Peers still sending to X then kept a packet in a forward that retried the resident lookup every second until the forward idled out, up to 15 minutes. Kept alive, the resident takes those forwards' traffic until the peers stop.

**Each proxy process** that sees X's flag:
- Closes every device of client X, using a client → device index kept under the manager's lock.
- After each resubscribe, re-checks the flags of all its devices' clients, because a device has no prompt backstop of its own.
- Reopening a device for X fails, because `MintHostedSession` requires an active client.

### What does not change

- **Disconnected clients still rely on timeouts.** A client that disconnects without sending its finals waits for the deadline and the sweep.
- **The reservation lease stays 24 h.** A 60–75 minute lease would expire tokens that PostgreSQL still counts as `redis_reserved` on contracts the sweep has not closed yet, and settling or releasing a missing token does nothing, so the grant would over-admit. Revisit this once settlement keeps up.

## Metrics

- **Settlement:**
  - `urnetwork_contract_covered_checkpoint_total`: receiver checkpoints completed on the report path, under decision B or section 1.
  - `urnetwork_contract_zero_final_settled_total`: unused contracts settled at the sender's zero final.
  - `urnetwork_contract_closed_total` versus `urnetwork_contract_opened_total`, by service. The sweep's share, the taskworker's, should fall.
- **Close requests:**
  - `urnetwork_client_close_sequences_sent_total`
  - `urnetwork_client_close_requests_sent_total{kind="contract|peer"}`
  - `urnetwork_key_events_dispatched_client_close_total`
- **Removal:**
  - `urnetwork_key_events_dispatched_client_removed_total`
  - `urnetwork_client_removed_closes_total{kind="transport"}` (`kind="resident"` until hotfix 4)
  - `urnetwork_proxy_client_removed_{subscribes,device_closes,read_failures}_total`
- **Grant health:**
  - `urnetwork_redis_contract_reservation_total{operation="reserve-pending",result="skipped"}`: warn above 15k/s.
  - `urnetwork_connect_contract_rejections_total{cause="insufficient_balance"}`

## Rollout

**Hotfix 1.** Branch `hotfix/client-close-2026-10-28`:
- Server: release source 2fcfc0617, the client close commits, and golang.org/x/net v0.62.0, which the govulncheck gate requires for GO-2026-6860.
- Connect: f24220de plus the `CloseSequences` commits.

| Service | Version | Deployed (UTC) |
|---|---|---|
| taskworker | 1083721510 | 07:45 |
| api | 1083726780 | 07:49 |
| connect | 1083729550 | beta 07:54; g1 and g2 08:00; g3 and g4 10:09 |
| proxy | 1083735070 | blocks g1–g10, 10:13–10:59 |
| alt | 1083736580 | crisp 12:39, fireside 12:44 |

Every step passed its gates: no storm alarm, PgBouncer waiting at 0, no stuck proxy calls, no OOM kills.

At 60% connect, the removal path closed about 54 transports and 11 residents per second at removal, and sent about 138 `CloseSequences` per second.

**Hotfix 2.** The zero-final rule (section 1), built the same way.

**Main.** server `44a61ebf6` (client close, rule 1a, x/net) and connect `71d176c6`. The zero-final rule follows.

**SDK.** The connect library's `CloseSequences` handler takes effect as providers update.

**Forward lookup flood, 2026-10-28.** Canceling a removed client's resident (hotfix 1) left peers' forwards retrying the resident lookup every second.

| | Before hotfix 1 | 15:00 UTC |
|---|---|---|
| Removals per second (API about 67) | — | 390 |
| Residents canceled per second | 0 | 377 |
| Resident forward lookups per second | 3,000–5,000 | 260,000 |
| Redis GETs per second | about 75,000 | 333,000 |

- The busiest redis shards reached 100% of a core at 14:50 UTC. Contract reservation errors followed, reaching 79 per second at 15:24.
- The lookups tracked the removal rate, which climbed through the morning: each canceled resident left about one spinning forward for up to 15 minutes.
- **Hotfix 4** (connect, owner "Hotfix connect"): a removal closes the transports and keeps the resident until its idle ttl.

**Rollback.** Redeploy 2026.10.27+1083160450. The keys expire on their own, and older binaries ignore them.

## Testing

All tests run against Redis and PostgreSQL on the local test gate.

- **Model:**
  - key and event parsing, and take-once semantics;
  - the receiver request on a used sender final, but not on a retried final, a sender checkpoint, or a receiver that already sent its final;
  - settlement at the sender's final after a checkpoint, and not when the counts disagree beyond the tolerance;
  - unused contracts settled, with their reservations freed, on the identified path; the legacy id-less path keeps the two-final rule; nothing settles once expiry has started;
  - removal setting flags and asking counterparties.
  - Existing close-report, decision B, checkpoint and expiry-repair suites were updated where they relied on the old order. A test that needs an open contract with both reports now applies the reports without settling.
- **Connect:** close requests reach a live client as `CloseSequences`. A removed client's transport receives the 4005 typed close well inside the 60 s recheck, and the exchange keeps its resident (the test fails if the removal cancels it). Both tests fail without the new subscription patterns.
- **Proxy:** the device index, closes on the removal event, resubscribe re-checks, and races, all under `-race`.
- **Connect library:**
  - each row of the section 2 table, the peer case, and the non-control source case;
  - an idle exit that still checkpoints;
  - nine mutations of the handler, each caught by at least one test;
  - all under `-race`.

## Risks

- **A sender that reports zero after sending bytes leaves the provider unpaid for them.** The receiver's later report is refused. Without the rule, the sweep would settle such a contract with only the sender's count unless the receiver reported first. The senders here are mostly the platform's own hosted windows.
- **The `CloseSequences` handler has three edge cases:**
  - A close for an old contract that arrives before the first pack of its successor ends the live sequence. The existing recovery restarts it.
  - On removal, a close that arrives between a queued contract being taken and being recorded as open sends (0, 0) before the real final. The platform keeps only the first final.
  - Removing a peer waits for its sequence exits without a deadline.
- **Load:**
  - one SADD per used sender final whose receiver has not reported;
  - one SET per removed client, plus the read of its open contracts;
  - one keyspace event per write, delivered to every subscribed process.

  The connect dispatch only routes events; it never blocks.
- **A burst** that overflows the subscriber's 1024-event buffer forces a resubscribe and a resync.

## Follow-ups

- **Which removals close.** The hook fires for every deactivation: API removals (about 67 per second on 2026-10-28) and the internal prober, guest and session cleanups (most of about 390 per second). Decide whether the internal cleanups should set the flag and ask counterparties to close.

- **Fewer prefetched contracts.** Stop the client from creating contracts it never uses: the source of the 81% and of most reservation churn.
- **The sweep's per-payer serialization.**
- **Reservation lease.** Align the lease with the deadline once settlement keeps up.
- **Flaky expiry-repair test.** `TestContractExpiryRepairPreviewApplyAccounting32` failed 2 of 8 runs under heavy local load and passed every run when the machine was idle.
