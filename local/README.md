# Local environment

Spins up the postgres + redis instances the `server` tests and local dev need,
on a dedicated docker network, and points the well-known hostnames at them.

## Usage

```sh
cd server/local
./run-local.sh
```

This runs in the foreground. In another terminal, run the tests:

```sh
cd server
./test.sh -run TestName
```

To run a local service or tool against the stack, start it through its
`run-local.sh`, which attests the launcher first (see
[Public DNS also answered 127.0.0.1](#public-dns-also-answered-127001)).
That includes `bringyourctl/run-local.sh`, so `./run-local.sh db migrate` now
needs the launcher too:

```sh
cd server/cli/api
./run-local.sh
```

Press `Ctrl-C` in the `run-local.sh` terminal to stop the containers, restore
`/etc/hosts`, and remove the loopback alias.

## What it does

- Starts `postgres:18` and `redis:8-alpine` via [`docker-compose.yml`](docker-compose.yml)
  on a dedicated bridge network (`urnetwork-local`, subnet `10.213.1.0/24`).
- Postgres provisions the role + database from the selected local `pg.yml` via
  [`postgres/initdb/01-init-app-db.sh`](postgres/initdb/01-init-app-db.sh). The
  role gets `CREATEDB` because the test harness creates/drops a database per test.
  The cluster is initialized with `LOCALE=en_US.UTF-8` so the harness's
  `CREATE DATABASE ... LOCALE='en_US.UTF-8'` succeeds.
- While the script runs it adds a **dedicated loopback alias** (`LOCAL_HOST_IP`,
  default `10.213.0.1`) to the loopback interface, publishes the DB ports on it,
  and maps the hostnames to it exactly once in `/etc/hosts`:

  ```
  10.213.0.1  local-pg.bringyour.com
  10.213.0.1  local-redis.bringyour.com
  ```
- After both containers are healthy and both published ports are reachable from
  the host, atomically publishes `/tmp/urnetwork-server-run-local.lock/ready`.
  That attestation binds the lock's owner token to the selected IP, hostnames,
  and ports. The launcher re-execs before acquiring resources, preserving its
  PID and flags while adding a random 128-bit nonce to its arguments. Its `v2`
  owner token binds that nonce to the PID. `test-env.sh` requires a live,
  non-zombie process with that exact nonce field in `ps`'s rendered command,
  the complete record, and the two unique launcher-managed mappings before it
  probes either service. Matching files or a reused PID alone cannot establish
  readiness.

## Why not 127.0.0.1

Tests create and **drop** databases. A tunnel/port-forward to a real (prod)
database commonly listens on `127.0.0.1:5432`, so if the local hostnames ever
resolved to `127.0.0.1`, a test run could wipe prod. This setup therefore never
uses `127.0.0.1`: it binds to a distinct dedicated address instead. That alone
does not make the stack's absence harmless; see
[Public DNS also answered 127.0.0.1](#public-dns-also-answered-127001).
The script refuses to run if `LOCAL_HOST_IP` is set to `127.0.0.1`, either
hostname already has any unmanaged mapping, a legacy managed block remains, or
another launcher owns `/tmp/urnetwork-server-run-local.lock`. It never tries a
second resolved address and never silently rewrites an operator-owned entry.
An alias that happens to reach a database is not sufficient proof: it may be a
legacy VM route, an independently managed tunnel, or state from a launcher that
has not finished starting.

### Public DNS also answered 127.0.0.1

The launcher's `/etc/hosts` block is not the only answer for these names. Until
2026-10-14 public DNS resolved both `local-pg.bringyour.com` and
`local-redis.bringyour.com` to `127.0.0.1`. Those records are deleted, but a
cached answer, a hosts-file edit or a `localhost` default in local tooling can
still put a client on loopback. While the launcher runs, its hosts block wins.
When it exits it restores `/etc/hosts`, and a client that resolves either name
again reaches whatever answers next. On a machine that also runs the production
data tunnel (`xops/bin/by-patch-main-data`, which forwards `5432`, `6432` and
the Redis cluster ports `6379`-`6427` on loopback), that is production. A DB
test run that outlived its launcher has redialed `local-redis.bringyour.com`
this way and reached a production Redis cluster. What kept its commands from
executing was only that the cluster rejected `SELECT` in cluster mode and
refused a cross-slot `EVAL`.

A preflight alone cannot prevent this: it attests the environment once, while
the clients resolve on every dial. So every `WARP_ENV=local` process is pinned
to its launcher's attested endpoints for its whole run:

- `test-env.sh` (tests) and `local/local-env.sh` (every other local process)
  validate the launcher's lock, live owner, readiness record and hosts block,
  resolve both names with the host resolver (`getent` or `dscacheutil`), and
  refuse unless each maps only to the launcher's address, in particular never
  to loopback. They then probe that address literal and export the validated
  endpoints as `WARP_TEST_ENV_ATTESTED_ADDRS` (`10.213.0.1:5432,10.213.0.1:6379`
  by default). Any failure leaves the variable unset. `local-env.sh` runs the
  same preflight as `test-env.sh` but keeps the caller's `WARP_SERVICE`,
  `WARP_BLOCK`, `WARP_VERSION` and `WARP_DOMAIN`. It leaves `test-env.sh`'s
  `WARP_VAULT_HOME` and `WARP_CONFIG_HOME` exported, so the service reads the
  vault and config the preflight selected: an explicit value, then
  `WARP_HOME`'s, a sibling checkout's, and finally the checked-in testdata.
  The preflight checks that the vault's `pg.yml` and `redis.yml` name the
  launcher's endpoints, which does not prove that the launcher started from
  that vault ([DB tests and the test vault guard](#db-tests-and-the-test-vault-guard)).
- The `cli/*/run-local.sh` services, `bringyourctl/run-local.sh` and
  `connect/sim-latency/run-local.sh` (for `run`, `fleet`, `reset` and a
  measuring `baseline`) source `local/local-env.sh` before they start the
  process, and exit without starting it when the launcher is not up.
- Every `WARP_ENV=local` process, test or not, pins every PostgreSQL and Redis
  socket to those endpoints
  ([`test_env_dial_guard.go`](../test_env_dial_guard.go)). Each dial resolves
  once, refuses any address outside the attestation with an error naming the
  host and the address it resolved to, and connects only to the checked
  literal. The pools, the fixture's own probe, lease and owner connections,
  child processes, and every go-redis client a server test builds dial this
  way; `TestRedisClientsDialThroughExplicitDialers` fails for a client left on
  go-redis's own dialer. So does every PostgreSQL connection a tool or fixture
  configures itself (`server.ConfigureLocalPgDial`: the `strecovery` census and
  `stmonitor` reads, and the `stmonitor` and heartbeat fixtures). A Main
  operator tool run with `WARP_ENV=local` in its environment is therefore
  refused too; run it with `WARP_ENV` unset or `main`.
- A test that points a pool at an in-process loopback proxy admits that
  proxy's own live listener with `server.Testing_AdmitTestEnvLoopbackListener`
  (its exact `ip:port`, while it stays open, until the test ends) and dials the
  proxy's upstream with `server.Testing_DialTestEnv`, as the model fixtures do.
- With the stack down, an attested process still dials only the attested
  address. Without the launcher's loopback alias that address leaves the
  machine by the default route, so the dial normally times out rather than
  being refused, and never falls through to loopback. Restart the launcher.
- A local process without the attestation refuses every dial, with an error
  that says to start `./local/run-local.sh` and restart the process through its
  `run-local.sh` (or source `test-env.sh` for tests). That includes a process
  started directly with `go run .`, one whose `main` sets `WARP_ENV=local`
  itself (sim-latency's defaults: a binary other than a test that starts
  without `WARP_ENV` chooses its guard at its first PostgreSQL or Redis use
  rather than at startup), and a shell that sourced an older `test-env.sh`. A
  test binary, and any process that starts with `WARP_ENV` set, chooses at
  startup, so a process that starts with another `WARP_ENV`, production
  included, locks in no guard. Integration fixtures (`TestEnv`) refuse to start
  without the attestation, so DB tests run only through `./test.sh` or a
  sourced `test-env.sh`.
- `sim-latency reset` and `run --reset` truncate tables with `CASCADE` and run
  `FLUSHALL` on logical db 0, which sends no `SELECT`. Before either sends
  anything, `server.RequireAttestedLocalEndpoints` checks every pool's vault
  authority (`pg.yml`, `pg_maintenance.yml` when present, and `redis.yml`):
  each must resolve only to attested endpoints, never an admitted test
  listener, and none may be loopback outside the private portable root.
- Loopback is admitted only when the attestation itself is loopback, which
  only the private portable root below produces (both portable flags and
  `WARP_TEST_ENV_PORTABLE_ROOT`), with its own `127.0.0.1` authorities, and
  never on a tunnel port (`5432`, `6432`, `6379`-`6427`).
- The sim-latency evaluator container has no launcher. Its entrypoint resolves
  the job's own `postgres` and `redis` Compose services once, requires exactly
  one address that is not loopback for each, and exports that attestation for
  the migrator and the runner.
- `WARP_ENV=local` matches in any case and with surrounding space, since a
  case-insensitive filesystem resolves `vault/Local` to `vault/local`. With
  `WARP_ENV` unset, a `WARP_VAULT_HOME` that names a local vault directory
  itself (`…/vault/local`) is local too, because the resolver reads that
  directory's own resources for any environment.
- Processes in any other `WARP_ENV`, production included, get no guard; their
  dial paths are unchanged.

On Docker Desktop / macOS the container IPs on the docker network are not
routable from the host, so host access (where `go test` runs) goes through the
published ports on the loopback alias rather than the container's network IP.
On Linux, Docker and host networking changes run through `sudo`; on macOS,
Docker Desktop runs as the current user. The tests run as the current user.

## DB tests and the test vault guard

A test binary reads a vault only below a fixture root: the release gate's
private portable root, the checked-in `testdata`, or the temp directory (or
`GOTMPDIR`) where a test writes its own. Any other vault, such as
`$WARP_HOME/vault` or a sibling `vault` checkout, can hold live secrets, so its
resources read as absent (`refuseNonFixtureVault` in [`env.go`](../env.go)).

The launcher, though, starts PostgreSQL and Redis with the credentials in its
selected vault's `local/pg.yml` and `local/redis.yml`, and on a developer
machine that is usually such a vault. So in managed-local mode, `test-env.sh`
selects a vault by the launcher's own rules, checks the authorities of that
vault's `pg.yml` and `redis.yml` against the launcher's readiness record, and
exports the vault as `WARP_TEST_ENV_LAUNCHER_VAULT_HOME`. Neither can prove
that the launcher started from that vault, only that the files name its
endpoints; credentials from another vault fail to authenticate. While
`WARP_ENV=local` and the vault home is that vault, a test binary
([`env_test_vault.go`](../env_test_vault.go)):

- reads `pg.yml`, `pg_maintenance.yml` and `redis.yml` only from the vault's
  `local` directory, which must be a real directory, and each only when:
  - the launcher lock holds a v2 owner, and its readiness record is complete
    and names that owner, a dedicated non-loopback address and the endpoint
    that the selected file names;
  - the process's dial guard, chosen at startup from the environment
    `test-env.sh` exports, attests the record's address with both its ports;
- reads every other resource from `testdata/vault`;
- reads nothing else of that vault: not its root or `all` directory, no other
  environment such as `main`, and no other `local` resource.

Every file a test binary reads from a vault must resolve, through any links,
inside the scope it was admitted for: a fixture vault's own root, the
checked-in fixture, or the launcher vault's `local` directory. A vault home is
judged cleaned, as the resolver opens it, and a resource path that climbs out
of its vault with `..` is refused.

The readiness record is read again at each lookup of those resources, so a
launcher that withdraws readiness stops the reads at once. A failure refuses
with an error that names what is wrong, never a credential or a value from the
refused file. The suite proxy and the portable services have no launcher, so
`test-env.sh` names no launcher vault for them, and a test reads their vault
only if it is a fixture root.

`URNETWORK_TEST_REAL_VAULT=1` still lets a test binary read any vault, as
before and without the containment above, but DB tests do not use it, and they
do not run by cherry-picking a change onto a base from before the guard.

### Running a DB test

1. Start `./local/run-local.sh` in its own terminal, wait for `Local environment
   is up`, and keep it in the foreground for the whole run.
2. Check that both service names resolve only to the launcher's address
   (`10.213.0.1` by default), never to `127.0.0.1`:

   ```sh
   dscacheutil -q host -a name local-pg.bringyour.com      # macOS
   dscacheutil -q host -a name local-redis.bringyour.com
   getent ahosts local-pg.bringyour.com                    # Linux
   getent ahosts local-redis.bringyour.com
   ```

   `test-env.sh` checks this as well and refuses loopback.
3. From `server/`, run the test with `test.sh`, which first runs its
   contract-admission gate, or source `test-env.sh` in Bash for one targeted
   test. Sourcing it into zsh leaves `WARP_ENV` unset, so wrap it in `bash -c`:

   ```sh
   ./test.sh -run '^TestName$'
   bash -c 'source ./test-env.sh && cd model && go test -count=1 -run "^TestName$" .'
   ```

4. Stop the run at once on a `cluster mode` error, a refused connection to
   `10.213.0.1`, or any dial to `127.0.0.1`, `::1` or `localhost`: the run is not
   reaching the local stack.

Do not set `URNETWORK_TEST_REAL_VAULT`, and do not edit, copy or remove the
launcher's lock or vault files to make a test run.

## Flags

| Flag | Effect |
| --- | --- |
| `--fresh` | Wipe the postgres data volume first (re-runs DB init). |
| `--keep-up` | Leave the containers running after the script exits. |

## Configuration (env overrides)

| Var | Default | Purpose |
| --- | --- | --- |
| `LOCAL_HOST_IP` | `10.213.0.1` | Loopback-alias IP the hostnames resolve to (must not be `127.0.0.1`). |
| `LOCAL_DOCKER_SUBNET` | `10.213.1.0/24` | Subnet for the `urnetwork-local` docker network. |

## Harness readiness and portable services

Keep `run-local.sh` in the foreground for the entire test or service run. On
exit it withdraws the readiness attestation before changing hosts, listeners,
or the loopback alias. It removes readiness only when both the lock and
attestation still contain its owner token; ambiguous state is retained for
inspection and future preflights fail closed. A test or service that outlives
it keeps its attestation, so every later dial can reach only the attested
address the launcher withdrew; it fails instead of falling through.

The launcher defers `HUP`, `INT`, and `TERM` during lock acquisition and ignores
further signals once cleanup starts, so an ordinary interruption cannot strand
the lock between directory creation and owner publication, or between owner
removal and directory removal. `SIGKILL`, host failure, and filesystem errors
can still leave incomplete state.

Readiness validation rejects an absent owner or an old `v1` token even when
the containers are healthy. This rejection does not authorize stale-lock
reclamation: retain the residual state and follow the ownership checks below.

An empty `/tmp/urnetwork-server-run-local.lock` is not proof that the services
are unowned. For recovery, first prevent concurrent launcher starts and test
runs, confirm that the original launcher and its cleanup have ended, and retain
the lock metadata plus the current hosts, interface, and exact Compose-service
identities. Recover only the residual mappings, alias, and containers whose
ownership is established; preserve any ambiguous or unrelated state. Once the
lock is confirmed abandoned and still empty, use
`rmdir /tmp/urnetwork-server-run-local.lock`, which refuses unexpected contents.
Do not recursively delete it or infer abandonment from a missing owner record
or a PID alone. Start a fresh foreground launcher and wait for `Local
environment is up` before resuming tests.

An isolated portable environment may provision its own disposable PostgreSQL
and Redis services instead of using this launcher. That workflow must select
the checked-in resources and opt out of launcher ownership explicitly:

```sh
export WARP_TEST_ENV_USE_PORTABLE_RESOURCES=1
export WARP_TEST_ENV_ALLOW_UNMANAGED_PORTABLE_SERVICES=1
source ./test-env.sh
```

The escape skips only the launcher attestation and managed-mapping checks; the
resource-authority checks and first-attempt service probes still run. Each
service name is resolved once and must resolve to exactly one non-loopback
address, which is then probed and attested for the whole run. A name that
resolves to loopback is refused, because public DNS answered the local service
names that way, and so is a `127.0.0.1` literal: loopback services need the
private portable root (`WARP_TEST_ENV_PORTABLE_ROOT`) with explicit `127.0.0.1`
authorities, as `local/release-gate-services.sh` sets. No mode accepts a
loopback endpoint on a production data tunnel port (`5432`, `6432`,
`6379`-`6427`). Do not use the escape to bless developer-machine aliases or a
legacy local VM route.

### Repository-owned suite proxy

When the exact `urnetwork-local-pg` and `urnetwork-local-redis` Compose services
are already healthy but the interactive launcher cannot be restarted, the
suite harness can use short-lived direct proxies without sharing that
launcher's resolver or loopback ownership. Select an IPv4 address already
assigned to a non-loopback host interface and an absolute, nonexistent private
state path:

```sh
cd server
export SUITE_PROXY_HOST_IP=192.0.2.44 # replace with an address assigned to this host
export WARP_TEST_ENV_SUITE_PROXY_STATE_DIR=/tmp/urnetwork-server-suite-proxy.$USER
./local/run-suite-proxy.sh
```

Keep that Bash process in the foreground. In a second terminal, select the same
state directory and explicit complete resource repositories before running the
suite:

```sh
cd server
export WARP_TEST_ENV_SUITE_PROXY_STATE_DIR=/tmp/urnetwork-server-suite-proxy.$USER
export WARP_VAULT_HOME=/absolute/path/to/vault
export WARP_CONFIG_HOME=/absolute/path/to/config
./test.sh -run TestName
```

`test-env.sh` derives both `BRINGYOUR_*_HOSTNAME` values from the attested
direct IP (and rejects conflicting pre-set values), expands the vault resource
authorities, and requires their host and port to exactly match the attestation.
Before any service probe or database test starts, it validates the checked-in
`suite-resource-manifest.txt`, resolving every entry from the explicit root,
then its `local` directory, then its `all` directory. The exact local full-suite
boundary is:

- vault: `auth.yml`, `brevo.yml`, `circle.yml`, `client.yml`, `coinbase.yml`,
  `helius.yml`, `jwt.yml`, `jwt-local-evaluator.pem`,
  `password.yml`, `pg.yml`, `proxy.yml`, `redis.yml`, `services.yml`, `st.yml`,
  `stripe.yml`, `wireguard.yml`, and `x402.yml`, plus `tls` certificate/key
  pairs for `ur.network`, `bringyour.com`, `main-connect.ur.network`, and
  `main-connect.bringyour.com`; each pair must be colocated at one direct or
  recursively versioned resolver location;
- config: `apple_roots.pem`, `brevo.yml`, `db.yml`, `email.yml`,
  `mmdb/places.yml`, `pro.yml`, `redis.yml`, `settings.yml`, `subsidy.yml`,
  and `tls.yml`.

`mmdb/places.yml` is the location seeder's place list, which ships in the dated
directory of the GeoLite2 database it is exported from
(`all/mmdb/<date>/places.yml`). A config entry may therefore be a relative
path, and it resolves through semantic-version directories at every level, as
the Go resolver does; no path element may begin with a dot.

An incomplete explicit resource checkout is rejected fail closed.
The underlying resolver orders certificate and key paths independently,
checking direct root/local/all files before recursively descending
semantic-version directories. Transport TLS selects the first common directory
from those ordered results and never combines generations. Suite preflight is
intentionally stricter: every resolver-visible location containing either half
of a required pair must contain both readable files. It therefore rejects
partial rotations even when runtime could fall back to another complete pair.
It also checks all parseable version directory aliases, including aliases that
normalize to the same resolver version, instead of relying on map collision
order.
If a private temporary vault root overrides only `pg.yml`, it must preserve both
the source vault's `local` and `all` resolver scopes; linking only `local` hides
the versioned TLS tree from `WARP_VAULT_HOME` and is rejected by preflight.
Suite-proxy mode is mutually exclusive with managed-local test paths,
`WARP_TEST_ENV_USE_PORTABLE_RESOURCES`, and
`WARP_TEST_ENV_ALLOW_UNMANAGED_PORTABLE_SERVICES`. It never guesses a vault or
config root.

The foreground helper verifies the upstream containers' immutable IDs,
canonical names, health, Compose project/service labels, and the exact
`urnetwork-local` network. It resolves `alpine:3.22` to a content-addressed
image ID, then creates only `urnetwork-suite-proxy-pg` and
`urnetwork-suite-proxy-redis`, with labels binding their owner process instance,
challenge token, generation, image, network, service, and upstream. Each runs a
forking `socat` listener, so concurrent test connections are not serialized.
Only after PostgreSQL answers a bounded SSLRequest and Redis answers a bounded
PING does it publish its fixed-format, non-executable readiness record. The
helper continuously rechecks every identity, attachment, binding, and protocol
endpoint.

On exit, readiness is withdrawn before containers are touched. A proxy is
removed only by immutable ID after all ownership labels still match; a missing,
replaced, or malformed object, a Docker transport error, and any foreign state
are retained fail closed for inspection. Absence requires a successful daemon
query for the full immutable ID. The helper never edits `/etc/hosts`, adds an address, changes
the upstream containers/network, or signals another launcher. It may pull the
selected Alpine tag into the local Docker image cache, then runs its resolved
content-addressed image ID. Do not synthesize or edit
the private owner/readiness files.

These shell helpers require Bash 3.2 or newer. Invoke their shebang entrypoints
from zsh; do not source `run-local-state.sh` directly into stock zsh.

## Notes

- Unless the portable-resource override below is set, an explicit
  `WARP_VAULT_HOME` is authoritative. Otherwise the launcher uses
  `WARP_HOME/vault`, a sibling `vault` checkout, or finally the checked-in
  `testdata/vault` fixture. The fallback credentials are public and throwaway;
  no production secret is stored in this repository.
- `WARP_TEST_ENV_USE_PORTABLE_RESOURCES=1` forces the checked-in fixture when a
  sibling vault checkout also exists. Set it consistently for both the launcher
  and the test shell; forcing it only for tests selects a different password
  from a stack initialized with `vault/local/pg.yml`.
- The portable fixture also provides a public test password pepper, a
  throwaway P-256 JWT signing key (`jwt-local-evaluator.pem`), and a
  synthetic subsidy configuration for model tests. The subsidy keeps nonzero
  accrual, points, referral and wallet thresholds; tests that exercise only
  ledger settlement select an explicit zero-subsidy copy. These files stay
  under `testdata/*/local` and are selected by the existing local-resource
  rules; they neither load Main resources nor change application policy.
- Startup authenticates with the selected application password and checks
  `CREATEDB`. Container health and an open port alone do not prove that the
  selected profile matches an existing PostgreSQL volume.
- When authentication fails, select the profile used to initialize the volume.
  Changing Compose environment values does not update existing database roles.
  Only use `--fresh` to change profiles when the local database contents are
  disposable: it deletes the PostgreSQL data volume and initializes a new one.
- A refused hosts/lock preflight happens before `--fresh`, loopback, kernel, or
  Docker mutation. Do not delete a marker or lock merely because its recorded
  PID looks old. First verify that no `run-local.sh` process and no child
  `docker compose ... logs -f` process from any checkout still owns the stack;
  stop and join every owner normally. Then back up `/etc/hosts`, remove the
  complete legacy managed block and any active `local-pg.bringyour.com` or
  `local-redis.bringyour.com` aliases, flush the resolver cache, and remove a
  stale lock only after that ownership check. Preserve unrelated aliases on a
  shared hosts line.
- A launcher started from an older checkout can lack readiness or use an old
  owner token without a live process challenge. Stop it normally and restart
  the current `run-local.sh`; do not synthesize `ready` by hand.
- The postgres data volume (`pgdata`) persists across runs; the init script only
  runs on a fresh volume.
- The postgres image must be the glibc (debian) build, not alpine: the test
  harness creates databases `WITH ... LOCALE='en_US.UTF-8'`, which alpine lacks.

The local readiness regressions are Go tests (`local_run_local_*_test.go` and
the launcher cases in `test_harness_test.go`). They use temporary metadata and
Go-managed child processes. Bash remains necessary at the integration boundary
because the existing platform launcher owns shell traps, sudo, host mappings,
and Compose; these tests invoke its real helpers without provisioning services.
This is the existing-platform exception to the runner's Go automation rule.
