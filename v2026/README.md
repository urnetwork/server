# server

## Local integration tests

Server integration tests create and drop PostgreSQL databases and lease Redis
logical databases. Start the isolated backing services in one terminal:

```sh
./local/run-local.sh
```

The launcher manages the dedicated loopback address and host mappings; do not
point the local service names at `127.0.0.1`, where a production tunnel could be
listening.

Use the canonical test runner from a second terminal:

```sh
./test.sh -run TestName
```

For a direct package release gate, source the same environment and dependency
preflight first. For example:

```sh
source ./test-env.sh &&
  go test . ./stats -count=1 -timeout=30m
```

The preflight sets `WARP_ENV=local` only when it is unset, refuses any non-local
value, resolves the required PostgreSQL/Redis resources, and checks both
services before tests start. An isolated server checkout uses the public,
throwaway resources under `local/testdata`; normally a sibling `vault`/`config`
checkout or explicit `WARP_VAULT_HOME`/`WARP_CONFIG_HOME` takes precedence. Set
`WARP_TEST_ENV_USE_PORTABLE_RESOURCES=1` to force the throwaway resources in an
environment that also has sibling configuration checkouts.
