# Session performance harness

A local, reproducible load test of the session code: minting, the authentication check, revocation and client removal. It drives the real code paths at production-shaped rates against a disposable Redis Cluster and the local test database. It repeats the load gates of [REVOKE-OPERATIONS.md](../REVOKE-OPERATIONS.md) and [REVOKE-FINAL.md](../REVOKE-FINAL.md) §12.

It's a local measurement, not a fleet capacity claim. Per-operation Redis cost scales to production by node count; whole-run rates are what one machine drives.

## Running

```sh
# from the server directory, with ./local/run-local.sh up
SESSION_PERF_PROFILE=quick ./session/perf/run.sh      # about 10 minutes
./session/perf/run.sh                                  # standard, about 25 minutes
```

Settings are environment variables:

| Variable | Default | Meaning |
|---|---|---|
| `SESSION_PERF_PROFILE` | `standard` | `quick` or `standard` |
| `SESSION_PERF_OUT` | `temp/session-perf/<utc time>` | where `report.md`, `report.json` and `test.log` go |
| `SESSION_PERF_LABEL` | `branch@commit` | the report's title |
| `SESSION_PERF_PHASES` | all | a comma list of `unit,capacity,steady,storm,signout,outbox` |
| `SESSION_PERF_STEADY_SECONDS` | profile | the steady phase's length |
| `SESSION_PERF_RATES` | profile | per-load overrides, for example `api=4000,lease=3000` |
| `SESSION_PERF_LOCK_DIR` | none | a mkdir lock held while the run uses the test database |
| `SESSION_PERF_REDIS_NODES` | `3` | cluster masters, 3 to 6 |
| `SESSION_PERF_REDIS_PORT_BASE` | `26379` | the first node's client port |
| `SESSION_PERF_EXPECT_DB_HOST` | `10.213.0.1` | the launcher address the run insists on |

To compare two branches, run the same command in each branch's worktree and compare the two `report.md` files. The labels tell them apart.

## Safety

- **Redis.**
  - The run starts its own Redis Cluster in one container, `urnetwork-session-perf-redis`, and removes it on exit.
  - The client ports are published on the local launcher's dedicated address (`10.213.0.1`), never on loopback, where a production data tunnel listens.
  - `run.sh` refuses client or bus ports in 6379–6427, 5432 or 6432, an address that already answers, and an existing container of that name.
  - Before anything is written, `run.sh` and the test both check that the cluster has exactly the expected masters and no replicas, every node announces a non-loopback, non-tunnel endpoint, and every master holds no key.
  - The test process dials through the test environment's dial guard. `run.sh` adds the cluster's node endpoints to the attestation that `test-env.sh` exports, so the process can reach the test database, the test Redis lease and the cluster's nodes, and nothing else.
- **PostgreSQL.** Only through `test-env.sh`. The run stops unless its attested database endpoint and the launcher address are `SESSION_PERF_EXPECT_DB_HOST`. The test runs in its own temporary database (`server.DefaultTestEnv`), which is dropped at the end.
- **Data.** Every network, client and credential is synthetic and created by the run.

## What it drives

Every call goes through the same entry points production uses:

