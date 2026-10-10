# Production debugging techniques

Use this catalog alongside [SIGNALS.md](SIGNALS.md) when an alert identifies a
symptom but the asynchronous execution path obscures the cause. Record what was
observed, what remains a hypothesis, and the test that distinguishes them.

The [async task audit](../taskworker/TASK_AUDIT.md) maps the registered targets
to their durable effects, replay/continuation rules, and regression entry points.

## Run the core operation synchronously against the affected environment

**Use when:** queued work reports success or no errors, but its intended durable
effect is missing. Contract expiration is one example; financial projections,
cleanup, and other multi-stage jobs have the same debugging shape.

Build a small synchronous operator path that calls the same model operation as
the task. Keep task scheduling and the synchronous loop separate. The loop must
await the actual operation and its commit, then read back the business state.
This removes queue admission, task handoffs, worker availability, and retry
timing from the immediate investigation while retaining the real production
data, constraints, and accounting rules.

A successful function return or task completion is not proof of the business
effect. First establish exactly what that return acknowledges: enqueueing an
intent, committing a debit, closing a contract, or finishing a projection.

1. Identify the alert's durable invariant. For contract closure, the relevant
   contract must have a terminal `outcome`; also inspect its settlement intent,
   escrow payout, payer debit, and provider earnings. A closed counter alone
   does not establish those facts.
2. Establish provenance: environment, database destination, local diff, task
   arguments, and deployed worker version/configuration. A local checkout is
   not evidence of the binary running in production. Keep raw production
   identifiers and captures outside the source repository.
3. Select a bounded sample and capture its initial state with read-only queries.
   Include both ordinary rows and the failing shapes. Release that connection
   before invoking a model function that acquires its own transaction.
4. Run the shared operation synchronously, within the authorized production
   scope. Preserve cancellation, bound each attempt, and emit a result for
   each item. Distinguish failed attempts, already terminal rows, missing rows,
   future deadlines, and newly committed closures. Preserve partial counts
   if the operator interrupts the scan.
5. Verify each acknowledged effect with a fresh database read. A read in the
   writer's transaction proves only its uncommitted view. Check financial
   conservation and a duplicate invocation as well as the terminal flag.
6. Compare with the task path using the same operation and equivalent inputs.
   If both fail, investigate the model, data, and transaction. If synchronous
   execution succeeds but queued execution does not reach the operation,
   investigate admission, ownership, routing, and worker configuration. If
   the task enters the operation, inspect its result and completion transaction.
7. Convert the observed cause into a deterministic regression using synthetic
   data. Exercise the real boundary where the failure occurs, including the
   helper that dispatches, commits, retries, or hands off the work. Validate the
   fix against the failing case before broadening the production run.

The contract command has a dedicated synchronous implementation:

```sh
cd bringyourctl
./run-main.sh contracts schedule-open-closures --limit=10
```

Despite its retained command name, this path calls
`model.CloseOpenContractsSynchronously`; it does not enqueue closure tasks.
It skips stored future expirations and uses the scan's explicit retirement time
for legacy rows without an expiration. It never infers a legacy signed lifetime
from an identifier or creation timestamp. Each `closed` output follows a commit
and a separate terminal-state check. Read the final counts and individual
failures; a partial run is not a successful full scan.

Relevant code and tests:

- [Synchronous operator loop](../model/subscription_contract_close_scan.go)
- [Shared deadline reconciliation](../model/subscription_contract_scheduled_close.go)
- [Accounting failure and replay tests](../model/subscription_deadline_reconciliation_test.go)
- [Task scheduling and deadline admission](../taskworker/work/startup_contract_closure.go)

### Expiration failure rules

The authoritative requirement is documented on
[`ReconcileContractAtDeadline`](../model/subscription_contract_scheduled_close.go).
A successful expiration close of an existing contract requires a committed
terminal outcome. Reconciliation uses retained evidence and available funds:

