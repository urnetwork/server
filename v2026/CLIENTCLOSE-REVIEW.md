# CLIENTCLOSE design and implementation review

2026-10-28.

The original review confirmed five findings: two P1 issues affecting accounting or transport availability, and three P2 issues affecting settlement recovery or prompt closure. All five now have implementation fixes and permanent deterministic regression coverage in the worktrees described under **Fix follow-up**. The original findings and reproduction evidence remain below.

## Scope

Reviewed the merged implementation and `CLIENTCLOSE.md` at:

| Repository | Revision |
| --- | --- |
| server | `31d9afc89d9dbb04ada44b21bd6dc243ace8a504` (`origin/main`) |
| connect library | `cbc51fa941421023a3f3bb5b48018de5f0131a0e` (`origin/main`) |
| SDK dependency used for validation | `f1e24fc3cd2b2ad87ed9ce67395f8e3f12492f9c` |

The server feature commits inspected were `3dfe257ed`, `6f6baae84`, `6261dda99`, `ac974123d`, `f5df741d2`, and `515fa766f`.

At review time, the local server checkout was 79 commits behind `origin/main` and contained unrelated edits. The initial review used isolated snapshots under `/tmp/clientclose-review-i6kxq9vb`. Source links below identify the committed revisions inspected. The subsequent fixes use separate Git worktrees; their locations and validation are recorded below.

## 1. [P1] A close request during sequence teardown can erase reported usage

