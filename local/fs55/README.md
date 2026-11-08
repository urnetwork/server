# fs55

FS-55, the rollback harness (DESIGN.md §22.8; §23.6's every-Gn-out row; R-OPS-5,
D-OPS-6, R-PHASE-8). For one migration group it builds two releases and runs four
phases on a scratch database:

1. migrate with N and start N: N's smoke holds;
2. migrate with N+1 while N serves: N's smoke still holds, and N's query shapes
   plan on the migrated schema with sequential scans off;
3. restart N on N+1's schema, the rollback: N's smoke holds;
4. start N+1 with the group's gate on: N+1's smoke holds, the group's own routes
   included.

N is a base ref built with no tag. N+1 is the same ref with
`sidecar-migrations promote` run for every pending group through the run's own,
built with no tag: what the group's Gn-out commit builds. Both build in temporary
git worktrees with a direct `go build`; the checkout is never touched. A smoke is
a fixed set of api routes (`smoke.go`) whose status codes must hold. A phase that
fails ends the run, and the phases after it report `not run`, never `passed`.

## Running it

On server binaries, through the private stack:

```
.stack/server-test.sh <server-dir> -count=1 -timeout 2400s -v \
  -run TestFs55OnServerBinaries ./local/fs55 -fs55.base=<ref> -fs55.group=MG1
```

`server.TestEnv` gives the run its own database and Redis db, and the binaries run
as `WARP_ENV=local` processes that test-env.sh's attestation pins. The test stack's
portable config holds only what tests read, so each start's config home links it
with the monoroot config checkout's `all/` and any `local/` file it lacks
(`-config-extra`); nothing is copied. Run directly, the command takes vault and
config homes that already name a disposable database:

```
go run ./local/fs55 -base <ref> -group MG1 -vault <home> -config <home> [-config-extra <home>]
```

Before any build or dial, both refuse an attestation, an authority or a vault that
names loopback 5432, 6432 or 6379-6427, which can be production data tunnels, or
any endpoint but the private stack's. Neither touches `~/urnetwork`'s containers
nor runs the whole-suite scripts.

## What is not here yet

- N's query shapes on server binaries: the harness plans the shapes a release
  names, and the server releases name none yet. Capturing them from a smoke
  needs `pg_stat_statements` on the private stack.
- Each group's own routes: `fs55GroupPlan` gives each group the gate phase 4
  turns on, and no group yet has a route a smoke can ask without a credential, so
  phase 4 asks only the common routes and its report says so.
- taskworker is built with api and bringyourctl, and is not started.
