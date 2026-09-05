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
  That attestation binds the lock's opaque owner token to the selected IP,
  hostnames, and ports. `test-env.sh` requires the complete record and the two
  unique launcher-managed mappings before it probes either service.

## Why not 127.0.0.1

Tests create and **drop** databases. A tunnel/port-forward to a real (prod)
database commonly listens on `127.0.0.1:5432`, so if the local hostnames ever
resolved to `127.0.0.1`, a test run could wipe prod. This setup therefore never
uses `127.0.0.1`: it binds to a distinct dedicated address instead, so the worst
case when the stack is down is "connection refused" — never a real database.
The script refuses to run if `LOCAL_HOST_IP` is set to `127.0.0.1`, either
hostname already has any unmanaged mapping, a legacy managed block remains, or
another launcher owns `/tmp/urnetwork-server-run-local.lock`. It never tries a
second resolved address and never silently rewrites an operator-owned entry.
An alias that happens to reach a database is not sufficient proof: it may be a
legacy VM route, an independently managed tunnel, or state from a launcher that
has not finished starting.

On Docker Desktop / macOS the container IPs on the docker network are not
routable from the host, so host access (where `go test` runs) goes through the
published ports on the loopback alias rather than the container's network IP.
On Linux, Docker and host networking changes run through `sudo`; on macOS,
Docker Desktop runs as the current user. The tests run as the current user.

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

Keep `run-local.sh` in the foreground for the entire test run. On exit it
withdraws the readiness attestation before changing hosts, listeners, or the
loopback alias. It removes readiness only when both the lock and attestation
still contain its owner token; ambiguous state is retained for inspection and
future preflights fail closed.

An isolated portable environment may provision its own disposable PostgreSQL
and Redis services instead of using this launcher. That workflow must select
the checked-in resources and opt out of launcher ownership explicitly:

```sh
export WARP_TEST_ENV_USE_PORTABLE_RESOURCES=1
export WARP_TEST_ENV_ALLOW_UNMANAGED_PORTABLE_SERVICES=1
source ./test-env.sh
```

The escape skips only the launcher attestation and managed-mapping checks; the
resource-authority checks and first-attempt service probes still run. Do not use
it to bless developer-machine aliases or a legacy local VM route.

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
  `helius.yml`, `ipinfo.yml`, `jwt.yml`, `jwt-local-evaluator.pem`,
  `password.yml`, `pg.yml`, `proxy.yml`, `redis.yml`, `services.yml`, `st.yml`,
  `stripe.yml`, `wireguard.yml`, and `x402.yml`, plus `tls` certificate/key
  pairs for `ur.network`, `bringyour.com`, `main-connect.ur.network`, and
  `main-connect.bringyour.com`; each pair must be colocated in the direct tree
  or one versioned directory;
- config: `apple_roots.pem`, `brevo.yml`, `city-list.yml`, `db.yml`,
  `email.yml`, `iso-country-list.yml`, `pro.yml`, `redis.yml`, `settings.yml`,
  `subsidy.yml`, and `tls.yml`.

An incomplete explicit resource checkout is rejected fail closed.
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
- A launcher started from an older checkout has no readiness attestation. Stop
  it normally and restart the current `run-local.sh`; do not synthesize `ready`
  by hand.
- The postgres data volume (`pgdata`) persists across runs; the init script only
  runs on a fresh volume.
- The postgres image must be the glibc (debian) build, not alpine: the test
  harness creates databases `WITH ... LOCALE='en_US.UTF-8'`, which alpine lacks.
