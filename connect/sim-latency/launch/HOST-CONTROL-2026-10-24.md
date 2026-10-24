# Inactive host-control release — October 24, 2026

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