| Condition | Expiration handling | Regression |
| --- | --- | --- |
| Report exceeds escrow or available balance | Charge the funded portion, pay from that charge, release the unused reservation | `TestDeadlineReconciliationClosesAccountingFailures` |
| Missing grant, wrong payer, invalid valuation or ambiguous participants | Preserve evidence; pay only the attributable, funded portion, possibly zero | `TestDeadlineEscrowPayoutsBoundEveryFundingFailure` |
| An accepted debit already consumes funds | Reserve that debit first; do not charge the same usage twice | `TestDeadlineEscrowPayoutsKeepPriorConsumption` |
| Invalid or one-sided reports | Preserve original reports; reconcile valid usage without fabricating a final report | `TestDeadlineReportAmountsReconcileOriginalEvidence` |
| Existing malformed/excluded usage | Keep it immutable, mark retirement unverified, and close | `TestDeadlineReconciliationRetainsExcludedOpenEvidence` |
| Malformed optional signed evidence | Refuse the optional receipt while committing the reconciled close | `TestDeadlineReconciliationMalformedSignedEvidence` |
| Busy financial owner, unavailable database, cancellation or failed required write | Explicit failure and atomic rollback; retain the task for retry | `TestDeadlineReconciliationCancellationRollsBack`, `TestDeadlineReconciliationSignedPublicationFailureRollsBack` |
| Already terminal or concurrently removed row | Return an explicit already-closed or missing result; never count a new close | Deadline and synchronous-scan tests |

The appended `transfer_balance_grant_run` migration protects recurring grant
tasks from replay after a lost completion acknowledgement. The subsequent usage
guard migration permits expiration to retain existing excluded proof; it does
not permit new or changed legacy proof. Apply these schema changes before
deploying their writers. Historical migration definitions remain unchanged.
Grant receipts protect work committed by the new writer; they cannot establish
whether an older binary already granted an unacknowledged task. Avoid mixed
old/new grant workers during that transition. No production migration or worker
deployment was performed during this investigation.

### Example: successful close logs with open contracts

In the contract investigation, a direct local loop logged `Closed` after
`CloseContractAtDeadline` returned no error. A read-back of all 161 successful
log entries in the interrupted sample found that every contract still had
`outcome IS NULL` and a legacy settlement intent. The function had acknowledged
the existing financial handoff; the log had overstated its meaning. There was
also a real timeout, and cancellation produced additional failed attempts.
Those are separate observations, not successful closures.

The intent worker exposed another layer: it recorded an accounting failure and
scheduled another attempt when reported usage exceeded available escrow. Some
sampled escrows referred to deleted grants. Repeating the handoff could not
resolve those cases. Deadline closure therefore needs explicit reconciliation
rules for insufficient funds and missing accounting data, followed by an atomic
financial and terminal commit. Infrastructure failures must remain explicit
errors, with replay safe after an uncertain commit acknowledgement.

After reconciliation was implemented, the bounded synchronous command reported
10 new closures. A separate read-only transaction verified all 10 terminal
outcomes, no remaining intents or unsettled escrows, and unchanged original
reports. These sampled escrows had missing grants, so no funds or provider
earnings were invented. A local worker restricted to `CloseScheduledContract`
then completed two real queued tasks; fresh reads verified both terminal
contracts, absent intents, and completed task Post steps.

The synchronous technique revealed the model issue; it did not, by itself,
explain or repair queue starvation. Investigate those layers independently.

## Trace accounting corruption before the expiration fallback

**Use when:** forced reconciliation closes a contract, but its original reports
or funding disagree. A successful fallback does not establish that ordinary
close accounting was correct.

Compare contract capacity, both accumulated reports and their checkpoint flags,
retained report identities, escrow reservations, surviving grant records, and
the accepted settlement intent. Check the contract's creation/report dates
against the deployed writers. Keep normal-close reproductions before the stored
expiration so the fallback cannot conceal their failure.

Two mechanisms reproduce the accounting failures from the closure incident:

