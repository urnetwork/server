# Corrected Go 1.27.3 frontier — retained incomplete points

Exploratory public-seed evidence only: **not an official baseline, accepted
scale, variance calibration or significance threshold**. No incomplete metrics
in this directory may be substituted for an epoch control sample.

## Identity and workload

- Frozen server: `0d58d0d91b069c62bcfb39cd72166c26925b786a`.
- Evaluator image ID:
  `sha256:effe9ba5dc9cc337b4f250977c5f921ea42e4a336b30e3a5b54a9fd1bbe7f793`.
- Simulator/scorer SHA-256:
  `4993081b4076693f231ca2e5d3ee4468646c54d2d6cbc89a01fe325fccf57dbc`.
- Canonical ten-repository source lock:
  `37555f58bbf1235f10600f8c1de7dafeef356985f719f5e029fdb8c2e69c9b4a`.
- Actual Go 1.27.3; 15,000 providers, 200 clients, 80 arrivals/minute, quality
  window 2; four exchange hosts and four fleet shards; impairment enabled;
  public seed 20261024. Actual parent and four child arguments were inspected.
- Fresh exact image-extracted source, disposable PostgreSQL/Redis and trusted
  migrations. Direct synthetic config/local and vault/local leaves only;
  no main/all mounts. Ten evaluation CPUs and a 72 GiB runner limit.

## 15,000-provider impairment-on result

Exit **1** after **1,090 seconds (18m10s)**. All 200 clients were built and
validated before measurement. Matchmaking probe 2 exhausted its HTTP attempts;
shard 0 then missed its TERM deadline and was killed. The final sidecar records
`completion_state: incomplete`, `incomplete_code: matchmaking_probe_failed`,
167 measured-window rows and 62 failed rows. No completion marker or final
resource report exists. The CSV SHA-256 in the sidecar matches the retained CSV.

The readiness acknowledgment for all four shards precedes ramp and prewarm in
the retained log. The PostgreSQL/Redis service log contains no
`provide_key_pkey` duplicate errors. This verifies the observed corrections'
boundaries, **not** the cause of the remaining matchmaking/teardown failures.
Ten serialization errors and five user-request statement cancellations remain
visible in service logs; they must not be summarized as an error-free database
run. Measurement started at `2026-10-25T00:46:28.408Z`, approximately 16m35s
after the logged public start, with approximately 75 seconds spent validating
the built client pool. Phase-label times are inferred from adjacent timestamped
records; the sidecar's measurement start is exact.
Neither prior failed-source measurements nor this incomplete sample establish
the corrected source's baseline or usable scale.

Cleanup removed all owned containers and networks, leaving the original
BuildKit, local PostgreSQL and local Redis containers. No retry or additional
point was started before retaining this result. No builds, tests or other
scoring overlapped the attempt.

## 7,200-provider impairment-on result

The next, separately started point used the same image, source, seed, client
count, arrival rate, quality window, shard count, timings and impairment mode,
with a fresh workload and disposable services for **7,200 providers**.
Exit **1** after **606 seconds (10m6s)**: the initial matchmaking probe failed
during measurement setup, after all 200 clients were built and validated.
The CSV contains only its header (zero measured rows); the sidecar is
`incomplete` with `matchmaking_probe_failed`. No completion marker or final
resource report exists. Teardown did not report a shard TERM failure.

Readiness still preceded prewarm. The service log contains zero provide-key
duplicate errors, two serialization errors and six user-request cancellations.
The runner's last handler failure reports cancellation of a `FindProviders2`
request; this alone does not establish why the bounded discovery call failed.
The point establishes neither a passing scale nor a baseline. All owned
containers/networks were removed, leaving the original services; no retry or
additional point ran before this result was retained.

## Preserved bytes

`failed-p15000-on/` and `failed-p7200-on/` retain each final sidecar, CSV,
ID-map receipt, generated
workload and complete runner/service logs. Deterministic gzip headers (`-n`)
permit exact decompressed-byte verification. `controller/` contains the actual
executed scripts, including private paths for provenance rather than deployment.
`image-record.json` is the original image build receipt.

All regular files are covered by the enclosing `baseline/MANIFEST.sha256`.
Older datasets and their outcomes remain unchanged. Local source evidence and
qualification boundaries are described in
[ARTIFACTS-READY-SNAPSHOT-2026-10-25.md](../../launch/ARTIFACTS-READY-SNAPSHOT-2026-10-25.md).
