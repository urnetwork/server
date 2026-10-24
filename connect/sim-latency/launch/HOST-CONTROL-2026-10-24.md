# Inactive host-control release — October 24, 2026

## Complete exact-Go bundle — 22:24 UTC

A separate complete bundle is installed, **inactive**, at
`/usr/local/libexec/urnetwork/competition-control-c5f3e70d7-go1273-v1`.
It uses the exact four clean Go 1.27.3 native programs recorded below, plus the
trusted evaluator helpers from the same clean main source and freshly pinned
build/qualification wrappers. Existing release directories are untouched.

All directories and executables are root:root 0555; other public helper files
are 0444. The two private settings files are root:root 0400. Wrapper settings
select only protected absolute paths and digests, never candidate-controlled
commands. The active-host path remains `/etc/urnetwork/competition-host.json`;
this installation does not rebaseline or replace that document.

| Installed file | SHA-256 |
| --- | --- |
| `source-build` | `64646e05c297024bb67f59380cc7ba21c873544271e8ba8b1287b2727a1083eb` |
| `source-build.json` | `59568777ec1fe8181555914efda8ca822776d0e07358f9e967493fe4724d2a89` |
| `source-qualify` | `0b571892427ec78a4f24839ad8c893ea36867da221dff300f9a44940f3e6be03` |
| `source-qualification.json` | `5ecac467446de2d0f8fdededdfffc6e767e8577d3c45aa208d73eff18abf17f2` |
| `competition-host-self-check` | `c6db6d3b8a5c0214c82bc2d2258ccad93a236acf2ffa36e07584050799ab78a9` |
| `evaluator/host-self-check.sh` | `5e47a21f8497145864c6cd2d3a47322d2567fae28f2499f193ec844b35a43618` |
| `review-broker` | `4541194193bd9a44d9932c32fb7f59e7a03abc96474cd6e063eeca99c3a3fd3f` |

Private preparation and minimal review-image context:
`temp/control-complete-go1273-20261024.QwuC5l1E` relative to the mono root.
The review image has not yet been built from this context. Dedicated model-key
provisioning, genuine provider-backed review and qualification, prospective host
qualification, actual API/worker readiness quorum and activation remain required.
The independent installed-bundle readback/argument-rejection check is pending;
static shell/JSON checks and installation hash readback passed. No main DB,
runtime policy, global review settings, service or supervisor was modified.

## Exact-Go native control release — 21:47 UTC

The latest four native control programs were built from clean main
`c5f3e70d76219798f4ed704c2c535d1aa4cb5f56`, using Go 1.27.3, Linux/amd64,
CGO disabled and greenteagc. Each binary reports `vcs.modified=false` and that
exact revision. This release includes both per-intent API acknowledgment
authentication and the strict Go 1.27.3 build/artifact policy. The corresponding
main runtime code passed three consecutive full simulator suites before build.

Installed directory:
`/usr/local/libexec/urnetwork/competition-control-c5f3e70d7-98b9412e`.
The directory and all four programs are root:root 0555. Installed hashes match
the retained build outputs:

| Program | SHA-256 |
| --- | --- |
| `competitionprepare` | `51be152b2cb58a2bfdcb06cd7c3d5fb10ee5d6819826ee4ffbaf300949da5efc` |
| `competitionqualify` | `91429d7a5f58d014841bf852ef9c16833f780023fbcd19d0445d7fdc95ab8311` |
| `competitionworker` | `ca69337cf487920548d159f6afba2b5a325f5a994b65e315577ed97b769c61d9` |
| `sim-latency` | `98b9412e6c5f0161895864eeb0e9f645a54790bd68d62e57289b10fa806b2ebd` |

Complete binary build-info records are retained under the mono-root-relative
`temp/main-control-go1273-20261024.KcjKPbrp`. No test or scoring run overlapped
this build; it overlapped only the separate trusted evaluator-image preparation.
This native release is inactive. No unit, wrapper/settings pin, host config,
staging supervisor or main control-plane row was changed. Older release
directories remain untouched. These program hashes are not OCI worker-image
digests and must not be presented as such. Exact release wiring and genuine
review/build/host qualification remain required before production activation.

## Later native release installation

At 20:00 UTC the four already-built native control programs from clean main
`822902437d8614b18ed89a850b5c1646a47c55a3` were installed separately under
`/usr/local/libexec/urnetwork/competition-control-822902437-61f7a622`.
The directory and all four programs are root:root 0555; installed SHA-256
values match the retained build outputs:

| Program | SHA-256 |
| --- | --- |
| `competitionprepare` | `b6d9e2eba649e8d4530e495d09be7504397b156b0285fb44d6497d8eae7c2433` |
| `competitionqualify` | `c069d27e78ccdbe4765fc13ae0de02c3eabadcf204ed2761e5ab3815de489e96` |
| `competitionworker` | `1003f4dbb1cc2a68bd5bc8278f7110cc7a9314600b5da20b9f6c7a6d26d1d6e7` |
| `sim-latency` | `61f7a62206227d03b76f64fa558731df09bf7bef57f241954b3b050a91671143` |

This release includes the per-intent API identity correction described in
[CONTROL-AND-MODE-2026-10-24.md](CONTROL-AND-MODE-2026-10-24.md). It is not
activated: no service, wrapper/settings pin or active host configuration was
changed. The older complete release below remains untouched. Trusted wrappers,
settings and genuine provider-backed qualification must be bound to their exact
chosen release before activation. No build or test ran during this installation.
These are native control programs, not an evaluator-image or worker OCI digest.

At 19:59 UTC the public API still returned 503, MinIO at
`172.28.208.177:23900` refused connections, and the dedicated review credential
was absent. Main maintenance remains a control-plane write hold.

## Earlier complete inactive bundle

Installed on sille at 17:19 UTC for launch preparation, **not activated**.
This release controls evaluation and reviewed-source promotion; it does not
replace or advance the frozen ten-repository evaluated checkpoint recorded in
[PRODUCTION-2026-10-28.md](PRODUCTION-2026-10-28.md).

## Provenance and installed identity

Source: server `c9b10800338eedf515d0f083d18a303944ab07c3`, reachable from
published main. All four native executables report `vcs.modified=false`,
`CGO_ENABLED=0`, Linux amd64, built with Go 1.27.3. Later documentation/merge
commits do not change this recorded build identity.

Root-owned release directory:
`/usr/local/libexec/urnetwork/competition-control-c9b108003-ba4a64e3`.
Directories/programs are mode 0555; the two private settings files are 0400.
New empty runtime directories `competition-reviewed-builds` and
`competition-source-tests` under `/var/lib/urnetwork` are root-owned 0700.

| File | SHA-256 |
| --- | --- |
| `sim-latency` | `ba4a64e310b12274b8113a36325a07e0d886e4e7e8773a4d83b7536a977aa9d4` |
| `competitionprepare` | `4db3078e27c381498ef882a1876f5394a6e14873a9abfe4235831b0f9727876c` |
| `competitionqualify` | `7e51f5ea143387da80eb68ffe8417db4624efddbf67a6516fa56744d0f90ad25` |
| `competitionworker` | `8ca016fa34ac9626d26ea8aacade93d5648a48c70fa696b94796539f3e143c28` |
| `source-build` | `cb556d4c2e9d1c70428805ef184b00ca85fc28d40bbab6adb95c62f21673ec70` |
| `source-build.json` | `c2b09bc2251e061921aee94a07b211b5a6638bca3cf7fcf1ccc72679f462bc66` |
| `source-qualify` | `091604c08ed9976214d49ae2a07f288d176041309c79b313e74dc26974a2505a` |
| `source-qualification.json` | `ff317946ea30f68cdf94ef20523031a4a0d15363ec33f008cfc18ea892cca191` |
| `competition-host-self-check` | `c6db6d3b8a5c0214c82bc2d2258ccad93a236acf2ffa36e07584050799ab78a9` |
| `evaluator/host-self-check.sh` | `5e47a21f8497145864c6cd2d3a47322d2567fae28f2499f193ec844b35a43618` |
| `review-broker` | `4541194193bd9a44d9932c32fb7f59e7a03abc96474cd6e063eeca99c3a3fd3f` |

Private preparation/archive location:
`temp/reviewed-host-release-20261024.C88DAxW7/control-bundle.tar` relative to
the mono root. Archive SHA-256:
`ca2cc5bac96f4c4e48182dba8bc92d00c9e4852af981ea5d1df27780c66ed385`.
It contains only the release bundle, with sorted names, epoch-zero timestamps,
numeric root ownership and installed permissions. It contains no model keys,
main configuration, vault, database fixtures or source checkouts. This is a
native archive identity, **not an OCI worker-image digest**.

