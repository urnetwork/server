
We need to create a server/connect/sim-latency/main.go that spins up a fully working simulation environment on env local (similar to how test.sh uses local), with the following design goals. The tool will be used in an algorithmic competition to improve the `FindProviders2`, `Client`, `LocalUserNat`, and other algorithmic performance in the egress provider stack.


## Design Goals

1. a local load endpoint http server needs to spun up that serves fake site traffic via /, which returns a list of fake suburls to query. This will be the basis for the end to end speed test.
2. the local security rules need to be disabled since we will be testing on local servers
3. We need to use a fake country/region/city called sim (country code zz). All providers will use the same region. Each provider will use a unique fake ip from a testing subnet allocated in sim-latency init providers.yml
4. a fleet of N providers with randomized and dynamic latency, speed, packet loss, and reliability will need to be created. We will use a mixture distribution where on any parameter a provider can exist. The provider parameters will be specific by an input file providers.yml that has the weights and settings for each provider. There should be an sim-latency init command that generates the providers.yml file, so that all runs can lock in the same weights
5. the sim-latency run command should spin up the environment (exchange server, api, providers) in the local environment, and then spin up mean M test clients every minute (chosen every second as a poisson process). The test client should conenct to the fake site /, get the list of suburls, and continue to query each until there are none left. This simulates a real client deepening load where resources are loaded once they are discovered. The fake site should aim to have a mean depth of K so that the loading tree terminates. Put K in providers.yml
6. The sim-latency run will output per-request statistics: total time, time to first byte, throughput. It will print these stats to stdout. The only printing to stdout that simlatency does will be the performance stats. stderr can print other simulation state logs.
7. sim-latency/main.go will use docopt for the cli
8. `FindProviders2` will export anonymized matchmaking statistics to the site local dir (WARP_SITE I believe). Each time the server starts it will create a `WARP_SITE/<server instance id>` subdir and save local files there. There should be a server/stats/stats.go that allows appending local stats to a stream name. The local stats should be protobuf. The protocol definition should be server/stats/sample and build similar to connect/protocol. 
9. For `FindProviders2` we need to export one sample per call that is the the set of samples clients with weights in weighted order (from the sub sampling, before any filtering), and the final N chosen. We want to be able to trace back the inputs and output of each FindProviders2 call, so that we can see how the sampling, sorting, and ultimate selection are working. 
10. I want to enable the stats sampling in our main environment also. I'd like to save the stats to minio. Evaluate a pipeline where the stats gets saved to minio, and removed on a 7 day rolling window. I want to be able to export all the stats for the past 7 days into a tarball and publish it.

## Implementation goals

1. The tool assumes the local environment `server/local/run-local.sh` is running
2. Use minimal and elegant code following connect/CODESTYLE.md
3. The tool should use the SDK for providers and clients, and add any simulation overrides to the sdk as needed (e.g. disable security rules)
4. We will aim for a provider count around 100k and a clients per minute around 1000. 
5. The tool needs a clean README.md that guides competitors on how to get started.
6. Using the tool should be self-contained and easy


## Resolved design (as built)

The competitor-facing guide is `sim-latency/README.md`. This section records the
architecture and the load-bearing decisions.

### Layout

- `server/connect/sim-latency/` — the tool (docopt cli): `init` samples the
  mixture into `providers.yml`; `run` stands up the environment and drives load;
  `fleet` runs one provider shard against an existing run's services.
- `server/stats/` — a generic local protobuf stats subsystem: per-process
  instance id, `Append(stream, msg)` (bounded queue, drop-on-full, never
  blocks), zstd length-delimited segments under the site dir, id anonymization,
  and a minio uploader + tarball export. Protocol at `server/stats/sample/`
  (built like `connect/protocol` via a Makefile).
- Upstream hooks: `connect` `ClientStrategySettings.ExtraHeaders` + a ws/http
  dial hook; `server` `ip_overrides` in settings (subnet → location); SDK
  `SimProvider`/`SimClient` (headless provider and client with the simulation
  overrides).

### Key decisions

