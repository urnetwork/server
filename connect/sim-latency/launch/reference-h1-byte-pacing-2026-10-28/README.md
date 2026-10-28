# H1 byte-pacing negative reference — unmeasured

Focused synthetic unit qualification passed: three independent LOW passes of
**8 baseline / 27 candidate tests**, zero skips or failures. This is a proposed
stronger regression reference, **not a measured slowdown, service-qualified
reference, native candidate binary or accepted calibration screen**. Publish
this bundle before subsequent service/native-pilot validation. It does not
authorize a seed draw, measurement or reuse of the old second committed seed.
The [bundle manifest](MANIFEST.sha256) pins the published source and QA evidence.

## Frozen product and separate tests

The baseline is server `a9e8e1a710b24c749e6499a0606938252c83e136`.
[source-lock.json](source-lock.json) records all ten frozen repository commits,
base image and the exact product/test source pins. Its original `qualified:false`
and `measured:false` fields are not a later acceptance receipt.

| Artifact | SHA-256 |
| --- | --- |
| [Product patch](product.patch) | `6f8c6f24c44a9627f053e58d4a59f845a0ba4b0b51e201f5cabddfb33b30629d` |
| [Qualification-only test patch](qualification-tests.patch) | `6ef291d4d3ea6ea4fba7600b9a1f8390b79cd13f845f5f0722adb39625623844` |
| [Qualification test source](qualification-tests.go.txt) | `76ab3398cb5a676a63030ab10f379d7436409062c57878accd157d9ced5db759` |
| [Authored reference-policy input, not native-scored bytes](patch-policy.json) | `5d78c808eff8d16eaa8f113656c9814dd1453acfd51bd891029a93bf4e4197cb` |
| [Ten-repository source lock](source-lock.json) | `87424d425b2b3fc867f28144a984a8624a381ee437c17d7f85d9fe5d658fb0bc` |

The scored patch changes only `connect/transport.go`, SHA-256 after application
`17b0245f96a841f8d41578d804a4be3b3abac4a39ae40ac5b68747d0bd4ba15e`.
It is **legacy server-relative**, not a mono-root patch. The reference policy
only appends that path to `allowed_paths`; `max_patch_bytes=262144`, forbidden
paths and absent `path_base` remain unchanged. This is not a broadening of the
public production policy. The test patch is separate and never enters a scored
candidate; `.go.txt` files here are evidence, not compiled package tests.

`patch-policy.json` preserves the **authored input bytes**, not the policy byte
pin for a native evaluator run. Do not replace or reserialize that `5d78c808…`
artifact or relabel any LOW source/test pins.

### Separate native-policy canonicalization follow-up

The published [scored policy](patch-policy.scored.json), SHA-256
`25e9c5b89041f35607b3a9542d13abd2649a9e99b804f59791780af19ab97861`,
is byte-exact `jq -S .` serialization of the authored input. The frozen evaluator
uses `jq -S '.patch_policy'` before hashing its native policy. Independent
readback confirmed byte equality to that projection, identical decoded semantics,
unchanged array order/limits/forbidden paths, and absent `path_base`.

**Only `25e9c5b8…` may bind the prospective native builds, their image keys,
public pilot and fresh protocol.** The authored `5d78c808…` remains static input
lineage, not an interchangeable native-policy hash. This reference-only policy
is not the public production policy, and the unchanged base image is policy-unbound.

[scored-policy-lineage.json](scored-policy-lineage.json), SHA-256
`6bf536b6362f1e30e551015ead5ac3ab5b7b88137b6af8fc5b6c33fbec80e860`,
pins the actual trusted release components and three **calculated expected**
native image keys. Component hashes and the read-only builder identity were
independently checked; the three public-input key calculations were recomputed.
They are not built image IDs, binary digests or measurement receipts. The raw
lineage's `prepared-not-published` status is its original creation-time observation,
not a claim of a later build. The original
[handoff](scored-policy-handoff.txt), SHA-256
`1be65a2a5affc536844da8e233a9b2b2b5d7f1c24a8bf273914d07ef33581fcc`,
also remains byte-exact. Native builds still require broader integration and
separate authorization; this follow-up ran no build, test, service, pilot or seed draw.

