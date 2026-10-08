# EVALUATION2 — final clean eval-48d baseline

The runbook and result record for the final **local directional** eval-48d
environment. It also carries forward the host-discipline requirements needed
when the same workflow is repeated on dedicated official hardware. It captures
everything learned from the 2026-07-30 through 2026-08-16 workstation and local
Linux campaigns.

Status: the 2026-08-16 eval-48d set is **complete: 20/20 consecutive
authenticated runs, zero exclusions, held-out A/A indistinguishable, and all
post-campaign verification gates passed**. It is still not the official floor:
the host exposes 24 logical CPUs, the build is locally modified, and no
independent-seed/reference-patch calibration or Macrocosmos acceptance exists.
The official floor must be measured fresh on the accepted production hardware.

## 0. Fix-first blockers

Do these before anything else:

- **`connect/trace.go:76` negative-WaitGroup teardown panic — FIXED
  2026-08-02**, verify the final build includes it. Root cause:
  `model.UpdateClientScores` registered `wg.Done` both as an in-closure
  defer and as a `HandleError` rescue handler; a redis panic on the export
  path double-counted the WaitGroup and the negative-counter panic inside
  the recovery killed the process (~2 in 3 runs at drain; post-window, so
  data survived but side-cars were lost). Fixes: the redundant handler
  removed at the caller (`network_client_location_model.go`), and both
  `HandleError` twins (`connect/trace.go`, `server/trace.go`) now contain
  rescue-handler panics — logged loudly, never fatal. Regression tests in
  `connect/trace_test.go`. (Containment does not repair a misused
  WaitGroup — stdlib corrupts the counter before panicking — it only
  prevents process death; the loud log is the signal to fix the caller.)
- **Build discipline.** The baseline is only valid across replicates of one
  build. Use a **clean commit** (no uncommitted changes — the side-car records
  `build_revision` + a modified flag, and `compare` warns across fingerprints).
  Record the revision in the campaign notes.
- **Stale `/etc/hosts` entries.** Remove any old lines for
  `local-pg.bringyour.com` / `local-redis.bringyour.com` that point anywhere
  other than the address `server/local/run-local.sh` manages (a dead on-link
  address blackholes dials; under load this stretched pg connects past their
  deadlines and killed runs).
- **Resident-contract cancellation race — FIXED 2026-08-16.** A synchronous
  `residentContractManager.HasActiveContract` cache miss could enter the DB
  after its manager context was canceled during drain. The resulting
  `context canceled` reached `connect.HandleError` as a frozen G5
  `Unexpected error` and invalidated the attempt. The manager now fails closed
  immediately after cancellation and selectively contains only lifecycle
  `Done` errors around contract-cache work; unexpected errors still re-panic.
  Focused normal and race tests cover the cancellation and re-panic behavior.

## 1. Hardware + OS requirements

- **48 GB RAM** (the environment's namesake budget; measured whole-stack peak
  is ~14–26 GB depending on revision — the margin is deliberate: a box near
  its limits measures its own scheduling noise).
- **≥ 10 performance cores.** The sim is CPU-bound before it is memory-bound;
  end-to-end egress degrades above ~2 k providers on 10 cores, which is why
  eval-48d is sized at 2 k.
- **File descriptors**: `ulimit -n` ≥ 1,048,576 (eval-48.sh raises it; verify
  the hard limit allows it).
- The kernel socket-buffer pool runs near its ceiling (~8 k established
  sockets; on macOS the mbuf 2 KB-cluster pool sits ~99% utilized): transient
  `ENOBUFS` during the connect ramp is **benign and expected**; it must not
  appear during the measured window.

## 2. Idle-host rules (hard requirements, not hygiene)

External CPU/disk pressure does not add mild noise — it breaks runs in
glaring ways (fail-rate blowouts, mid-run market collapse, warm-up wedges,
pg dial starvation). Before a campaign:

