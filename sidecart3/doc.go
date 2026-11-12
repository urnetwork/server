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
//     tables; S1's code-create clause runs through server's router, and S3a and S4a through the builders
//     T2 runs.
//
// server/sidecar-t3.sh runs this with -tags sidecar_testhooks,sidecar_pending and
// WARP_TEST_ENV_FAIL_FAST=1, inside the private test stores test-env.sh attests: its suites cadence on
// each merge, and its nightly cadence with the rows and their seeds.
//
// Not yet here, and failing closed where a row would reach it:
//
//   - server's api for a T3 sidecar to enroll through: a T3 Net refuses a row that adds a sidecar, whose
//     clients would run on T2's platform fakes, rather than passing it there.
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