The exact default is one microsecond per emitted user-frame byte, in the
existing dedicated H1 writer, for both WebSocket and H1Plus framed encoding.
It adds no receive-callback wait, lock or worker. Bounded FIFO batch ownership,
overflow refusal, one original absolute write deadline, quiet cancellation and
post-success accounting remain explicit. Four MiB of such bytes adds
4.194304 seconds in the virtual arithmetic test; this is **not an observed
end-to-end regression or pilot measurement**.

## Focused QA and retained failure

[qualification.json](qualification.json) binds all six raw positive logs and
the controlled negative log. Each baseline pass ran the same eight existing
guards; each candidate pass added nineteen pacing guards, including nil-hook
production timers under `testing/synctest`, both encodings, cancellation,
deadline boundaries, ownership and a joined shutdown. Final package times were
1.296/1.303/1.302 seconds baseline and 1.394/1.388/1.384 seconds candidate.
They are test-package elapsed times, not performance scores.

The actual execution profile reported by the owning LOW runner and primary-agent
transcript was exact cached Go 1.27.3, `env -i`, `GOWORK=off`,
`GOTOOLCHAIN=local`, `GOPROXY=off`, normal checksum verification,
`-mod=readonly -race -p=2 -count=1 -timeout=5m -json`, CPUs 20,22,
`GOMAXPROCS=2` and `GOMEMLIMIT=2GiB` (**soft**, not a hard RSS cap), under a
ten-minute outer bound. Only checked-in local fixtures and private Home/Site
paths were supplied; the database guard stayed local and unattested, with no
real-vault opt-in. Raw Go JSON alone does not authenticate this compiler/profile.

The initial LOW candidate run stopped on a fixture data race; it was not a
product failure or a passing qualification. The original
[race log](qa/original-fixture-race.json), [old fixture](qa/failed-fixture.go.txt)
and [failure lineage](qa/failure-lineage.json) remain preserved. A live parent
slice/counter read was not happens-before a later timer-driven writer update;
virtual time and an earlier `synctest.Wait` did not provide that ordering.
The correction uses immutable channel events while live, a controlled I/O
release and explicit completion receipts before mutable snapshots. It does not
disable the race detector, skip tests or change the product patch.

The old-fixture overlay reproduced the expected `DATA RACE` and failing package
before the six fresh full passes. That negative is not counted as a passing
test. All runner source manifests before/after the negative and every positive
pass were byte-identical, SHA-256
`5e4695b3b8338fb2c548b0e529ac9ce1d5f960b4dcdcd62016a0f834e0c2efdd`.
The complete repeated manifests remain private rather than duplicating megabytes
here. Public raw logs contain synthetic test output and stack/provenance paths,
not campaign inputs or credentials.

## Remaining gates

Broader H1/H1Plus lifecycle, real deadline, forwarding, backpressure and ownership
integration with independently attested disposable services remains pending.
The native reference binary/image has not been built or pinned by this bundle.
The measured-direction gate still needs its own prospectively frozen public
fixed-seed R9/R9 pilot, excluded from held-out calibration; no favorable direction,
placeability or significance is inferred from these unit tests. A new held-out
screen needs the separately reviewed/qualified exact protocol and published
commitments before measurement. No automatic retry, deadline change, margin
reduction, gate waiver, late opening or activation is authorized here.

The [old failed screen](../FIRST-COMMITTED-SCREEN-2026-10-28.md), its controls and
the unstarted original second seed remain preserved. R9, G1–G6, significance,
the frozen 26.9% margin, better/no-op patch bytes and the base evaluator image
are unchanged by this reference preparation.