- **Pause backups.** On macOS: `sudo tmutil stopbackup`, and permanently
  exclude the Docker VM disk image
  (`sudo tmutil addexclusion -p ~/Library/Containers/com.docker.docker`) —
  its churn made every backup run for hours and each backup window corrupted
  or killed runs ("postgres unreachable under the connected fleet").
- **Expect Spotlight/media-analysis storms** (`mds_stores`,
  `corespotlightd`, `mediaanalysisd`) after large file churn; wait them out
  or disable indexing for the work volume. Combined storm CPU < 40% and no
  active backup is the validated quiet gate.
- **No foreground use** during the measured campaign; on AC power, lid open,
  wrapped in `caffeinate -i` (caffeinate does not prevent lid-close sleep).
- `server/local/run-local.sh` (postgres + redis) must stay up for the whole
  campaign — **its exit removes the /etc/hosts entries and the loopback
  alias**, which wedges in-flight runs and fast-fails subsequent ones. Run it
  in a session that survives (its own terminal/tmux).

## 3. Environment identity (eval-48d)

The environment is pinned by `eval-48.sh` (the constants block is the
definition). Current revision **eval-48d** uses eval-48c's scale, mixture, and
byte-identical workload, but pins warm clients in fixture order, assigns their
exchange hosts by stable pool index, and retries only missing identities up to
a small fixed bound. It therefore has a new binary/environment identity. The
fixture hash was reproduced with both Go 1.26.5 and 1.26.6; do not mix eval-48b
or pre-fix eval-48c artifacts into this baseline.

| parameter | value |
|---|---|
| providers | 2,000 · mixture v2 (residential 45%, mobile 25%, business-fiber 15% @ 50–250 Mbps caps 24–64, hosting 15% @ 200–600 Mbps caps 32–96) |
| seed | 48 |
| clients | 200-identity warm pool · 80 arrivals/min |
| site | two-tier bodies: web 4–512 KiB; download 2–6 MiB on 25% of pages (seed-48 tree: 37 pages, 7 download-tier, 33 MB full-crawl) |
| window | 30 m measured · ramp 1 m · prewarm 13 h (instant) · settle 1 m · hosts 4 · `--fleet-shards 4` · every run `--reset` · 70 s inter-run quiescence |
| service identity | `WARP_ENV=local`, `WARP_SERVICE=sim`, `WARP_BLOCK=sim`, `WARP_HOST=127.0.0.1` (the wrapper overrides inherited test labels) |
| throughput gate | ≥ 1 MiB (`results.go throughputMinBytes`) — samples only the download tier |
| canonical file | `eval-48g/providers-eval48d.yml`, sha256 `549ec41c033f344d6e0a6b1de82b404bb63d5a8dfb5861b6c4b6d55886cdace4` |

Verify on the eval box: `./eval-48.sh init` regenerates from the seed and
fails loudly on a sha mismatch. (`init` is deterministic and `compare` refuses
mismatched-sha runs.)

**Verdict rules v2** (in `metricDefs`/`compare`): primaries `ttfb_p95_ms` +
`throughput_p50_bytes_per_s`; guards = primaries + `fail_rate` +
`throughput_p05_bytes_per_s`. `throughput_p95` is reported, non-gating (on
earlier builds its fast tail was a run-level lottery, and the final eval-48d
campaign still measured 21.33% run CV). Re-promoting it would require a rules
decision backed by official-hardware evidence.

## 4. Procedure

```bash
cd server/connect/sim-latency
go build -o sim-latency .          # clean commit; record revision
go test -count=1 .                 # unit gate

./eval-48.sh init                  # generate + sha-verify the canonical file
./eval-48.sh run --duration 3m --meta eval-48g/smoke.run.json > eval-48g/smoke.csv \
                                   # smoke: expect 200/200 established, low fail

caffeinate -i ./eval-48.sh campaign 13    # sequential A/A replicates
./finalize-local-baseline.sh 20 2  # top up; summarize; held-out A/A; verify
```

