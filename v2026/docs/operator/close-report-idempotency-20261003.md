# Checkpoint report replay

The native SDK can retry one incremental checkpoint as a fresh transfer after
the receiver commits it but its ACK is lost. Transport sequence deduplication
does not cover that fresh operation. A local test on release5900 source610a
delivered one checkpoint100 twice through `ConnectControlFrames` and the real
PostgreSQL model. Final destination usage became300 against a200-byte grant and
source report200. Settlement correctly refused the resulting250-byte bilateral
usage; payer funds and provider totals remained unchanged. Two independent
equal-size checkpoints100+100 settled200 normally. Final-close retries were
already inert. These are synthetic controls, not attribution of existing Main
reports or authorization to rewrite them.

Migration764 appends `contract_close_report` after the exact deployed1–763
prefix. A receipt is keyed by contract, authenticated party and a nonzero UUID
report ID, and stores the acknowledged amount and checkpoint flag. Receipt and
increment share the existing contract-row transaction. An exact retry skips
the increment and resumes unfinished settlement; terminal retries succeed
without replaying accounting. Changed amount/finality or a non-party request
is rejected. Equal-sized independent reports retain distinct IDs. No shared
payer/network lock is added to close-report admission. Receipts remain until
their owning contract is deleted, when its foreign-key cascade reclaims them.
The ignored unacknowledged-byte field does not participate in billing identity.

The optional protobuf field is backward compatible. This backend commit and
its protocol-only dependency do **not** enable ID emission. Empty IDs retain
the existing legacy path and never query the new table. Existing applications,
including iPhone clients, can keep using that path. Their ID-less incremental
replays remain ambiguous: identical payloads may also be legitimate reports.
Payload hashing, amount clamping and rewriting original reports are not safe
repairs.

Roll out schema and backend support first, verify the native764 catalog and
the relevant API/Connect backend generations, then enable stable IDs in hosted
SDK close operations. An old backend ignores the new field and cannot retain
its receipt, so routing an operation across old and new backends does not
establish exactly-once application. Rollback must keep compatible readers and
writers for any ID-enabled producers. Schema764 needs its own backup/migration
prerequisite; the previous759–763-only48-hour exception does not cover it.
Installing or starting the streaming backup writer is not a completed recovery
point. The qualified current watcher also needs an appended764 artifact
contract before promotion; this service source must not replace it with an
older monitor tree.

Focused controls exercise native ACK loss and equal-size independent reports,
atomic receipt failure/rollback, a committed report whose response and later
settlement are lost, payload conflicts, cancellation, terminal replay, and128
separate contracts while the shared payer balance is locked. Existing reports
and insufficient-escrow rejections stay intact. A replay-capable local control
does not prove that ACK loss caused the retained Main accounting cohort; no
historical operation IDs were recorded there. Current queue growth, mixed
generations and stale observations remain separate visibility limitations.