| Operation | Entry point |
|---|---|
| password sign-in | `session.MintNetworkSession` |
| auth-client with an API key (own session) or a network credential (inherited) | `model.AuthNetworkClient` |
| hosted proxy create | `model.AuthNetworkClient` with a proxy config |
| hosted device open | `session.MintHostedSession` |
| hosted derived child | `model.AuthNetworkClientFromParent` |
| refresh | `session.RenewSessionCredential` |
| code create and code login | `model.AuthCodeCreate`, `model.AuthCodeLogin` |
| API request authentication | `session.ClientSession.Auth`, on a bearer request |
| connect lease recheck | `session.ValidateByJwtState` |
| session hint read | `session.NetworkSessionRevision` |
| Sessions list | `session.GetNetworkSessions` |
| single revoke, sign out others | `session.RevokeNetworkSession`, `session.RevokeOtherNetworkSessions` |
| session client cleanup | `model.CleanupSessionOperation` |
| sign-out-all (the password reset's retirement) | `model.PlanNetworkSignOut`, `model.CommitNetworkSignOut` |
| remove-client | `model.RemoveNetworkClient` |
| the maintenance task | `session.SweepSessionIndexShard`, `ExpireSessionReceipts`, `RepairSessionIndexes`, `model.RecoverSessionOperations`, `session.ObserveSessionMaintenance`, with the task's budgets |

The run switches `session_creation_enabled` and `revocation_record_enabled` on, and lifts the hosted proxy caps.

## Phases

1. **unit.** Each operation runs alone for a fixed count. The report gives its latency and the cluster's Redis CPU, commands, EVALs and memory per call.
2. **capacity.** This repeats the REVOKE-OPERATIONS maximum-state profile.
   - A network is filled to 1,000 live sign-ins, and each is used once through the API, which writes its last-use key and record projection.
   - Revoke-others cycles fill the retained set to 1,000 live and 9,000 markers.
   - It measures list, hint reads and bulk revoke at both states, and what a new mint gets at each cap.
   - A second network is filled to the new cap of 5,000 client-bound sessions (hosted proxies). If the mint path refuses at its live cap, as main does at 1,000, the rest are written with the mint script's key layout and bound in SQL; the report counts these as synthetic.
3. **steady.** Every workload at its production shape at once, for the profile's duration:
   - auth-client (half API key, half network credential) and removal after a lifetime;
   - hosted children and their removal;
   - refresh and sign-in;
   - one hot network creating and removing hosted proxies at 7,300 a day, compressed;
   - a code fleet at 96 redemptions a day, compressed;
   - API authentication and lease rechecks over a working set of clients and sign-ins;
   - hint reads of small networks and of the 5,000-session network;
   - lists, single revokes, and the maintenance task every 30 s.

   A time series records Redis cores, commands, memory, index outbox rows and the hot network's live sessions every 10 s.
4. **storm.** Sign out other sessions on the 5,000-session network, then its client cleanup, while background lease checks run. Checks are split by whether their record key lives on the revoked network's node. The slowlog gives the node time each script holds.
5. **signout** (standard profile). Sign-out-all on a second 5,000-session network, then probes whether its hosted proxies still check and re-mint (decision 4).
6. **outbox.** The index repair and its backlog gauges, timed at seeded outbox backlogs.

## Rates

Production shapes, from Main on 2026-11-18:

| Shape | Production | standard | quick |
|---|---|---|---|
| auth-client mints | ~150/s | 150/s | 40/s |
| hosted derived child mints | ~223/s | 223/s | 40/s |
| refresh | ~6/s | 6/s | 6/s |
| password sign-ins | ~1.6/s | 1.6/s | 1.6/s |
| hot proxy network | 7,300/day | ×120 (10/s) | ×120 |
| code fleet | 96/day | ×600 | ×600 |
| API authentication | ~10k/s fleet | 2,500/s | 800/s |
| lease rechecks | ~14.7k/s fleet record checks | 2,500/s | 800/s |
| hint reads | ~5.3k/s fleet | 300/s | 100/s |

Each load is open-loop. A load that can't keep up counts dropped arrivals instead of slowing down, and the report gives target and achieved rates.

## Gates

`report.md` starts with the gates. Budget gates pass when every call finishes without failure inside its storage budget: two seconds for an authentication storage call, and two seconds for all attempts of a list or bulk revoke. The reference column compares with the 2026-10-09 local profile in REVOKE-OPERATIONS.md.

| Gate | Passes when |
|---|---|
| G1 max-state memory | 1,000 live + 9,000 markers take at most 1.25× the reference 2,717,332 B |
| G2–G6 list and bulk revoke at the live cap, the maximum state and the new cap | within budget, no failure |
| G7 bulk revoke at the new cap | within budget. G7b warns when one script holds a node more than 100 ms. G7c warns when checks on that node pass 100 ms or 10× their earlier p99 |
| G8 sign-out-all at the new cap | completes without an enforcement error |
| G9, G10 lease rechecks and API authentication under steady load | no failure, within budget; warn above 10× the sequential reference p99 |
| G11 mints, removals, lists and hints under steady load | no failure except designed refusals |
| G12 capacity refusals | none at production shapes (decision 6: the cap is not enforced) |
| G13 eviction | none |
| G14 index outbox | the backlog doesn't grow during the steady phase |
| G15 index repair at a backlog | repair within its 1 s stage budget; warn when the backlog gauges miss their 100 ms budget |
| G16 revocation and removal | every revoked or removed credential refused at its next check |
| G17 decision 4 | hosted proxies still check and re-mint after sign-out-all |
| G18 stream lease fan-out | not run here; connect's tests own it |
