package example_commands_test

import (
	"os"
	"testing"

	"github.com/redis/go-redis/v9/internal/urnetworktestguard"
)

// URnetwork: these examples dial localhost:6379 and flush it, so they run only
// with urnetworktestguard's opt-in.
func TestMain(m *testing.M) {
	os.Exit(urnetworktestguard.Run(m))
}
