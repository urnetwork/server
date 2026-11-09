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
//     buckets of the Net's own, and which refuses the fake contract issuer and ControlId sink.
//   - faults.go, what makes the base's plans happen on real stores: put, get, list and delete on the
//     Blobs, redis on the Redis, pg-commit through server.Tx's test hook, and http on a client's
//     transport to server's api.
//   - run.go, Run, RunRow under §22.8's rules with the nightly seeds, and Diverge, the check Q-TEST-1
//     asks for.
//   - suites.go, RunSuites, each sidecartest store suite on its real store, and RunCrossslot, §22.4's
//     runtime CROSSSLOT lint over the scripts and multi-key calls T3's engines made.
//
// server/sidecar-t3.sh runs this with -tags sidecar_testhooks,sidecar_pending and
// WARP_TEST_ENV_FAIL_FAST=1, inside the private test stores test-env.sh attests: its suites cadence on
// each merge, and its nightly cadence with the rows and their seeds.
//
// Not yet here, and failing closed where a row would reach it:
//
//   - server's exchange (server/connect/resident.go:902): a T3 Net refuses a row that adds a connect
//     client or a sidecar, rather than passing it on T2's platform fakes.
//   - server's api for a T3 sidecar to enroll through: the same refusal covers AddSidecar.
//   - the 3-master Redis Cluster: test-env.sh attests one Redis node until QD-028's change lands, so
//     RunCrossslot fails rather than linting on a node that accepts what 3 masters refuse.
//   - the store clock (TEST-M2-07's clock.go) and T3's MinIO layouts, QuorumLoss and DriveMiss.
//
// T3 runs never use t.Parallel: DenyPostgresForTest and the pg-commit hook are process-wide and serial,
// and a Net installs the process's test hooks. This package imports sidecartest, and sidecartest imports
// no server package, which keeps go-redis and minio-go out of the sidecar module.
package sidecart3
