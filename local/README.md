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
- Postgres provisions the role + database from `vault/local/pg.yml` via
  [`postgres/initdb/01-init-app-db.sh`](postgres/initdb/01-init-app-db.sh). The
  role gets `CREATEDB` because the test harness creates/drops a database per test.
  The cluster is initialized with `LOCALE=en_US.UTF-8` so the harness's
  `CREATE DATABASE ... LOCALE='en_US.UTF-8'` succeeds.
- While the script runs it adds a **dedicated loopback alias** (`LOCAL_HOST_IP`,
  default `10.213.0.1`) to the loopback interface, publishes the DB ports on it,
  and maps the hostnames to it in `/etc/hosts`:

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
The script refuses to run if `LOCAL_HOST_IP` is set to `127.0.0.1`.

On Docker Desktop / macOS the container IPs on the docker network are not
routable from the host, so host access (where `go test` runs) goes through the
published ports on the loopback alias rather than the container's network IP.
`sudo` is used only to edit `/etc/hosts` and manage the loopback alias; docker
and the tests run as you.

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

- Credentials/ports come from `vault/local/{pg,redis}.yml`. If you change them,
  re-run with `--fresh` so the postgres init script re-provisions.
- The postgres data volume (`pgdata`) persists across runs; the init script only
  runs on a fresh volume.
- The postgres image must be the glibc (debian) build, not alpine: the test
  harness creates databases `WITH ... LOCALE='en_US.UTF-8'`, which alpine lacks.
