# Retained archive: published synthetic and build evidence

This compact bundle contains reviewed, credential-free synthetic test logs and
native build metadata. No live archive, inventory, provider snapshot, private
request, seed value or raw WORM receipt is included.
The [manifest](MANIFEST.sha256) pins every published file in this bundle.

The [unit qualification](../RETAINED-ARCHIVE-QUALIFICATION-2026-10-28.md) records
three independent LOW runs of 27 race-enabled tests, zero skips/failures, with
before/after source/module/HEAD pins. `unit-pass1.json` through `unit-pass3.json`
are the byte-exact raw logs; their original private stderr files were empty.
`unit-source-pins.sha256` identifies nine Go files plus `go.mod` and `go.sum`;
`unit-source-head.txt` retains the precommit base. The unit runs used
`GOMEMLIMIT=2GiB`, a **soft Go runtime target, not a hard RSS cap**.

`native-buildinfo.txt`, `native-provenance.json`, `native-build.stderr.txt`,
`native-build-scope.txt` and `native-build-journal.txt` are byte-exact build-phase
records. That separate successful build used an observed hard cgroup
`MemoryMax=2147483648`, swap disabled, UID 1000, CPUs 20,22, Go 1.27.3 and CGO=1.
The installed executable's SHA-256 is
`6132191ee9ae1bf9c410fa16fee508c0f02cdcb2c60e94df069cadd5e969964c`.
Build-info SHA-256 is
`4bacd203ab0fc85a50e1d8a3350ce998b1f2ef3ed0e7f9353a8aa02cdbd5842e`;
provenance SHA-256 is
`16fe096bf8b312e10ce1d160d64141aaf707f029b659b88f2bd2d9ec62525877`.

The raw provenance truthfully says independent installed verification and native
use were pending **when it was created**. Those original bytes were not relabeled.
Subsequent independent installed verification and three successful retained
source-version readbacks are recorded separately in the
[retention record](../FIRST-COMMITTED-ARCHIVE-RETENTION-2026-10-28.md).
The provenance's pending unit-memory wording correction is applied in the
current qualification document. The scope snapshot is a live sample, not a final
exact peak; the final journal reports its rounded peak separately.

These scoped observations do not prove a complete `tests.sh` wrapper run, a
full transitive source census, delivery to a replica, calibration acceptance,
or production readiness. Local paths in raw records preserve original provenance;
they are not portable invocation instructions or permission to repeat an upload.