The independent review scratch image is locally available as
`sha256:403a65d08bc549b7720d5b78e255cfaee880fc421f07cefc12964dd6150cd478`.
Its minimal build context contained the exact native simulator/control binary,
public CA certificates and tracked review Dockerfile, not the surrounding
private fixture directory. A readonly, non-root, capability-dropped,
network-disabled `--help` smoke test passed. That proves executable startup,
not a completed provider-backed review or egress qualification.

## Qualification already executed

Before installation, the reviewed-host transition/preparation controller suite
passed three race repetitions against disposable local PostgreSQL/Redis;
`connect/sim-latency/tests.sh -count=1` passed three consecutive complete runs.
Post-merge focused controller/simulator tests passed three race repetitions.
Logs are retained in private
`temp/reviewed-host-validation-20261024.01yQq2cF`:

| Log | SHA-256 |
| --- | --- |
| `controller-host-preparation-race.log` | `a89ffc4155ee63c11add6521d18d8fba8ec8259bf67f1eb7b13ab447c4905495` |
| `full-suite-1.log` | `1175349797a91059e0f92c7621fe81431114f404d2c83ab75e76d99d452d1ddf` |
| `full-suite-2.log` | `637f07171946d47466e0b6cb8a2b5e48b981e4f209a3d6ecf9451355f12dfa75` |
| `full-suite-3.log` | `3bc9e9d83e39d989b64ad7555a5b1b291f6d88a7fb27a7037126fee02068fb2f` |

The installed qualifier accepted its pinned settings/checker and failed closed
because the dedicated source-test provider credential is absent. No candidate
was executed and no review/test receipt was fabricated. Shell syntax and both
JSON settings passed validation. The staging supervisor remains inactive and
the active host configuration was not changed.

## Still required before activation

- Provision an approved dedicated model credential. Source testing expects
  `/var/lib/urnetwork/competition-source-tests/model.key`, root-owned 0400;
  the review image separately expects
  `/var/lib/urnetwork/competition-review/model.key` readable only by its
  remapped container UID, under a root-owned 0700 directory. Do not substitute
  Codex OAuth credentials or expose main vault mounts to candidate code.
- Install the review image/key configuration and prove actual Sol 6.1 max
  review and independent Sol 6.1 low test execution, including recovery and
  failure paths. Existing mocked-provider tests are not that live proof.
- Confirm main maintenance is over before control-plane writes. At 17:18 UTC,
  `/competition/info` still returned HTTP 503 and MinIO
  `172.28.208.177:23900` refused connections. Do not open a round on this evidence.
- Wire the exact release/settings pins into the preparation, qualification and
  supervisor configuration; retain genuine runtime identities and fenced
  readiness acknowledgments. This installation alone does not do that.
- Complete exact-image host/artifact/rebaseline qualification and the remaining
  scale/reference/seed calibration, then activate the scheduled production
  season without admitting submissions before October 28, 00:00 UTC.

Installing these control programs does not freeze continuously updated main API
software and does not alter the evaluated source commits or previous evidence.

## Frozen evaluator scripts installed separately

At 17:25 UTC the unmodified container-script directory from the clean frozen
server `67c87445abe6725b25b3df2a13af7eb99cd4f8ed` was installed under
`/usr/local/libexec/urnetwork/competition-evaluator-826569fd-67c87445/container`.
Every installed file compares byte-for-byte with that checkout. Directories and
shell programs are root-owned 0555; other files are root-owned 0444. Shell syntax
validation passed. No active config pin or running worker was changed.

| Frozen evaluator entry point | SHA-256 |
| --- | --- |
| `evaluator.sh` | `d55eac73bebd5a3be1ef01c0ad024633a3c71d01b481350386b06810e64feed2` |
| `build-submission.sh` | `460a6bb91f8ba20f818f4bcf8b245840aa796f34e9710edf5c359c7c4d4227de` |
| `prepare-evaluation-source.sh` | `af4e3c92ff1668a59b93aa6649293a06afaaf0bb707447ad92abac4e047c29a2` |

These scripts accompany evaluator image
`sha256:826569fdeb02f72ec4f24ace7c93b16b69a60e1875c31799c5a0217b402e0d6c`;
its embedded simulator/scorer SHA-256 is
`dd682292eba9d38f0fecd7b48ecdd1a96f942fe44fdbc44927bff472226d89ab`.
The newer main control binary above is not a replacement for that measured
simulator. Neither filesystem installation constitutes whole-worker or
authority-bearing exact-image qualification.
