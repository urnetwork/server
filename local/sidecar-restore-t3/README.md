# FS-42 on T3

§22.8's Postgres-loss drill (FS-42; D-OPS-14, D-OPS-18, FI-12) as OPS-M3-04 builds it at M3: the primary lost, both
of edge-6's dumps restored through xops's own scripts, in each order of their snapshot times, and §19.10's restore
frame run through the api step with every step twice and FI-12's clauses checked after each.

```sh
source ./test-env.sh                                # after the private test stack's env, as for any direct go test on Postgres
local/sidecar-restore-t3/run.sh                     # both orders
local/sidecar-restore-t3/run.sh -run '^TestFs42MainDumpNewer$'
```

What one run does, in `sidecarrestore.PostgresLossFixture` (server/sidecarrestore/fs42_fixture.go):

| Step | How |
|---|---|
| seed | the test's private database is the primary; networks, members and sidecars' clients are written through api's own model functions, a config log through server's sequencer on T2's fake first cluster |
| main dump | `pg_dump --format=directory` with `sidecar-tables.conf`'s exclude list, as `bringyour-pg-backup.sh` runs it, and a `backup-point.txt` whose `copy_ended` is the database's time to the microsecond |
| sidecar dump | xops's `bringyour-pg-sidecar-dump.sh`, its generation then checked, decrypted and unpacked as `postgres-backup/restore.md` says |
| loss | a new database: the main dump restored into it, then `bringyour-pg-sidecar-restore.sh` under the incident, which reconciles the rows by the dumps' times, marks the database and starts `incident-<id>.tsv`; the env's pools then point at it |
| frame | `RunFrameThrough`: each step through the named one, twice, a step no task registered ended by hand |

The scripts run under this machine's bash with the host's tools faked (`runuser`, `zfs`, `findmnt`, `df`, `chown`,
`flock`, `timeout`), as xops's live tests run them, and `psql`, `pg_dump` and `pg_restore` run in a client container
of the server's own image (`postgres:18`) through `BRINGYOUR_PG_BINDIR`'s wrappers, seeing the fixture's directory at
the same path. Each fixture makes three roles on the private server (`sc_restore_*`), one database (`scr_*`) and one
container (`sidecar-test-restore-*`), and removes them, gpg's agent with them. Nothing is fetched, and nothing but the
private server's published port is dialled.

The tests run only where `SIDECAR_RESTORE_T3=1`, which this script sets, so a plain `go test` of the package skips
them rather than reaching for Docker.

At M3 the schedule asserts, in both orders: a network, member and sidecar newer than the main dump keep their ids; a
client deactivated between the dumps stays so; a network deleted between the dumps stays deleted with its sidecar
rows, and a client created after the sidecar dump is deactivated to re-enroll and listed (main newer); a network
newer than both is lost and its log listed as orphaned; and a revocation filed after both dumps is honoured once api
replays proj/. Later owners add their steps on the same fixture at their milestones' Gn-outs.

From M8 the api step also raises every push token row's revision by 2^32, once per incident (OPS-M8-01), which two
tests here check: `TestFs42PushTokensRaisedOncePerIncident`, every restored row raised once by the frame's two runs
and a third, above the revision the lost primary reached; and `TestFs42PushTokenRaiseSkippedBeforeMg8`, a primary
with no `sidecar_push_token` restored and the step ending with the raise skipped and listed. FS-42's own M8 clause,
the applies that follow and a client created after the dump re-registering, is SDK-M8-12's.
