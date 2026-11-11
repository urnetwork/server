//go:build sidecar_localstack && sidecar_testhooks

// FS-1's and FS-58's histories under CheckLinearizable on T3 (TEST-M2-02; §22.8's history rule is T3's): the runs
// linearize_rows_test.go makes on T2's fakes, on T3's Nets, whose evs instances run server/cli/evs's block over the
// private test stores and the local stack's MinIO. FS-1 takes a wider spread of lost answers than on T2, since each
// run here costs real store round trips.

package sidecart3

import (
	"testing"

	"github.com/urnetwork/sidecar/sidecartest"
)

func TestFs1HistoriesLinearizeOnT3(t *testing.T) {
	testEnv().Run(t, func(tb testing.TB) {
		linearizeFs1Rows(tb, func() *sidecartest.NetSettings { return NetSettings(tb, nil) }, 100)
	})
}

func TestFs58HistoriesLinearizeOnT3(t *testing.T) {
	testEnv().Run(t, func(tb testing.TB) {
		linearizeFs58Rows(tb, func() *sidecartest.NetSettings { return NetSettings(tb, nil) })
	})
}
