//go:build sidecar_localstack

// S3b and S4b on T3 (§22.7; TEAM-M6-12): sidecartest/scenarios' builders, the ones T2 runs, on a T3 Net, gkm's engine
// on the local stack's MinIO and the private Redis and the Team's log on server's sequencer, under the harness and the
// guard. The store clock is on, as S3a's and S4a's are, since KEYS's schedules move the clock across ages gkm reads
// from LastModified.

package sidecart3

import (
	"testing"

	"github.com/urnetwork/sidecar/sidecartest"
	"github.com/urnetwork/sidecar/sidecartest/scenarios"

	"github.com/urnetwork/server"
)

func TestScenarioS3bOnT3(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false}).Run(t, func(tb testing.TB) {
		sidecartest.RunScenario(t, scenarios.S3b(t), ScenarioSettings(t, keysScenarioSettings()))
	})
}

func TestScenarioS4bOnT3(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false}).Run(t, func(tb testing.TB) {
		sidecartest.RunScenario(t, scenarios.S4b(t), ScenarioSettings(t, keysScenarioSettings()))
	})
}