- Target **k ≥ 20 clean same-seed replicates** for the Apex calibration gate.
  One replicate ≈ 36 min (~6 min warm-up + 30 m window); plan at least 13 h of
  quiet machine time, then extend the campaign if failures leave fewer than 20
  authenticated completions.
- The campaign loop is failure-tolerant: one bad replicate logs and skips; a
  wedged replicate is killed by the **55 m watchdog**; three consecutive
  failures abort (that means the environment itself is down). Progress lines
  stream to `eval-48g/campaign.log`.
- `finalize-local-baseline.sh` does not accept a merely elapsed campaign as
  complete. It extends the campaign until at least 20 completion markers exist,
  authenticates them, records incomplete attempts as exclusions, produces the
  Markdown/JSON/SVG reports, and runs two genuinely held-out A/A evaluations.
  Its final verifier will not compile or test while any simulator is active.
- Long unattended campaigns on a machine with background storms can use
  `eval-48g/storm-wait-resume.sh` (v5: relaunch-on-abort with cooldown) and
  `eval-48g/salvage-sidecar.sh` (reconstructs a side-car from csv + logged
  window when a post-window crash eats it — only needed while the trace.go
  blocker is unfixed). On properly prepared hardware neither should trigger.

## 5. Fail-closed criteria + investigation

Contamination is usually glaring rather than subtle, but outcome-dependent
quarantine would bias the measured noise. Keep every authenticated completion
unless a predeclared, artifact-backed infrastructure rule proves it invalid.
Use the set's fail-rate/row-count band and the summary's robust-outlier report
only to trigger investigation; never remove a completed run merely because its
score is inconvenient. Warm-up failures, interrupted runs, and malformed or
unauthenticated artifact chains fail closed and are inventoried as excluded
attempts. Signatures observed:

| signature | cause |
|---|---|
| fail 2–20×, rows −20–80%, ttfb inflated | CPU/disk storm during window |
| blocks degrading to 85–100% fail mid-run | market collapse under starvation |
| warm-up wedge (watchdog kill at 55 m) | starved timing-gated warm-up phases |
| fast exit-2 ~60 s (pg dial panic) | stack down / dead hosts entry / disk storm |

This local workflow has no signed infrastructure-exclusion record, so do not
move an authenticated completion to `eval-48g/runs/flagged/`; the summary fails
closed if it sees one there. Production may support such an exclusion only
after freezing a signed reason/evidence schema. In the normal workflow,
`eval-48.sh baseline` includes every run with a complete sidecar and its
authenticated completion marker; the audit refuses hidden gaps or duplicate
attempt identities.

## 6. Deliverables of the final evaluation

1. `eval-48g/baseline.json` — the noise floor for the exact recorded
   host/build identity (k ≥ 20 authenticated runs). It is official only when
   collected by the accepted production-hardware runbook and accompanied by
   the signed calibration manifest; a workstation artifact is directional.
2. `eval-48g/baseline-summary.json`, `.md`, and
   `eval-48g/baseline-stability.svg` — independently authenticated run
   identities, failure-ceiling Apex raw scores, noise/drift/convergence,
   per-run diagnostics, exclusions, decision thresholds, and local resource
   envelope. The JSON fingerprints RSS, host, and native-service telemetry;
   native PostgreSQL summed process RSS is explicitly observational and
   double-counts shared pages, and any early-run coverage gap is listed rather
   than represented as a zero measurement.
3. Drift check across the campaign: per-metric slope vs run start-time — the
   completed eval-48d set had no scored metric with |t| >= 2 and required no
   outcome-based exclusions.
4. Convergence: `sd_by_replicates` flat over the last few k;
   `min_detectable_delta_by_runs_per_side` published for m = 1..5.
