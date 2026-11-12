//go:build sidecar_localstack

// S3a and S4a on T3 (§22.7; TEST-M2-11): sidecartest/scenarios' builders, the ones T2 runs, on a T3 Net, gkm's engine
// on the local stack's MinIO and the private Redis, under the harness and the guard. The store clock is on, since
// KEYS's schedules move the clock across ReaderKey and retention ages that gkm reads from LastModified.

package sidecart3

import (
	"testing"

	"github.com/urnetwork/sidecar/sidecartest/scenarios"

	"github.com/urnetwork/server"
)

// T3's settings for a scenario on gkm's worlds.
func keysScenarioSettings() *Settings {
	settings := DefaultSettings()
	settings.StoreClock = DefaultStoreClockSettings()
	return settings
}

func TestScenarioS3aOnT3(t *testing.T) {
	open := ""
	(&server.TestEnv{ApplyDbMigrations: false}).Run(t, func(tb testing.TB) {
		open = scenarios.RunOnTheMlsSubset(t, scenarios.S3a(t), ScenarioSettings(t, keysScenarioSettings()))
	})
	if open != "" {
		t.Skip(open)
	}
}

func TestScenarioS4aOnT3(t *testing.T) {
	open := ""
	(&server.TestEnv{ApplyDbMigrations: false}).Run(t, func(tb testing.TB) {
		open = scenarios.RunOnTheMlsSubset(t, scenarios.S4a(t), ScenarioSettings(t, keysScenarioSettings()))
	})
	if open != "" {
		t.Skip(open)
	}
}
