// Tests in this package own the config version: a test that orders configs
// sets WARP_CONFIG_VERSION itself. Inherited from the environment, the
// variable would make every enabled probe pass read the disabled marker from
// PostgreSQL, including in tests that have no database.
package work

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	os.Unsetenv("WARP_CONFIG_VERSION")
	os.Exit(m.Run())
}