5. README noise-floor table updated with the final local result; the stability
   plot artifact refreshed (per-run series +
   threshold lines for `ttfb_p95`, `throughput_p50`, `throughput_p05`).
6. `eval-48g/heldout-aa-compare.json` — the submission workflow sanity check:
   one A/A `compare` of two held-out runs against the baseline must return
   `indistinguishable`.
7. `eval-48g/postcampaign-verification.log` — deterministic artifact replay,
   invariant checks, tests, race detector, vet, and repository compile gate.

## 7. Final eval-48d results (2026-08-16, local — NOT the official floor)

Host `sille` (linux/amd64, 24 logical CPUs), frozen Go 1.26.5 simulator SHA-256
`62665d25290c9ee9e81434a542e1ccd959709eb35ccdc82fef511e33204c5b29`,
build revision `05c745050657fdd31f908d7a0e06ef4e26f636d8` with
`modified: true`. The post-fix campaign contains exactly 20 authenticated
attempts (`r001`–`r020`), all 200/200 clients established, and zero excluded
attempts. Earlier bring-up and pre-fix artifacts are archived separately.

| metric | mean +/- sd | cv | minimum detectable delta, 1 run/side | 3 runs/side |
|---|---|---:|---:|---:|
| Apex raw score | 23,823.31 +/- 696.30 ms | 2.923% | quarter-margin floor: 11.691% | not policy-qualified |
| `ttfb_p95_ms` (primary diagnostic) | 1,025.40 +/- 47.82 ms | 4.663% | 116.93 ms | 67.51 ms |
| `throughput_p50_bytes_per_s` (primary diagnostic) | 290.75 +/- 8.20 kB/s | 2.819% | 20.04 kB/s | 11.57 kB/s |
| `throughput_p05_bytes_per_s` (guard) | 84.99 +/- 2.35 kB/s | 2.765% | 5.75 kB/s | 3.32 kB/s |
| `fail_rate` (guard) | 1.154% +/- 0.114 pp | 9.901% | — | — |
| `total_p95_ms`, successful rows | 20,902.62 +/- 435.72 ms | 2.085% | — | — |
| `goodput_bytes_per_s` | 41.151 +/- 0.299 MB/s | 0.727% | — | — |
| `throughput_p95_bytes_per_s` (reported) | 11.258 +/- 2.401 MB/s | **21.330%** | — | — |

All scored-metric drift statistics were below |t|=2. Robust triage flagged
only TTFB-p95 values in `r004` and `r011`; both stay in the baseline because no
predeclared infrastructure rule invalidated either run. The three primary
diagnostic SD sequences satisfy the local last-five <=10% span heuristic. The
Apex raw-score SD sequence does not (22.26% span), and some between-run SDs are
below within-run block SE, so the noise-floor warning remains conservative.

The local scalar cannot support a single-run 1% takeover. Deterministic
median-of-R estimates remain far above 0.25% CV for all tested values: 2.863%
(R=1), 2.020% (R=3), 1.671% (R=5), and 1.431% (R=7). Official hardware,
independent seeds, and reference-patch separability must determine the actual
R and takeover margin.

Held-out `h001` and `h002` were collected after the baseline and both produced
authenticated artifact chains. Their comparison verdict is
`indistinguishable` at alpha 0.05; B-vs-A Apex raw score changed -0.653%, median
throughput +0.625%, and TTFB p95 -2.075%, with neither primary significant
after Holm correction.

The local resource envelope was stable: peak simulator RSS 14.97 GiB, mean
simulator CPU 10.23 logical-core equivalents, peak established sockets 26,051,
and zero swap in every run. Host and native PostgreSQL/Redis telemetry fully
covered all 20 windows. The matchmaking audit found 20,947 samples, zero empty
pools, and at least 99.46% window span per run.