**Location:** connect library [transfer_close_sequences.go:174–200](https://github.com/bringyour/connect/blob/cbc51fa941421023a3f3bb5b48018de5f0131a0e/transfer_close_sequences.go#L174-L200).

`closeContracts` emits a zero final when it cannot find a holder willing to handle the close. Both receive and send teardown can appear unheld before their real usage reports have been emitted:

- Receive teardown calls `takeCloseRequests`, which sets `closeRequestsTaken`, before reporting its contracts. A concurrent `requestCloseContract` then returns false, sending the caller down the zero-final fallback.
- Send teardown removes the contract from `ContractOpenKeys` inside `CloseContractWithCheckpoint` before serializing and admitting its counted final. A concurrent close also takes the zero-final fallback.

The server stops accumulating a party's usage after its first final. If the generated zero arrives first, the actual checkpoint or final is refused. This can reduce provider payment and recorded usage. It occurs during ordinary teardown, beyond the queue-take race already listed in `CLIENTCLOSE.md`.

**Deterministic evidence:** existing report hooks were used to pause the real report before serialization, inject a control-sourced `CloseSequences`, and then release the report. Under `-race`, the observed order was:

| Sequence | Reports emitted in order |
| --- | --- |
| Receive | Zero final; then a 24-byte checkpoint |
| Send | Zero final; then a 20-byte final |

The send test acknowledges the 20 bytes through the real `SendSequence.Ack` path. No race-detector warning is necessary for this ordering defect.

**Required change:** retain report ownership through teardown and order each contract's counted reports before a synthesized final. A close request must not interpret an in-progress report as an unused contract. Cover both receive and send exits, including overlapping close requests.

## 2. [P1] A full close batch exceeds the transport limit

**Location:** server [connect/client_close.go:94–106](https://github.com/bringyour/bringyour/blob/31d9afc89d9dbb04ada44b21bd6dc243ace8a504/connect/client_close.go#L94-L106).

The listener pops up to 1,024 items and puts all of them in one `CloseSequences` frame. The resulting payload alone is 18,432 bytes. The default client H1 limit and server exchange framer limit are 16,384 bytes.

This can occur when a provider reconnects with accumulated requests, or when removal queues many contracts for one counterparty. `Client.Send` preserves the single application frame; there is no fragmentation of the list to fit the carrier. The framer's split-write optimization does not bypass its total-message limit.

**Deterministic evidence:** a test populated the real Redis pending set with 1,024 contracts, ran `clientCloseListener.deliver`, wrapped its output as a control `Pack`/`TransferFrame`, and passed it through the default framer:

```text
requests=1024 inner_bytes=18432 carrier_bytes=18526
Max message len exceeded (16384<18526)
```

The exchange write path rejects the carrier and closes its connection; a local H1 client also rejects an oversized message and ends the transport. The reliable control sender can retry the same oversized message, causing repeated failures and delaying subsequent control traffic. The requests have already been removed with `SPOP`.

**Required change:** bound the encoded carrier size and split requests across frames. A conservative smaller item limit, such as 512, would fit the current limits; size-based enforcement should account for protocol envelopes. Add coverage for a full pending batch and multiple batches.

## 3. [P2] Exact retries lose the new settlement decisions

**Location:** server [model/contract_close_report.go:96–101](https://github.com/bringyour/bringyour/blob/31d9afc89d9dbb04ada44b21bd6dc243ace8a504/model/contract_close_report.go#L96-L101), with the new decisions produced in [model/subscription_model.go:3141–3146](https://github.com/bringyour/bringyour/blob/31d9afc89d9dbb04ada44b21bd6dc243ace8a504/model/subscription_model.go#L3141-L3146).

Report admission commits separately from settlement. The new sender-final decision survives only in the in-memory `admission.completion` value. If the process exits, the request is canceled, or settlement fails after admission, an exact retry finds retained evidence and returns an empty admission. Settlement then receives `contractCheckpointNoCompletion`.

The retry is acknowledged successfully, but neither new case resumes settlement:

- An identified sender zero final with no receiver report.
- A sender final following a receiver checkpoint.

There may be no further report to trigger another attempt, leaving the reservation held until expiry.

**Deterministic evidence:** the reproduction commits the production admission function, omits settlement to simulate interruption at that boundary, and retries the same report through `CloseContractWithReport`. Both cases produce:

```text
outcome=<nil> debit_journals=0 reserved=40
```

The control test using ordinary Redis admission, uninterrupted zero-final settlement, and the debit worker passes and releases the reservation.

**Required change:** persist completion eligibility or reconstruct it from sufficient durable evidence. The zero-final case can be rechecked under the contract lock. The checkpoint case must preserve the ordering distinction because the existing receiver-after-sender rule has stricter count requirements. Add interruption and settlement-failure retry tests for both new rules.

## 4. [P2] Recovery scans run before Redis subscriptions are ready

**Location:** server [proxy/proxy_client_removed.go:186–191](https://github.com/bringyour/bringyour/blob/31d9afc89d9dbb04ada44b21bd6dc243ace8a504/proxy/proxy_client_removed.go#L186-L191). The close-request resync has the same dependency at [connect/key_event_subscriber.go:430–434](https://github.com/bringyour/bringyour/blob/31d9afc89d9dbb04ada44b21bd6dc243ace8a504/connect/key_event_subscriber.go#L430-L434).

`SubscribeKeyEvents` returns before Redis acknowledges `PSUBSCRIBE`. The new consumers immediately perform their recovery reads. A removal can therefore happen after a device's flag was checked but before its subscription becomes active. The flag is present, but the event is missed and no further scan occurs while the subscription remains healthy.

A removed proxy device can continue serving until another recovery scan or its idle timeout. Close requests can similarly remain pending until another kick or expire. This gap exists during healthy startup or resubscription; no connection failure or overflowing buffer is required.

**Deterministic evidence:**

- A synthetic Redis connection holds `PSUBSCRIBE` before execution; the actual subscription helper nevertheless returns success.
- A proxy test places removal between the recovery read and subscription activation. A later event successfully closes a second device, while the first remains live after ten minutes of virtual time. Its flag-read count remains two: install and initial recovery.

**Required change:** wait for subscription acknowledgments from every subscribed master before starting recovery reads, or trigger the reads on confirmed readiness. Test a removal and a queued close in this interval.

## 5. [P2] A contract close does not wake a receiver blocked on delivery capacity

**Location:** connect library [transfer.go:16276–16299](https://github.com/bringyour/connect/blob/cbc51fa941421023a3f3bb5b48018de5f0131a0e/transfer.go#L16276-L16299), interacting with [transfer_receive_delivery_dispatch.go:283–292](https://github.com/bringyour/connect/blob/cbc51fa941421023a3f3bb5b48018de5f0131a0e/transfer_receive_delivery_dispatch.go#L283-L292).

The new ordinary receive-loop select handles `closeNotify`. When delivery is pending or blocked, however, the loop instead enters `waitDelivery`. That function's dynamic select watches cancellation, incoming packs, the timeout, and delivery-capacity notifications, but excludes `closeNotify`.

A contract close accepted by `requestCloseContract` can therefore remain pending until downstream capacity becomes available, another pack arrives, or the idle/gap timer expires. This defeats prompt contract closure on the backpressure path.

**Deterministic evidence:** a real signed-contract TCP pack acquires a downstream delivery receipt waiting on capacity. A control-sourced close for its current contract produces no close report while the receiver remains in that wait. The test fails under `-race` without advancing the idle timer.

**Required change:** make the pending-delivery wait observe close notifications and apply the request on the sequence goroutine, preserving its accounting and cleanup rules. Cover both a current-contract close and a close of an earlier held contract during backpressure.

## Design assessment

The per-contract locking, rollback of synthesized receiver finals on settlement failure, and bounded removal queue are appropriate foundations. The ordinary zero-final path was verified with Redis admission and debit-worker release. The five findings above concern boundaries that the current tests do not cover.

Two design limitations should remain explicit:

- **Zero acknowledged bytes do not prove a contract was unused.** An honest sender can have sent bytes whose acknowledgments were lost; `UnackedByteCount` does not affect the new eligibility rule. The owner explicitly accepted immediate zero-final settlement and its payment risk. Describe this policy accurately when interpreting the “unused” metric.
- **Removal alone does not guarantee immediate settlement.** The implementation asks counterparties for finals, but a contract whose removed party never committed its own final still needs expiry. The hosted-window `CloseAndWait` ordering makes the common case more favorable, but the general removal guarantee should distinguish resource shutdown from settlement.

The documented best-effort notification losses, bounded contract scan, and zero-final payment policy were treated as accepted design choices, not additional implementation findings.

## Validation and retained reproductions

All confirmed reproductions were run with `-race`. Their failures are deterministic assertions exposing the findings; no race-detector warning was reported in those runs.

| Finding | Reproduction tests |
| --- | --- |
| 1 | `TestReviewLibraryCloseDuringReceiveExitPreservesReportOrder`; `TestReviewLibraryCloseDuringSendExitPreservesReportOrder` |
| 2 | `TestReviewClientCloseFullBatchFitsTransport` |
| 3 | `TestReviewSettlementCommittedFinalRetry`, both subcases |
| 4 | `TestReviewRemovalSubscribeReturnsBeforeReady`; `TestReviewRemovalAfterRecoveryReadBeforeSubscribeReady` |
| 5 | `TestReviewLibraryContractCloseWakesPendingDelivery` |

Passing focused checks included the five existing client-close model tests, the existing proxy removal tests, the existing library `TestCloseSequences*` tests and receive-exit close-request test, and the added `TestReviewSettlementZeroFinalRedisReservation` control.

The original targeted integration run was not green: `TestExchangeClosesRemovedClient` failed with `clients a and b are not both connected`, before exercising removal. This was not established as a CLIENTCLOSE regression. The fixture and subsequent integration results are addressed under **Fix follow-up**.

Reproduction sources and logs remain in the disposable review snapshot:

```text
/tmp/clientclose-review-i6kxq9vb/
  connect/review_library_close_exit_test.go
  server/connect/review_close_batch_test.go
  server/model/review_settlement_retry_test.go
  server/review_removal_subscribe_ready_test.go
  server/proxy/review_removal_subscribe_gap_test.go
  run-review-tests.sh
  library-review-results.log
  model-review-results.jsonl
  batch-review-results.jsonl
  subscription-review-results.jsonl
```

The temporary tests intentionally fail against the reviewed implementation. Permanent regressions now accompany the fixes in the worktrees below.

## Fix follow-up

Implementation follows `connect/CODESTYLE.md`. The fixes were prepared on `fix/clientclose-review-20261028` in these isolated worktrees, then merged with current main on `integrate/clientclose-review-20261028`:

| Repository | Worktree | Initial fix base revision |
| --- | --- | --- |
| server | `/Users/brien/urnetwork/temp/clientclose-fixes-20261028/server` | `b98cd928fa4e318cfdd4ad3211eb7615f66bd1f9` |
| connect library | `/Users/brien/urnetwork/temp/clientclose-fixes-20261028/connect` | `cbc51fa941421023a3f3bb5b48018de5f0131a0e` |

The initial implementation left the original checkouts untouched apart from this report. Integration incorporates the concurrent main-branch resident hotfix `aa9559c08`, transport-delay changes, revised settlement coverage, and the legacy client zero-final policy through `c20f2bc6f`. Removed clients' residents still live until their idle timeout to prevent repeated forward lookups. Legacy client reports now retain zero-final eligibility, while internal `CloseContract` calls retain the two-final rule. Validation uses the clean SDK revision recorded in the original scope.

| Finding | Implemented change | Permanent regression coverage |
| --- | --- | --- |
| 1: teardown report ordering | Receive sequences keep ownership until exit reports reach the manager. The manager registers counted ownership before releasing send indexes and delays a coalesced synthetic zero final until outstanding counted reports succeed. Queue removal and open registration also share one lock, closing the documented handoff gap. | `transfer_close_sequences_race_test.go` and `transfer_contract_close_reports_test.go`: receive/send exits, peer closes, queued handoff, delayed ACKs, duplicate requests, cleanup failure, callback-before-return and joined shutdown. |
| 2: oversized close batches | `ExchangeSettings.ClientCloseBatchSize` defaults to 512. Encoded legacy carrier sizing includes maximum sequence and RTT fields, limits each batch, and drains the remaining Redis backlog. Configured carriers below 4 KiB leave requests pending because the shared sender may coalesce other control frames into a larger carrier. | `connect/client_close_batch_test.go` and `connect/client_close_carrier_test.go`: mixed backlog larger than 1,024 requests, larger configured batches, 4 KiB framing, both wire encodings, and preservation of requests with an unsupported 1 KiB carrier. |
| 3: lost settlement eligibility | Admission persists its completion decision and, for sender-after-checkpoint completion, the receiver count that supports it. Fresh settlement and exact retries reread that proof under the contract lock. A later receiver decision takes precedence; a changed receiver count invalidates the older sender decision. Two actual finals retain precedence over a stale zero-final decision. | `TestContractCompletionRetry*` cases cover interruption, rollback, reservation release, signed and legacy client reports, internal legacy refusal, rolling writers, stricter receiver-after-sender guards, current/zero receiver increments, racing admission, missing witnesses and two-final precedence. |
| 4: subscription readiness gap | Subscription startup waits for every pattern on every Redis master. Early messages remain buffered; startup error, timeout, cancellation, overflow or reconnect invalidates the generation. Recovery scans therefore start only after readiness. | Eleven `TestRedisKeyEventSubscription*` tests use real go-redis parsing over controlled connections. Real Redis verifies first-write delivery and reconnect; proxy tests verify removal and recovery. |
| 5: blocked receiver ignores close | The pending-delivery select observes `closeNotify` and applies requests on the receive-sequence goroutine. Closing the current contract exits; closing an earlier held contract preserves the current sequence and independent control delivery. | `TestCloseSequencesWakesPendingDelivery` and `TestCloseSequencesWakesPendingDeliveryForPreviousContract` exercise signed contracts and real pending TCP delivery receipts. |

The exchange-removal fixture now uses a real client to acknowledge control traffic and keep its resident alive, plus a raw socket on the same client instance to observe the typed authorization close. Previously the raw-only client could lose its resident during setup because it did not send control pings. This change repairs the fixture without changing production lifetimes.

Integration exposed another stale-decision case within finding 3: after a receiver checkpoint of 9 and an interrupted sender final of 11, a later receiver checkpoint bringing its count to 10 must apply the stricter receiver-after-sender guard. The old retry reused the sender's decision and settled. Deterministic regressions also reproduced this on a fresh settlement attempt and after an older writer changed the count. The retained receiver-count witness and receiver-decision precedence fix all three. A legacy client interruption test separately failed before its eligibility was threaded into durable admission.

The connect integration also adds an explicit ACK-completion barrier to the existing unheld-contract test. This keeps that fixture independent of worker scheduling after main's transport changes.

An unchanged controller fixture also assumed exactly two native deliveries despite real SQL work and 150 ms ACK timeouts. Its broader-run failure did not record the delivery count, and an instrumented isolated rerun passed. Withholding ACKs through three controller deliveries then deterministically failed the old assertion with `committed=3`. The repaired fixture counts close frames, waits for the required commits, and joins native clients before its final accounting checks. It permits additional valid retries while preserving two independent logical reports in the no-loss case and every exact ledger and metric assertion.

### Validation of the fixes

Every fix has deterministic root-cause coverage. Database and Redis integration runs use the shared `network-intensive-suite-lock.sh run-all` gate, with fixture resets serialized. All completed suites below use `-race` and `-count=1` unless noted.

| Check | Result | Retained log |
| --- | --- | --- |
| Library close, report ownership, queue lifecycle, shutdown and receive delivery suites, including ten new tests | Passed, 8.117 s | `/tmp/clientclose-review-i6kxq9vb/library-fix-results.log` |
| Eleven controlled Redis subscription startup tests | Passed, 3.947 s | `/tmp/clientclose-redis-startup-results.log` (captured command and result) |
| Original settlement fixes: nineteen selected tests, followed by the additional two-final guard case | Passed, 161.224 s plus the guard | `/tmp/clientclose-settlement-final-race.log`, `/tmp/clientclose-settlement-final-guard.log` |
| Broader settlement coverage before the final lock consolidation: 57 selected tests | Passed, 512.803 s; final consolidation covered by the runs above | `/tmp/clientclose-settlement-race.log` |
| Real Redis first-write/reconnect and three proxy removal/recovery cases | Passed, 5.782 s and 6.795 s | `/tmp/clientclose-subscribe-fixes.log` |
| Final exchange delivery/removal and four close-carrier cases after incorporating the resident hotfix | Passed, 101.489 s | `/tmp/clientclose-exchange-final.log` |
| Connect main integration: focused close, ownership, lifecycle and framer coverage | Passed, 5.633 s; the repaired ACK-barrier fixture also passed twenty race repetitions | `/tmp/clientclose-library-main-integration.log` |
| Server main integration: exchange delivery/removal, four close-carrier cases and eight transport-delay cases | Passed, 136.924 s | `/tmp/clientclose-server-main-integration.log` |
| Final receiver-witness guards: five cases | Passed, 29.850 s, without `-race`; subsequently completed under `-race` in the partial run below | `/tmp/clientclose-settlement-witness-green.log` |
| Legacy client integration: interruption/retry, internal-final refusal and updated sender-zero-final case | Passed, 51.979 s, without `-race`; the two new retry cases subsequently completed under `-race` | `/tmp/clientclose-legacy-client-green.log` |
| Native checkpoint/controller fixture after deterministic retry repair | Passed, 17.276 s | `/tmp/clientclose-native-checkpoint-green.log` |

The final broader model race run selected 39 tests and timed out after 1082.119 seconds while `TestContractCloseReportSignedAdmissionRoundTrips` was still applying migrations in fixture setup, before its body. The 27 preceding selected tests completed without assertion failures or race reports: all 21 `TestContractCompletionRetry*` cases, four covered-checkpoint guards, the rolling-writer aggregate guard and admission round trips. Registration order and the timeout stack establish that coverage; the remaining historical cases were not completed. This is an incomplete broader run, not a passing model package. Evidence is retained in `/tmp/clientclose-settlement-main-final-race.log` and `/tmp/clientclose-model-final-test-order.txt`.

The same command subsequently ran 21 controller cases in 671.136 seconds. Twenty completed without failure, including the updated A/B and legacy-zero cases; the native checkpoint fixture above failed its original exact-two assertion. Its diagnostic rerun passed in 16.996 seconds, the deliberate third-delivery reproduction failed as expected in 12.623 seconds, and the repaired test passed in the focused race run listed above. The original failure's delivery count is unknown; the deterministic reproduction establishes the invalid retry-count assumption. Logs are `/tmp/clientclose-native-checkpoint-diagnostic.log`, `/tmp/clientclose-native-checkpoint-red.log` and the green log above. The broader command exited unsuccessfully and is not presented as a passing suite.

Mutation overlays independently restored the lost-decision and stale-read behavior and made their regression tests fail. The overlays did not alter the worktree sources. `gofmt` and `git diff --check` pass for the changed files. These are targeted suites; a complete repository-wide test run is not claimed.

### Migration and remaining boundaries

- Apply the additive migration for `contract_close.settlement_completion smallint NOT NULL DEFAULT 0` and nullable `settlement_completion_checkpoint_byte_count bigint` before deploying the server change. Existing rows and older writers default to no retained proof; they do not gain inferred cross-party ordering. The migration uses a five-second lock timeout. An old writer can submit a report without changing the aggregate count; the witness preserves the originally accepted accounting state and does not prove that no later report occurred.
- Native control ACKs confirm ingress delivery, not a database commit. The new ownership state prevents synthetic zero finals from overtaking counted delivery; it does not change ordering between independent counted reports. Failed cleanup or shutdown drops the synthetic obligation and retains the existing expiry backstop.
- Notification loss, bounded removal scans, the accepted zero-acknowledged-byte payment policy, and settlement of a contract whose removed party never reported remain the design boundaries described above. The resident hotfix deliberately preserves resident lifetime after transport removal.
