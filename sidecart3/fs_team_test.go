//go:build sidecar_localstack

// FS-14, FS-16, FS-102 and FS-124 on T3 (TEAM-M2-19): the builders sidecartest/fault/fs_team.go registers, on T3's
// Nets, so every append goes through server's real sequencer over the local stack's MinIO (config_log.go). FS-102's
// and FS-124's main cases run under every rule their rows take; FS-14 and FS-16 are forged rows and FS-102's and
// FS-124's forged cases forged ones, which run on T2 alone, so here their twins run, as §22.8's cadence rule says.

package sidecart3

import (
	"testing"

	"github.com/urnetwork/sidecar/sidecartest/fault"
)

func TestFs14OnT3(t *testing.T) {
	testEnv().Run(t, func(tb testing.TB) {
		Run(tb, fault.Fs14HonestBase, nil)
	})
}

func TestFs16OnT3(t *testing.T) {
	testEnv().Run(t, func(tb testing.TB) {
		Run(tb, fault.Fs16VetoWins, nil)
	})
}

func TestFs102OnT3(t *testing.T) {
	testEnv().Run(t, func(tb testing.TB) {
		RunRow(tb, "FS-102", "", nil)
	})
}

func TestFs102ForgedOnT3(t *testing.T) {
	testEnv().Run(t, func(tb testing.TB) {
		Run(tb, fault.Fs102AdmittedBeforeTheEnd, nil)
	})
}

func TestFs124OnT3(t *testing.T) {
	testEnv().Run(t, func(tb testing.TB) {
		RunRow(tb, "FS-124", "", nil)
	})
}

func TestFs124ForgedOnT3(t *testing.T) {
	testEnv().Run(t, func(tb testing.TB) {
		Run(tb, fault.Fs124Countersigned, nil)
	})
}