See `eval-48g/baseline-summary.md`, `baseline-summary.json`,
`baseline-stability.svg`, `heldout-aa-compare.json`, and
`postcampaign-verification.log`. The verifier replayed the baseline/summary
twice byte-identically and passed artifact invariants, normal tests, race tests,
vet, repository compilation, shell syntax, Python syntax, and `git diff
--check` at 2026-08-16 18:01:01 UTC.

## 8. Historical directional results (2026-08-02, workstation)

M1 Max MacBook Pro 10c/64 GB, build `41e966fa`+wip, **k=14 clean of 21
attempts** over a 13.2 h daytime span; 7 flagged (contention), two side-cars
salvaged (post-window teardown panic). Floor:
`eval-48g/baseline-directional.json`; per-run plot: the "eval-48b baseline
stability" artifact.

| metric | mean ± sd | cv | min Δ @1 run/side | @3/side |
|---|---|---|---|---|
| `ttfb_p95_ms` (primary) | 1795 ± 112 ms | 6.2% | 280 ms (16%) | 162 ms (9%) |
| `throughput_p50` (primary) | 126.1 k ± 5.1 k B/s | 4.0% | 12.7 k (10%) | 7.3 k (5.8%) |
| `throughput_p05` (guard) | 43.4 k ± 3.7 k B/s | 8.5% | 9.3 k (21%) | 5.3 k (12%) |
| `fail_rate` (guard) | 0.58% ± 0.17 pp | 30% | 0.44 pp | 0.25 pp |
| `throughput_p95` (reported) | 495 k ± 32 k B/s | **6.4%** | — | — |

Reading: the environment design is validated. `throughput_p50` is dead flat
across 13 h (drift t = −0.1) and decidable at ~10% with a single evaluation
run; `ttfb_p95` decidable at ~16%/9% (1 run / 3 runs per side). No metric
drifts significantly, though `ttfb_p95` and `fail_rate` lean warm
(t = +2.08 vs critical 2.18) — ambient daytime load that idle final hardware
should remove, likely tightening ttfb's CV toward the ~5% seen in quiet
stretches. The previously-lottery `throughput_p95` stabilized to 6.4% CV on
this build (fix landed between builds; re-promotion to primary is a rules
decision pending confirmation on final hardware). Watch-item:
`throughput_p05` runs at 8.5% CV on this build (vs 2.2% pre-fix) — guard
thresholds are correspondingly looser.

## 9. Gotchas ledger (hard-won, host-portability)

- **GNU vs BSD userland**: the dev workstation's PATH serves GNU coreutils
  (`date`, `timeout`, `stat`) — BSD idioms (`date -j -f`, `stat -f`) fail,
  and a silently-empty `$(date -j …)` once fired a scheduled stop 7 h early.
  Scripts here use GNU `date -d`; on a BSD-userland host, adjust.
- **`nc` against a blackholed address can hang** regardless of `-w`; probe
  the stack via local state instead (loopback alias present + 5432 listener).
- **TERM is lifecycle-aware as of score artifact schema 2.** It cancels ramp,
  prewarm, settle, warm-client construction, and measurement promptly, then
  joins clients, fleet shards, services, and stats. The outer runner still uses
  TERM followed by KILL after its frozen grace as a containment boundary; a
  required KILL is an incomplete evaluation.
- **`pgrep -f` observer effect**: a monitoring command whose cmdline contains
  the watched pattern trips the campaign's already-active guard. Bracket the
  pattern (`sim-latency [r]un`) in any watcher.
- **Current identity and selection prerequisites**: JWT validation joins the
  persisted active network/user/device/client identity, shared fleet networks
  authenticate as their first fixture user's admin, and provider modes must be
  acknowledged server-side. The selection pipeline also fail-closes on fresh
  egress health/location evidence; simulation provisioning seeds deterministic
  passing evidence because there is no external prober in this harness.
- Ports/`TIME_WAIT` are not a concern at eval-48 scale (~37 TIME_WAIT
  between runs); the mbuf ceiling is (see §1).
