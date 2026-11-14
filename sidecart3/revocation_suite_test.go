//go:build sidecar_localstack

// §22.7's revocation suite's device form on T3 (TEST-M2-12): sidecartest's suite with the checks sidecartest/scenarios
// registers, the ones T2 runs, each on a T3 Net: gkm's and evs's engines on the local stack's MinIO and the private
// Redis. The store clock is on, since the keys check moves the clock across a grant's expiry, which gkm reads from
// LastModified. A check that needs the hooks reads open on this leg, as on T2's plain one.

package sidecart3

import (
	"testing"

	"github.com/urnetwork/sidecar/sidecartest"
	"github.com/urnetwork/sidecar/sidecartest/scenarios"

	"github.com/urnetwork/server"
)

func TestRevocationSuiteDeviceFormOnT3(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false}).Run(t, func(tb testing.TB) {
		report := sidecartest.RunRevocationSuite(t, sidecartest.RevocationFormDevice, &sidecartest.RevocationSuiteSettings{
			Net: NetSettings(t, keysScenarioSettings()),
		})
		if len(report.Of(sidecartest.RevocationReadLoss)) == 0 || scenarios.RevocationKeysNextGeneration == nil {
			t.Errorf("the device form ran no check of a read loss: %s", report)
		}
	})
}
