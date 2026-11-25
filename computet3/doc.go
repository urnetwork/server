// Package computet3 holds the compute ledger's tests on the private test stack's PostgreSQL 18 and Redis (DESIGN-COMPUTE
// §9, §14.4; BAL-M12-01), which the sidecar repo's tests never reach: sidecar/model's schema and scripts run here on
// the real stores, through server's own Db and Redis adapters (server/core, server/sidecarhost), each test in a
// database of its own and a Redis database of its own.
//
// Every test here builds under sidecar_pending, so server.DefaultTestEnv applies MG-C1 with the other pending groups,
// and runs through .stack/server-test.sh with that tag. compute_env_t3_test.go holds the shared env; each ledger task
// adds its own compute_*_t3_test.go (BAL-M12-02 to 05, 07, 08, 11 to 16 and 21, BAL-M13-02 and 03, BAL-M14-01 and 02,
// GAMMA-M12-01 and 06). Nothing here ships: the package holds no code but its tests.
package computet3
