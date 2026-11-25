//go:build !sidecar_pending

// `sidecarctl compute provider` in a binary that does not link MG-C2's compute_provider, which is every binary until
// M12's Gn-out registers the group: each command refuses and reads or writes nothing. Needs the private test stack,
// through .stack/server-test.sh, for its WARP_ENV.

package main

import (
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// Each command exits 1 naming the unlinked group, and prints nothing.
func TestComputeProviderCommandsRefuseWithoutTheirTable(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		env := server.RequireEnv()
		now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
		for _, args := range [][]string{
			{"compute", "provider", "ls", env},
			{"compute", "provider", "gate", env, "engy", "--gate=G-PROV-engy", "--result=pass", "--by=rota-a"},
			{"compute", "provider", "suspend", env, "engy", "--by=rota-a", "--reason=its account was suspended"},
			{"compute", "provider", "resume", env, "engy", "--by=rota-a"},
		} {
			settings := defaultComputeProviderCommandSettings()
			settings.now = func() time.Time { return now }
			stdout := &strings.Builder{}
			var err error
			switch args[2] {
			case "ls":
				err = runComputeProviderLsWithSettings(args[3:], settings, stdout)
			case "gate":
				err = runComputeProviderGateWithSettings(args[3:], settings, stdout)
			default:
				err = runComputeProviderChangeWithSettings(args[3:], args[2], settings, stdout)
			}
			if err == nil || !strings.Contains(err.Error(), "does not link MG-C2") || stdout.Len() != 0 {
				tb.Errorf("%q: %v, printed %q", args, err, stdout.String())
			}
		}
	})
}
