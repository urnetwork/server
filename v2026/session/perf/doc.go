// Package perf is the local performance harness for session minting, the
// authentication check, revocation and client removal (README.md).
//
// The harness is test code only. TestSessionPerf runs when
// SESSION_PERF_REDIS_CLUSTER names a disposable Redis Cluster that run.sh
// started, and skips otherwise, so ordinary package runs never reach it.
package perf
