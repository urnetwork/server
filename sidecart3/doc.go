// §22.4's T3: the engines and fault schedules T2 runs on sidecartest's fakes, run on server's real stores,
// so a fake that does not do what its store does fails (D-TEST-2, Q-TEST-1). A row is one exported
// fault.Builder in sidecar's sidecartest/fault, registered for T2 and T3; its T2 test runs it there through
// fault.RunRow, and server's T3 test runs the same registration here through RunRow, never a second copy
// (plan-TEST-small-readings reading 1).
//
// What lives where:
//
//   - net.go, the T3 Net: sidecartest.Net whose NetSettings.Engines host its engines on
//     sidecarhost.NewServiceHost over server's Redis and FileClusters on the local stack's MinIO, in
//     buckets of the Net's own, whose NetSettings.Platform runs its connect clients on server's exchange,
//     and which refuses the fake contract issuer and ControlId sink.
//   - exchange.go, that platform: server's exchange and ConnectHandler on loopback listeners of their own,
//     each client authenticated by a jwt model.AuthNetworkClient mints in a funded network of the Net's,
//     its control answered by server's controller, so a fake_issuer row runs on what production gives it.
//   - faults.go, what makes the base's plans happen on real stores: put, get, list and delete on the
//     Blobs, redis on the Redis, pg-commit through server.Tx's test hook, and http on a client's
//     transport to server's api.
//   - run.go, Run, RunRow under §22.8's rules with the nightly seeds, and Diverge, the check Q-TEST-1
//     asks for.
//   - suites.go, RunSuites, each sidecartest store suite on its real store, and RunCrossslot, §22.4's
//     runtime CROSSSLOT lint over the scripts and multi-key calls T3's engines made.
//   - store_view.go, the Postgres fault's StoreView reads on T3, read-only, which NetSettings sets on
//     every T3 Net (TEST-M2-01).
//   - linearize.go, CheckLinearizable, §22.8's history rule: Porcupine over a fault.History, per ref,
//     with §11.8's commit rule and each unknown outcome's end open, which only server's tests import
//     (TEST-M2-02).
//   - clock.go, the store clock (TEST-M2-07; QD-264): every request's deadline in the store's time
//     (StoreTime), and, with Settings.StoreClock, LastModified and the named Redis families' ttls on the
//     schedule's clock, Advance aging them at once; RunRow runs a case with it exactly when catalog.yml
//     flags the case store_clock, and FS-114 never.
//   - cluster_faults.go, T3's 4-host file-cluster layouts (Settings.Layout, EVSB-M2-17's
//     local/file-cluster-t3) and their faults: HostDown, HostUp, QuorumLoss and DriveMiss carried out on
//     the minio fork's containers, and HostCluster for a read through one host.
//   - capture_stores.go, §22.7's CC3 (TEST-M2-09): every object, every Redis key on every master and
//     every sidecar Postgres table's rows added to a Net's capture, with PrivacyClassPostgres and, under
//     sidecar_leaktest, its leak (capture_stores_leak.go).
//   - scenario.go, §22.7's scenarios on T3 (TEST-M2-11): sidecartest's runner on a T3 Net, its stores
//     captured, the keys its host wrote given to the guard, and §7.4's allowlist from server's own route
//     tables; S1's three clauses run through server's router, and S3a and S4a through the builders T2
//     runs.
//   - scenario_sidecar.go, T3's enrolled sidecars: T3Api, server's api in process, every credential it
//     answers or is sent handed to the capture, and T3Sidecar, a sidecar.Sidecar enrolled through
//     server's own routes whose clients run on server's exchange, which S1's token and genesis clauses
//     run on.
//   - disruption.go, §22.8's disruptions beyond the schedules (TEST-M2-15): RedisRestart and
//     PostgresRestart on the test's own database, ResidentKill on server's exchange, and LbFixture, nginx
//     from warp's directives (local/sidecar-lb-fixture) whose Drain cuts an upstream block; their rows
//     run in sidecar-t3.sh's nightly form under sidecar_disruption.
//   - version_pair.go, §22.8's sidecar N against N±1 (TEST-M2-16): VersionPair, two dispatchers on
//     server's exchange planting each other's newer and older types and wire majors, each answered
//     UNIMPLEMENTED or VERSION; a binary of another commit set is not built, and every result says so.
//   - acl_exchange_tier.go, the fault.AclExchangeTier every T3 Net names (ACL-M4-04's FS-21): sidecars'
//     clients minted and bound as a sidecar's enrollment and bind leave them, on an exchange of the tier's
//     own, under server's contract gate over the row's state source with sidecar.yml's switch on,
//     network_acl_head raised as api's sequencer raises it, and a client's relay cut and restored, the one
//     partition server's exchange takes.
//
// server/sidecar-t3.sh runs this with -tags sidecar_testhooks,sidecar_pending and
// WARP_TEST_ENV_FAIL_FAST=1, inside the private test stores test-env.sh attests: its suites cadence on
// each merge, and its nightly cadence with the rows and their seeds.
//
// Not yet here, and failing closed where a row would reach it:
//
//   - a T3 Net's AddSidecar: a T3 Net refuses a row that adds a sidecar through sidecartest's helpers,
//     whose clients would run on T2's platform fakes, rather than passing it there; a T3 test enrolls a
//     T3Sidecar through server's api instead (scenario_sidecar.go), which reaches no evs, gkm or sockets
//     host of server's yet.
//   - a link's conditions (SetLink) and a contract's class (SetContractClass) on server's exchange, which
//     takes neither: a row that asks for one fails as unapplied (sidecartest.Net.Unapplied).
//   - the capture of the exchange's frames (CC2).
//   - the 3-master Redis Cluster: test-env.sh attests one Redis node until QD-028's change lands, so
//     RunCrossslot fails rather than linting on a node that accepts what 3 masters refuse.
//   - api on the store clock: T3 hosts no api but the config log's sequencer, which takes the Net's clock
//     itself, and server's exchange and controller keep real time (exchange.go).
//
// A Net that opens a connect client needs server's schema, so a test of a row with clients runs in a
// TestEnv that applies the migrations; a Net with none needs no schema. T3 runs never use t.Parallel:
// DenyPostgresForTest and the pg-commit hook are process-wide and serial, and a Net installs the process's
// test hooks. This package imports sidecartest, and sidecartest imports
// no server package, which keeps go-redis and minio-go out of the sidecar module.
package sidecart3
