package quotahost_test

// Gives quotahost's own tests sidecart3's inline-path gate (§22.5). sidecart3 imports api, whose handlers
// import quotahost, so only the external test package can import it.

import (
	"testing"

	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server/quotahost"
	"github.com/urnetwork/server/sidecart3"
)

func init() {
	quotahost.TestingInlinePath = func(t testing.TB, name string, redis sidecar.Redis, path func(redis sidecar.Redis)) int {
		return sidecart3.InlinePath(t, name, redis, path).RedisCallCount
	}
}
