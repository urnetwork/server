# Current financial source integration

This successor joins qualified financial source `5089a9dde0ca5d60a8dda0493c861de53ed4e0c4` with upstream `f024b11a7666d3ba0e1025c083123d32f01285c2`. The original `1d3c` full-model result retains its exact source and graph; it does not qualify the upstream asynchronous-debit changes.

The complete published migration prefix from `f024` remains in its original order. The unpublished customer challenge migration follows the debit, test-drain, and Solana payment migrations. The tracked published Connect 631, SDK 9ae, SCTP 644 and explicit gvisor fork remain selected.

The bounded Redis grant selector retains its finite rows, selected-grant count, unknown-counter refusal, owned compensation and SQL publication fence. It grants a smaller contract only after observing a complete census with enough credit for the existing shrink floor. Contract bytes, escrow allocations and Redis reservations carry the exact granted amount.

Owned-token recovery observes terminal state and the debit journal in one SQL snapshot. An unapplied debit retains its token and recovery marker, including after settlement has reduced the token or contract history has been archived. The asynchronous debit worker still owns writeback and eventual release. After a committed writeback, recovery recognizes the exact reduced amount from its applied debit record. Other recovery candidates continue; no global payer lock or historical scan is introduced.

The incoming debit lost-ack fixture now follows the actual Eval key count, including the fifth owned-recovery key. New controls cover bounded shrink, incomplete-census refusal, pending debt plus healthy-peer progress, reduced applied tokens, archived pending debt, the real release hook and migration order. They require separate normal/race qualification; source integration and compilation alone are not a behavioral pass.

Redis remains the existing approximate admission cache, with its original expiry and loss semantics. PostgreSQL debit records and immutable provider usage retain financial authority. No database migration, production service, payment or chain transaction was executed by this integration.
