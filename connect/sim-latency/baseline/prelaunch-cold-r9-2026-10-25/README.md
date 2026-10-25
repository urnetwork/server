# Cold R9 preparation — caller admission correction

Status: **no cold R9 result or accepted baseline yet**. The first attempt failed
before any replicate, build or container. Its unchanged input and failed
precheck evidence remain here; a second fixture is only prechecked, not scored.
Both are explicitly offline, with synthetic API/worker/hardware labels, never
live jobs, provider receipts or host-readiness attestations.

## Root cause and correction

At `2026-10-25T02:58:15Z`, the first evaluator invocation exited 1 in less than
one second: `local mount root must be an absolute canonical directory`, followed
by direct config/local digest mismatch. The leaf's bytes were correct, but the
precheck hashed them with elevated privileges while the manager ran as UID 1000.
The direct synthetic leaves are mode 0700 and owned by the remapped container
UID 165532. The caller could not traverse them. The existing manager service is
explicitly `User=root`, `Group=root`; this qualification follows that configured
identity, not a claim that production has already been activated.

The corrected fixture keeps every source/image/input hash, timer, R9 count,
margin, resource gate and leaf permission unchanged. Its artifact paths and
attempt number differ, and **the entire preflight and future evaluator use the
same UID-0 context**. Direct leaf reads and hashes now pass without elevating
only individual probes. No main API, database, MinIO, vault or service changed.
There was no measurement to discard or reuse from the failed admission.

## Deterministic Go principal regressions

`principal-reproduction/` contains a standalone Go 1.27.3 Linux module. Two
regressions launch the identical helper with explicit credentials and synthetic
files: root reads succeed while an unprivileged UID gets exact permission
denial; a second case proves visible metadata is not proof of read access.
The helper executable remains reachable, so execution failure cannot masquerade
as the expected denied input read. No sleep, scheduler race, production capture
or silently skipped privilege requirement is used.

Both regressions passed three times under the race detector, with zero skips;
owned synthetic `/tmp` fixtures were cleaned. The third top-level test is the
subprocess helper, which is a no-op in the parent invocation. Exact executed
test-source SHA-256:
`753e488136613341f2d90eb4427b225bd39983d11414e7d92aeb4e7195e44adc`.
Executed test-binary SHA-256:
`fd818f1f49f636453271bc6eabf8d3f074c614dd4aef9b95a747f1acc27829d7`.
Reproduction log SHA-256:
`21098d8319580db2d54cf122ed0c7e3bf3de2877b1f2270cd63877a4e22698df`.

To reproduce on this Linux host, select actual Go 1.27.3, compile with
`GOTOOLCHAIN=local GOWORK=off go test -race -c -o principal.test` in that module,
then execute `sudo ./principal.test -test.v -test.count=3`. Compilation need not
be privileged; the test binary must execute as root. Build paths may change
the binary digest; behavioral reproduction, not a byte-identical rebuild, is
the test's completion criterion.

## Preserved records and limits

- `admission-failure/` retains the original request, fixture declaration,
  precheck, resource-boundary receipt, terminal receipt and stdout/stderr.
- `root-principal-precheck/` retains the second prepared request/declaration,
  whole-root preflight and boundary receipt. Its preflight log SHA-256 is
  `e9676c3f8fd4b0b444aa6cc1c666f412ac8cd4ab8e2bc1f6f1be273bcaa6be80`.
- `providers.yml.gz` is the public seed-20261024, 3,600-provider workload,
  unchanged between attempts. It is input reuse, **not measurement reuse**.
- `noop.patch` is the exact comment-only candidate (SHA-256
  `1076be3ed61df0e14ae90c13bbe4c35442de6371064fd7d78f61ed6834e170f1`).
- Raw one-off precheck source is retained as `.go.txt` forensic evidence, not
  an active or accepted production tool. The first precheck is explicitly
  known to be insufficient; do not use its privileged hash as an admission pass.

The enclosing manifest authenticates all files. No completion marker, resource
score or baseline is invented for the failed admission. A real full evaluator
run, unchanged G1–G6, three-hour budget, variance/reference qualification,
live readiness and genuine review/test-provider receipts remain necessary;
see [the predeclared protocol](../../launch/COLD-R9-PROTOCOL-2026-10-25.md).
