// Package urnetworktestguard keeps this module's Redis-dialing test suites off
// whatever answers on localhost's Redis ports, unless the run says those ports
// are disposable test servers.
//
// URnetwork, 2026-11-25: the root package's suites and the doctests dial
// localhost's default Redis ports (6379, 6380, 6381, 6390-6392 and others)
// directly, and send FLUSHALL, FLUSHDB, CLIENT KILL and ACL changes. A
// developer machine forwarded those ports through an ssh tunnel to a
// production Redis cluster, and a run of these tests flushed it.
package urnetworktestguard

import (
	"fmt"
	"os"
	"testing"
)

// The opt-in. Set EnvName to EnvValue only on a machine whose localhost Redis
// ports are disposable test servers.
const (
	EnvName  = "URNETWORK_GO_REDIS_TESTS"
	EnvValue = "disposable-localhost"
)

// Allowed reports whether the run opted in, with exactly EnvValue.
func Allowed() bool {
	return os.Getenv(EnvName) == EnvValue
}

// Run runs m when the run opted in. Otherwise it runs nothing, so a package's
// tests never dial localhost's Redis ports by default, and says why.
func Run(m *testing.M) int {
	if !Allowed() {
		fmt.Fprintf(os.Stderr, "go-redis tests skipped: they dial localhost's default Redis ports and flush them; set %s=%s only where those ports are disposable test servers\n", EnvName, EnvValue)
		return 0
	}
	return m.Run()
}