- **Services in-process, fleet shardable.** `run` hosts api + N exchange/connect
  hosts + the reliability pipeline in-process (self-contained, no child vault
  plumbing); the heavy provider fleet runs in-process by default or as
  `--fleet-shards` subprocesses (and can be added from other machines via
  `fleet`). Only the run process writes stdout (CSV); all logs are stderr.
- **Fake region via `ip_overrides`.** Providers/clients present unique
  forwarded-for addresses from testing subnets (`198.18.0.0/15`, `198.20.0.0/16`);
  the server maps those subnets to the `zz`/"Sim" region, so the real geolocation
  → location → reliability → score path runs unchanged. No mmdb changes.
- **Impairment under the provider ws** (not fake reporting), so the server's own
  latency/speed tests measure it; cheap inline model (no per-conn goroutine) for
  100k scale; a fleet loop modulates a base/degraded regime; churn drives the
  real reliability machinery.
- **`providers.yml` is explicit ground truth** — concrete per-provider entries
  (ids, ip, sampled conditions), so runs reproduce and exported samples join to
  it. Provider→network assignment is itself a mixture (mostly singleton + a tail
  of shared fleet networks) because reliability weight is network-share adjusted.
- **FindProviders2 sample export** (goal 9): one anonymized sample per call —
  the loaded pool in scaled-weight order + the chosen ids — captured off the hot
  path (best-effort, gated on stats enabled). Enabled in main on the api
  instances and shipped to minio; local runs write raw ids for ground-truth
  joins.
- **Goal 10 pipeline.** api ships finalized segments to minio (vault
  `minio.yml`, dedicated service account); retention is a bucket lifecycle rule
  (7-day expiry) + hard quota set by `xops/main/ansible/playbook-minio.yml` from
  vault `stats.yml`; `bringyourctl stats export-samples --days 7 --out stats.tgz`
  packs a self-describing tarball. Anonymization uses a FIXED vault hmac salt
  (global traceability, not reversible to production ids; rotation is a manual
  op only).

### The warm-up period (and why prewarm is required)

Providers are not selectable until their reliability weights clear the
`FindProviders2` gates. The binding gate is the 12-hour lookback ≥ 0.7: the
weight is `valid_blocks / full_window` (`reliabilityCoveredBlockCount`
normalizes by the full 720-block window), so from a cold start it needs ~500
valid blocks ≈ **8.4 hours of uptime** (the 60-min/0.95 gate needs ~57 min).
The score gate ALSO requires a completed latency+speed test (a provider missing
the speed test scores at the cutoff and is excluded).

No sim can wait 8.4h. `run` therefore has a configurable **warm-up** that brings
the market to a stable state before clients start: ramp (connect) → prewarm
(establish scores) → tests (latency/speed) → settle (pipeline propagates). All
tunable to be as fast as possible: `--ramp`, `--prewarm`, `--settle`,
`--pipeline-interval`, `--test-timeout`, `--announce-timeout`.

`provisionPrewarm` establishes the market directly: it materializes
`network_client_location_reliability` from the connected, tested fleet and then
writes the final `client_connection_reliability_score` rows with a passing
weight (1.0) for every score lookback — rather than backfilling raw reliability
blocks, which fought the running-window/shift/degraded maintenance. The
provider's quality/speed score still comes from its real tests, so ranking is
unaffected; only the reliability-history requirement is short-circuited. The
pipeline then runs in prewarmed mode (`Services.SetPrewarmed`): it keeps
refreshing the location reliabilities (so churn still gates selection) and
re-exports the redis samples, but does not recompute reliability scores (which
would wipe the prewarm). `--prewarm 0` restores the true cold start. Verified
end to end: providers become selectable in seconds and tunneled requests return
200.

### Impairment model note

Network impairment is applied under the provider websocket. Latency is charged
once per write BURST (the first write after an idle gap ≥ the latency), not per
frame — the tunnel streams a transfer as many small ws frames, and charging
one-way latency on each would collapse throughput to latency×frame-count (an
early bug produced ~800 B/s). Bandwidth is a per-write token bucket; loss is an
occasional retransmit-sized stall at burst start.
