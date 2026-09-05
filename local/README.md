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
- The postgres data volume (`pgdata`) persists across runs; the init script only
  runs on a fresh volume.
- The postgres image must be the glibc (debian) build, not alpine: the test
  harness creates databases `WITH ... LOCALE='en_US.UTF-8'`, which alpine lacks.
