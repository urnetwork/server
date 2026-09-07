// Executes monitor-generated fixtures against the pinned Mimir PromQL engine
// without adding that engine or its dependencies to the Server module.
package subtensor_test

import (
	"os"
	"testing"
	"time"

	"github.com/prometheus/prometheus/promql/promqltest"
)

func TestSubtensorPromQL(t *testing.T) {
	path := os.Getenv("MONITOR_PROMQL_TEST_SCRIPT")
	if path == "" {
		t.Fatal("MONITOR_PROMQL_TEST_SCRIPT is required")
	}
	script, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	engine := promqltest.NewTestEngine(t, false, 5*time.Minute, 1_000_000)
	promqltest.RunTest(t, string(script), engine)
}
