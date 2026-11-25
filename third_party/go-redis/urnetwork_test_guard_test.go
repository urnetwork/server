package redis_test

import (
	"os"
	"testing"

	"github.com/redis/go-redis/v9/internal/urnetworktestguard"
)

// URnetwork: this package's suites dial localhost's default Redis ports and
// flush them, so they run only with urnetworktestguard's opt-in.
func TestMain(m *testing.M) {
	os.Exit(urnetworktestguard.Run(m))
}