- The old id-less path summed checkpoint increments. If an acknowledgement was
  lost, sending it again incremented the same party twice. With a 600-byte
  reservation, one 400-byte checkpoint delivered twice plus a 100-byte final
  produces 900 bytes at the receiver against 500 at the sender. The 700-byte
  mean exceeds escrow; normal Redis settlement fails and legacy settlement
  retains a failing intent.
  If the report difference exceeds 16 MiB, ordinary close instead marks a
  dispute. That call returns nil but does not commit a terminal outcome.
- Historical retention deleted expired grants without excluding unsettled
  escrow. Normal settlement joins escrow to its grant; deleting the grant makes
  reserved funds disappear from that calculation. The current retention owner
  protects unsettled escrow and pending debit journals. Grant expiration ends
  admission and must not erase an outstanding financial obligation.

The three original sampled contracts were created September 22–26, 2026. Their
destination totals were approximately twice their source totals, two had missing
grants, and none retained report identities. They predate the October 3 report
deduplication changes. These observations fit the reproduced mechanisms; without
individual historical reports or deletion audit records, they do not prove the
exact duplicated delivery or grant-deletion invocation.

The compatibility path remains relevant. A bounded October 10 sample of 128
contracts created about fifteen minutes earlier included two positive
destination checkpoints on contracts with no report receipts. Most other
id-less reports in that sample were zero-byte source finals, which do not
demonstrate checkpoint duplication. This is evidence of the old report path
remaining in use, not an estimate of its prevalence across production.

New SDK reports retain one `ReportId` through native and out-of-band retries;
both controller routes must preserve it into `CloseContractWithReport`.
Old clients remain supported with deliberately conservative accounting: retain
the largest id-less checkpoint, and use the greater of that bound and the sum
of distinct identified checkpoints if delivery routes mix. Add the final
increment once. Equal-sized legacy operations cannot be distinguished from
retries and may be undercounted; identified operations remain independently
recorded. Never fabricate report identities or signatures to fill that gap.

A conservative lower bound below an exact peer is not evidence of a dispute.
Ordinary settlement uses the existing mean of the accepted totals. A lower
bound exceeding an exact peer still triggers the ordinary disagreement check.
Transport metrics count only newly committed report bytes, including when
admission succeeds but later settlement needs a retry. Billing uses the
reconciled totals; neither should count a replay twice.

Migration 813 adds `contract_close.legacy_checkpoint_byte_count` and
`identified_checkpoint_byte_count`; apply it before deploying the new
close-report writers. The identified total bootstraps from existing receipts
when an old row first changes, then advances with each new receipt. All writers
must be upgraded before assuming the old accumulation path is gone. Existing
aggregates are preserved as a historical floor, so this change prevents new
inflation but does not reconstruct unknown old deliveries or repair their
totals. Expiration's fair reconciliation remains responsible for those
unfinished historical contracts.

Also distinguish expected waiting from a failure: a source final plus a
destination checkpoint intentionally remains resumable until the destination
finalizes or expiration retires it. A dispute can have `open=false` and a
`close_time` while `outcome` remains null. A legacy close can acknowledge an
intent while still awaiting money settlement. The terminal outcome, together
with the debit and payout records, is the closure authority.

[Normal-close root-cause tests](../model/subscription_normal_close_regression_test.go)
exercise duplicate delivery with and without report identities, both financial
backends, the dispute threshold, and historical versus protected grant
retention. Replaying the same committed legacy checkpoint caused the tests to
fail with insufficient escrow or dispute before the fix; both report modes now
settle before expiration. The retention negative control still reproduces a
missing grant. The
[controller route tests](../controller/connect_close_report_test.go) also check
that HTTP and resident-frame retries share the same report receipt and metrics
count only newly committed bytes. The
[legacy compatibility tests](../model/contract_close_legacy_test.go) cover mixed
delivery order, rollback, concurrent retries, finality and directional disputes.

## Prove queue starvation at a bounded admission boundary

