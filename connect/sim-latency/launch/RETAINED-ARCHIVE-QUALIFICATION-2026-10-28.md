# First-committed retained archive: synthetic unit qualification

The new [archive package](retained-archive/README.md) passed independent Sol 6.1
low qualification: **27 tests in each of three consecutive offline race-enabled
passes**, 81 test passes total, zero skips and zero failures. Independent Sol 6.1
max source reviews are complete with no remaining blocking finding in this scope.
This qualifies synthetic unit behavior only; live archive operation remains
unqualified and no calibration or launch acceptance is claimed.

## Actual execution and retained evidence

The test runner used UID 1000, the exact cached Go 1.27.3 toolchain at
`/home/by/go/pkg/mod/golang.org/toolchain@v0.0.1-go1.27.3.linux-amd64/bin/go`,
CPU affinity 20,22, two-way Go build concurrency and a 2 GiB memory bound. Each
pass ran only `./connect/sim-latency/launch/retained-archive` with
`-mod=readonly -p=2 -race -count=1 -timeout=5m -json`.

A clean `env -i` selected `GOENV=off`, `GOWORK=off`, `GOTOOLCHAIN=local`,
`GOPROXY=off`, checked-in synthetic Vault/Config roots and private temporary
Home/Site/Go temporary paths. `WARP_ENV=local` kept the PostgreSQL/Redis guard
active with no attestation, so accidental guarded database access would refuse.
The real-vault opt-in, cloud credentials and unmanaged-service escapes were
absent. Tests used temporary owned sources and flock inodes plus an in-memory
transport; no native archive command, retained source, seed, provider or live
storage operation was invoked.

The qualification evidence root is `/tmp/retained-archive-unit.6FYtr1Ga`.
Before execution, the independent runner recorded all nine Go files plus
`go.mod`, `go.sum` and checkout HEAD. After each pass, all source/module pins
and HEAD matched. The checkpoint reviewer independently parsed the three JSON
logs, rehashed the four manifests and verified their pins against current files.
The manifest `source.before.sha256` and `source.after1.sha256` through
`source.after3.sha256` all have SHA-256
`91554333e5ed713dcec3f0aab894b95bde54843e754ef4ee15f9dd3674a98aeb`.

| Pass | Completed at UTC on October 28 | Package duration | JSON SHA-256 |
| --- | --- | ---: | --- |
| 1 | `04:43:22.984398742Z` | 7.223 s | `c47eff2f546a1217d47f9ed56c7fc00dd6eb9c5ca1f2f7d949eda3e0c2e992cb` |
| 2 | `04:43:33.301727966Z` | 7.218 s | `0bfaac24be669f71e5f23f17ac0dde7ed97d3aa8f68787302ef4b81d33fb3a2c` |
| 3 | `04:43:43.670138142Z` | 7.269 s | `dc8315f982a89ddebd2d381279a517d318580696f1dd42acac484e1f90a34a81` |

The files are `pass1.json` through `pass3.json`. Each corresponding
`passN.stderr` is empty, SHA-256
`e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855`.
Package duration is the Go JSON terminal event's elapsed time, not build or
outer-runner wall time.

An earlier three-run set at `/tmp/retained-archive-unit.EYOUDu5m` also passed
27 tests per run with zero skips/failures in 7.231, 7.356 and 7.401 seconds.
It lacked a complete contemporaneous prepass source census; it remains earlier
evidence, not a substitute for the pinned qualification above. Its original
JSON SHA-256 values are, in pass order:

- `80c37dbaaea17f4d4207037ebecba518788baedffd635978a6daa2ae184731a5`.
- `75def428d606da6c3db8b62ee5714c2798d0a218ce131ec500689a8787d5166b`.
- `691fefe661f28d655cf1bbf64c289c7d22840b2645a7edc4169d4b9a3b0e164f`.

Earlier setup failures remain preserved and do not count as passes: the shared
launcher had no readable owner; `/usr/local/go/bin/go` with local toolchain
selection was Go 1.26.6 rather than 1.27.3; and the offline cache lacked the
already-pinned public Coinbase SDK module. The first preflight log at
`/tmp/retained-archive-tests.7uT9CsMk/setup.log` has SHA-256
`5d150aa523d9e6df56fc6909b6753e5ffac0b0de0b646e725c5f6b73b0d31d2f`.
Preparation selected the existing exact cached compiler and populated only
checksum-verified public dependency cache entries; no `go.mod`, `go.sum`,
global toolchain, launcher lock, service or frozen evaluator image was changed.

## Exact tested source

The working checkout base was server
`7e2c1fcabdb392e278181ff99508b22c5a4b94a5`; the new package was not yet committed.
These complete file digests, not the base commit alone, identify its tested bytes:

| File under `retained-archive/` | SHA-256 |
| --- | --- |
| `archive.go` | `7414f19bc2b52e5863f64878e194317ecf5e3f8a21a797deba4c4496e368d180` |
| `archive_test.go` | `ddfef90fd97e0079f992ba739919158ba356e4ace973b5fc0549892fc7074c6b` |
| `files.go` | `b1b5c246f695fd72fddf868b4eac79017c5adea6469df9d12e7bd15fec0f4fbb` |
| `main.go` | `3f15fca0c4eedb6cedb46dc42d107ba60cff603f70c7ff2c3b84936d8fb1d433` |
| `metadata_test.go` | `8d34d016be04de464dac3ca2e87307686d69a926e600e501d41df881db57d491` |
| `native.go` | `b3367a66f97bc593135088ad8bd9f503d714119ad96e183799bd9854723adc21` |
| `types.go` | `c4c8ea364c8feb416ed81d422b66b8a7c14c49b27719f0f5a19054aea04025ee` |
| `upload.go` | `70f26aa96e6925ca3783b7c62f3dcf6541b9f22f50fd4f21925b927933f1564d` |
| `upload_test.go` | `3c2106de2473f1a95601350e09135f987b4e06c186cf78cee6e3967d8782d860` |

The unchanged module pins were `go.mod`
`d4504db17c2096c85cacbb265449d9480748091456617a756a4868227f041b8f`
and `go.sum`
`3ce94ba627bfacb55e3052e05d3e575c658f6666690bba2cfd004bf4a87725f1`.

Review covered protected ancestry and member validation, complete bounded census,
opaque streaming, durable one-use claims, separate exact-plan upload authority,
retention identity, exact-version content verification, cancellation and refusal
without retry or rollback. The final same-length corrupted-readback test flips
one byte without changing length, requires the content-digest refusal, retains
the spent claim, forbids a receipt and proves that another invocation cannot
retry the fake transport. It was present in all three passing runs. Initial
independent max review covered the 26-test version; the full-package independent
review separately reviewed the final added 27th test before execution. The
checkpoint reviewer then read all nine files without changing their bytes.

## Remaining boundaries

The standalone native command binary has not been built, installed or assigned a
binary digest. The complete `tests.sh` integration wrapper has not passed in this
qualification. Root-only command dispatch, the exact live scoring inode and
evidence ancestry, native blob configuration/protection/version history, retained
upload, exact remote-version readback and replica delivery remain separate,
unperformed integration checks. Source review and fake transport tests do not
prove them. No archive was prepared or uploaded and no originals were deleted.

The [failed first screen](FIRST-COMMITTED-SCREEN-2026-10-28.md) remains failed.
The [new-screen approval](CALIBRATION-NEW-SCREEN-OWNER-APPROVAL-2026-10-28.md)
does not supply a prepared seed, reviewed exact protocol, new measurement,
provider waiver, late opening or production activation. Stop after publishing
this checkpoint; later build and native archive work require their own admission.
