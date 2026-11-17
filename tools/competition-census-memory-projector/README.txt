Qualified standalone local postprocessor: 36 ordinary and 36 race passes,
zero failures or skips, using Go1.27.4 and the exact source-owned twelve cases.
No database, network, native process control, source mutation or arbitrary
test/marker selection. This repairs only test-json Output fragmentation; it
does not change or rerun the native forty-minute diagnostic.

One fixed role: github.com/urnetwork/server/monitor /
TestMigrationsSignalMergedCatalogUsesExactColumns, exact marker
MIGRATION_CENSUS_SESSION_DIAGNOSTIC followed by a space.

Build/test only after the independent diagnostic compiler/actor is released:
Go1.27.4, GOWORK=off, offline stdlib-only, CPU20,22/p1/GOMAXPROCS2, bounded
private SHM TMP. Exact12 top-level TestProjection cases, normal/race count3
(expected36+36 passes, zero skip). The first test causally proves that the old
per-record JSON parser receives an incomplete payload at a1-KiB split, while
the actual streaming processor preserves the complete long event. Other tests
cover multiple records, mixed/non-output/wrong-role records, UTF8+byte-split
reader, truncation, malformed/duplicate/null JSON, every work/size bound,
contiguous event IDs, exact marker boundaries, output errors, private file
custody, symlink refusal and exclusive non-replay outputs/failure receipts.

After qualification, run as root ONLY once the original input owner is closed:
projector INPUT_JSONL NEW_EVENTS_JSONL NEW_RECEIPT_JSON
All paths absolute; input private regular/single-link/no-follow/current-owner;
both new output paths must be unused. The old jq output/status and original Go
log/outcome remain untouched. No overwrite/retry is authorized by this file.

Streaming algorithm: bounded complete outer Go JSON records; concatenate ONLY
the exact target's ordered Output bytes into a bounded logical-line buffer;
parse marker JSON only after its actual newline. Mixed unrelated/non-output
records do not flush that buffer. A target terminal with an unfinished logical
line refuses. Complete events preserve all raw JSON numbers/fields; duplicate
keys, invalid UTF8/JSON/depth, sequence gaps and every over-bound input refuse,
never silently truncate. Streaming output may retain a valid prefix if a later
record is corrupt; the independently written receipt then says incomplete.

Defaults are owned settings:128MiB input,256KiB outer record,64KiB logical line,
64MiB projected output,262144 records,32768 events, JSON depth16. The receipt
records bytes READ/hash, events/output hash, source test outcome if observed,
and projection error. The input hash is explicitly a read-prefix hash when
the parse failed early; it is not falsely labeled the full source artifact.
Input stat/inode/mtime/size are checked again, but cannot prove the absence of
a paused writer: parent must positively close the original owner first.

projection_complete means parsing/reassembly complete, NEVER native success,
memory fix, all-prefix qualification, worker readiness or recovery authority.
The original Go exit and original failing jq extraction stay independent.
The diagnostic sources and original logs remain unchanged; this is a separate
module. Reassembly does not repair the underlying PostgreSQL memory failure.

On 2026-11-12 the qualified binary projected the closed diagnostic's entire
8,715,321-byte log, preserving its SHA-256
78f547ddabdba1b8c82f80cbd26deff22579fe03fb98e79b7fc5444c37531d1d.
It recovered 14,563 contiguous events, including 188 before-apply and 187
after-census markers, and preserved test_outcome=fail. This is partial native
work, NOT the required 221-prefix success. The earlier jq output and errors
were retained independently.