**Use when:** due tasks accumulate behind another task function, especially
when producers depend on newer downstream tasks to finish their business work.

Inspect the actual candidate query and limit, not just aggregate queue counts.
Compare due time, claim/release metadata, function, retry count, and the retained
task arguments. An old unclaimed task is different from an actively retried
task. Increasing batch size or assigning priority may not help when an earlier
sort key fills the entire candidate window.

Reproduce the mechanism with more old producer rows than the candidate window,
then insert a newer due consumer row. Run a fixed number of real admissions.
The negative control must leave the consumer unclaimed while the old backlog
remains. The corrected policy must admit and complete it within a bounded
number of turns while still progressing the producer. Use controlled state
transitions and barriers, not a sleep or a throughput guess.

Test every entry point that reaches the admission helper. In this investigation,
function fairness was enabled by the executable's `Run` setup but omitted by
`InitTaskWorker`, and `EvalTasks` ignored the option even when enabled. Testing
only the native polling loop could miss both adjacent paths.

- [Deterministic starvation control and regression](../task/claim_function_starvation_test.go)
- [Function admission policy](../task/claim_function.go)
- [Worker construction policy](../taskworker/claim_admission.go)

Claim statements share the collector's guard session with live executions.
When a refill claim's context expired during a slow FETCH, pgx closed that
session, and every live finalization on it then failed with `conn closed` or
`database ownership session is not idle`. A claim with a deadline now keeps
`statement_timeout` below its remaining budget before each statement, so the
server ends a slow claim statement first and the session stays usable.

- [Guard session survives a slow claim statement](../task/claim_statement_budget_test.go)

A production queue prefix alone does not prove which scheduler binary or policy
is running. Preserve that uncertainty until deployment provenance and actual
claims establish it. Likewise, a local metric increment may be absent from a
dashboard that scrapes only deployed processes; verify the database effect
independently before interpreting the metric.

## Separate a refused claim from a failed business operation

Trace discovery, advisory admission, the exact row recheck, claim commit,
post-commit metadata read, task body, and finalization as separate boundaries.
An unlocked discovery cursor can retain a row that another worker completes
before the exact recheck. Acquiring its advisory keys does not prove the row
still exists or remains eligible. Inspect that exact identity in both
`pending_task` and `finished_task` before diagnosing a refused recheck as a bug.

In this investigation, all eight sampled rejected identities had been finished
by another worker between discovery and recheck. Their Post steps were complete,
but their contracts remained open and their results contained settlement
handoffs. The refusal was correct; the acknowledged business effect was wrong.
The [claim recheck tests](../task/task_claim_recheck_test.go) use transaction
barriers to cover deletion, future leases, held row locks, independent progress,
and successful admission after release.

## Reproduce resource starvation at the acquisition boundary

A task can reach its body and still block before its first business write if
it retains a connection and calls a helper that acquires another connection of
the same type. Trace ownership through helpers and cache refreshes, not just
the visible task function. Separate this from queue admission starvation.

Payment reconciliation held a session advisory lock across a run, while its
model calls acquired PostgreSQL again. Its skipped-store audit path alone was
enough to need a second connection. The repair keeps the run lock and passes
the same direct PostgreSQL connection through reads, sequential transactions,
entitlement refreshes and audit writes. Releasing exclusion early or using a
second pool would not repair the ownership boundary.

Use a disposable database with singleton pools and an acquisition tracer that
rejects a second acquisition while the run lock is retained. This reproduces
the circular dependency immediately; a short elapsed-time timeout is not the
proof. The regression is
`TestPaymentReconciliationUsesOneSessionWithSingletonPools` in
[payment_reconcile_connection_test.go](../controller/payment_reconcile_connection_test.go).
The guard also runs with the store repair and webhook-race tests. Transaction
tests separately verify that an uncertain commit cannot replay or replace the
session carrying the lock.

