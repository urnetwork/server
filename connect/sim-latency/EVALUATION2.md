# EVALUATION2 — running the final clean eval-48b baseline

The runbook for standing up the **final evaluation environment** on the
dedicated evaluation hardware and measuring the competition's official noise
floor. It captures everything learned from the 2026-07-30 → 2026-08-02
workstation campaigns (eval-48a k=12, eval-48b bring-up, and the directional
set below) so the final run is clean on the first attempt.

Status: the 2026-08-02 workstation set is a **directional stability read**
(wip build, daytime contention) — it validates the environment design but is
not the official floor. The official floor is measured fresh on the final
hardware by this runbook.

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

## 1. Hardware + OS requirements

- **48 GB RAM** (the environment's namesake budget; measured whole-stack peak
  is ~14–26 GB depending on revision — the margin is deliberate: a box near
  its limits measures its own scheduling noise).
- **≥ 10 performance cores.** The sim is CPU-bound before it is memory-bound;
  end-to-end egress degrades above ~2 k providers on 10 cores, which is why
  eval-48b is sized at 2 k.
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

## 3. Environment identity (eval-48b)

The environment is pinned by `eval-48.sh` (the constants block is the
definition). Current revision **eval-48b**:

| parameter | value |
|---|---|
| providers | 2,000 · mixture v2 (residential 45%, mobile 25%, business-fiber 15% @ 50–250 Mbps caps 24–64, hosting 15% @ 200–600 Mbps caps 32–96) |
| seed | 48 |
| clients | 200-identity warm pool · 80 arrivals/min |
| site | two-tier bodies: web 4–512 KiB; download 2–6 MiB on 25% of pages (seed-48 tree: 37 pages, 7 download-tier, 33 MB full-crawl) |
| window | 30 m measured · ramp 1 m · prewarm 13 h (instant) · settle 1 m · hosts 4 · `--fleet-shards 4` · every run `--reset` |
| throughput gate | ≥ 1 MiB (`results.go throughputMinBytes`) — samples only the download tier |
| canonical file | `eval-48g/providers-eval48b.yml`, sha256 `7851a0d0c0d2c80c4c28f0ecd752305f11a87087cf16d7056ad5ea8f027dfd26` |

Verify on the eval box: `./eval-48.sh init` regenerates from the seed and
fails loudly on a sha mismatch. (`init` is deterministic; the sha above was
produced on arm64/darwin — confirm it reproduces on the final hardware the
first time, since `compare` refuses mismatched-sha runs.)

**Verdict rules v2** (in `metricDefs`/`compare`): primaries `ttfb_p95_ms` +
`throughput_p50_bytes_per_s`; guards = primaries + `fail_rate` +
`throughput_p05_bytes_per_s`. `throughput_p95` is reported, non-gating (on
the pre-fix build its fast tail was a run-level lottery; the 2026-08-02 build
measured it at ~7% CV — if that holds on the final hardware, re-promoting it
is a rules decision to revisit).

## 4. Procedure

```bash
cd server/connect/sim-latency
go build -o sim-latency .          # clean commit; record revision
go test -count=1 .                 # unit gate

./eval-48.sh init                  # generate + sha-verify the canonical file
./eval-48.sh run --duration 3m --meta eval-48g/smoke.run.json > eval-48g/smoke.csv \
                                   # smoke: expect 200/200 established, low fail

caffeinate -i ./eval-48.sh campaign 12    # sequential A/A replicates
./eval-48.sh baseline              # noise floor from every completed run
```

- Target **k ≥ 12 clean replicates** (15 preferred). One replicate ≈ 36 min
  (~6 min warm-up + 30 m window); plan ~9–10 h of quiet machine time.
- The campaign loop is failure-tolerant: one bad replicate logs and skips; a
  wedged replicate is killed by the **55 m watchdog**; three consecutive
  failures abort (that means the environment itself is down). Progress lines
  stream to `eval-48g/campaign.log`.
- Long unattended campaigns on a machine with background storms can use
  `eval-48g/storm-wait-resume.sh` (v5: relaunch-on-abort with cooldown) and
  `eval-48g/salvage-sidecar.sh` (reconstructs a side-car from csv + logged
  window when a post-window crash eats it — only needed while the trace.go
  blocker is unfixed). On properly prepared hardware neither should trigger.

## 5. Clean-run criteria + quarantine

Contamination is glaring, never subtle. Quarantine (move to
`eval-48g/runs/flagged/`) any run outside the set's own band; the workstation
heuristic was **fail_rate < 1% and rows_in_window > 50 k**, but recalibrate
the band from the first 3 quiet runs on the final hardware (fail-rate levels
are build- and host-dependent). Signatures observed:

| signature | cause |
|---|---|
| fail 2–20×, rows −20–80%, ttfb inflated | CPU/disk storm during window |
| blocks degrading to 85–100% fail mid-run | market collapse under starvation |
| warm-up wedge (watchdog kill at 55 m) | starved timing-gated warm-up phases |
| fast exit-2 ~60 s (pg dial panic) | stack down / dead hosts entry / disk storm |

The baseline must be computed from an explicit clean list if anything was
flagged: `./sim-latency baseline --runs r001.csv,r003.csv,... --out
eval-48g/baseline.json` (the `eval-48.sh baseline` subcommand includes every
run with a side-car).

## 6. Deliverables of the final evaluation

1. `eval-48g/baseline.json` — the official noise floor (k ≥ 12, one build,
   one host, all-clean).
2. Drift check across the campaign: per-metric slope vs run start-time —
   nothing should exceed |t| ≈ 2 (workstation sets showed no significant
   drift once contaminated runs were excluded).
3. Convergence: `sd_by_replicates` flat over the last few k;
   `min_detectable_delta_by_runs_per_side` published for m = 1..5.
4. README noise-floor table updated (mark this file's directional table
   superseded); the stability plot artifact refreshed (per-run series +
   threshold lines for `ttfb_p95`, `throughput_p50`, `throughput_p05`).
5. The submission workflow sanity check: one A/A `compare` of two held-out
   runs against the baseline must return `indistinguishable`.

## 7. Directional results (2026-08-02, workstation — NOT the official floor)

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

## 8. Gotchas ledger (hard-won, host-portability)

- **GNU vs BSD userland**: the dev workstation's PATH serves GNU coreutils
  (`date`, `timeout`, `stat`) — BSD idioms (`date -j -f`, `stat -f`) fail,
  and a silently-empty `$(date -j …)` once fired a scheduled stop 7 h early.
  Scripts here use GNU `date -d`; on a BSD-userland host, adjust.
- **`nc` against a blackholed address can hang** regardless of `-w`; probe
  the stack via local state instead (loopback alias present + 5432 listener).
- **Go signal handling queues TERM during warm-up phases** (`run.go` only
  drains the signal channel at phase boundaries) — a "stop now" needs TERM,
  a grace period, then KILL.
- **`pgrep -f` observer effect**: a monitoring command whose cmdline contains
  the watched pattern trips the campaign's already-active guard. Bracket the
  pattern (`sim-latency [r]un`) in any watcher.
- Ports/`TIME_WAIT` are not a concern at eval-48 scale (~37 TIME_WAIT
  between runs); the mbuf ceiling is (see §1).