A restricted contract worker also committed a claim, then timed out acquiring
the ordinary database connection for its metadata read. A separate bounded
`SELECT 1` failed on that pool while the direct maintenance pool succeeded.
This was an independent availability observation; it did not prove the claim
helper deadlocked. A subsequent restricted task run succeeded. Avoid replacing
a failed production dependency with a different connection path and then
claiming the original path was verified.

## Discard task completion after a successful body commit

For each task, identify what happens if its model transaction commits but the
worker never records the result. Run the real serialized body against a
synthetic pending row, deliberately discard the result, then let the normal
worker claim the same row. Inspect both the durable effect and the successor.

This exposed two different failures: free/pro/referral grants were minted
again on replay, while an onboarding campaign advanced its model row and then
lost its successor because the stale-step fast path returned no continuation.
Grant tasks now retain an atomic receipt keyed by task identity; campaign
replay reconstructs the successor from the retained campaign state. New task
identities remain distinct grant runs, and stopped campaigns do not restart.
See [grant replay tests](../controller/subscription_grant_task_test.go) and
[campaign replay tests](../controller/onboarding_campaign_task_test.go).

## Keep a singleton batch moving past one failed item

**Use when:** a recurring singleton reports the same few item failures on
every attempt, its `reschedule_error_count` keeps growing, and `run_at` drifts
toward the one-hour backoff cap while other items in the same batch do commit.

Compare the stored error's item identities across attempts with the durable
state of the items that did not fail. If those items committed but the task's
arguments did not advance, the batch is retrying from the same position and
reaching the same failures first. Then find which deadline produced a context
error inside one item: a bounded child operation such as a connection check,
commit or ownership admission can expire while the task context stays live.

In the 2026-10-10 `CloseExpiredContracts` stall, each attempt closed about 216
contracts, yet about 30 rows near the head of the same raw page failed with
`timeout: context deadline exceeded` before their expiry proof committed. That
text is pgx's wrapper for an operation whose own context deadline expired while
the task context was live. In that first no-retry transaction only two
deadlines can surface this bare text: the five-second validation ping of a
newly created pooled connection and the 30-second commit; the attempt's
duration excludes the commit. The ping is the inferred cause, not a measured
one. Because the page witness refused any context cause, the cursor never
advanced, and ordinary backoff had reached roughly an hour after 29 attempts.

A completed page now issues a row receipt for each failed row once its parent
context is still live after every row returned. Such a context stop is that
row's own failure. The page records the rows it left open, advances its
cursor, and the task stays failing with every row in its stored error. When
other rows made progress, it retries at the ordinary two-to-four-second scan
cadence; the rows return on their lane's next pass. A page on which every row
failed keeps ordinary backoff, capped at one minute for this target. A single
slow subpage stops starting rows after two minutes and checkpoints the prefix
that started. Nothing here closes a failed row or changes the financial rules.

- [Page receipts and dispatch limit](../model/subscription_model.go)
- [Row receipt witness](../model/subscription_expiry_page_progress.go)
- [Model regressions](../model/subscription_expiry_deferred_row_test.go)
- [Task-path regressions](../taskworker/work/subscription_deferred_row_test.go)

Distinguish a row refused by another owner from one refused by its own page.
Deadline reconciliation try-locks every grant of the row's payer. Two rows of
one payer started together in the same page refuse each other, so one was
always reported busy; both startup-expiry tests in the taskworker package
first failed on exactly that refusal. A page now reads, in one bounded lookup before
any transaction, which selected rows are already past an explicit deadline,
and gives each payer's such rows one turn at a time; other rows keep full
parallelism. A failed lookup only disables the turns. See
[payer turns](../model/subscription_close_turns.go) and
[their regression](../model/subscription_close_turns_test.go).

`MaintainNetworkSessions` had the same shape: one failing index member or
operation cleanup stopped the rest of its shard and the 30-second task fell
into hour-long backoff. Its sweep and recovery now continue past a failed item
and return every failure, and its target caps the retry delay at 30 seconds.
