//go:build sidecar_filecluster_t3 && sidecar_testhooks

// FS-86's card commits on T3's 4-host file-cluster layouts (§22.4, §22.8, D-TEST-17; §16.11's cards ref commits row,
// D-MCP-31; RECORD-M10-07; QD-1068): the row's reading. The schedule sidecartest/fault exports (fs86_cards_evs.go) runs
// on a T3 Net whose metadata cluster is meta-1 and whose segment cluster is seg-1 of the layouts
// local/file-cluster-t3 starts (t3.sh build, then up), the drives the NVMe rows price, with evshost's block as the evs
// world's instance. 16 devices' card writes land on the one cards ref's head for a whole peak hour at the Team limit's
// 2.3 pushes a second, then 15 s at three times it, held to the row: at the peak p99 <= 300 ms from arrival to the
// write's return, at most 0.5 412 reruns a commit on average and no 429 BUSY; at the burst p99 <= 1 s, at most 1 rerun
// on average, BUSY on at most 0.1% of pushes, each committing within 5 s; no FOLD_BEHIND in either. A miss at the Team
// limit blocks the gate (D-TEST-17), so a miss fails the test, and the owner hears then.
//
// It runs in sidecar-t3.sh nightly, never in the whole-suite scripts, and its numbers are a reading only with nothing
// else running on the machine: the hour is real time. It fails rather than skips when the layouts are down. The local
// stack's runs of the same schedule (sidecar_directory_load_t3_test.go) are the reduced-scale form and gate nothing.
//
// Not here: the stress case's hour, which is no gate's; format 2's index entries and heads; the year-one mix beside the
// directory's pushes; the 26 h run; and the governor's FI-14.

package server_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/urnetwork/sidecar/sidecartest/fault"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/sidecart3"
)

// T3's settings on the layouts, as local/file-cluster-t3 starts them: a Net's metadata cluster on meta-1 and its segment
// cluster on seg-1.
func sidecarDirectoryLoadLayoutSettings(tb testing.TB) *sidecart3.Settings {
	layout, err := sidecart3.LoadClusterLayout(filepath.Join("local", "file-cluster-t3"), "meta-1", "seg-1")
	if err != nil {
		tb.Fatal(err)
	}
	settings := sidecart3.DefaultSettings()
	settings.Layout = layout
	return settings
}

func TestFs86CardCommitsAtTheTeamLimitOnTheLayouts(t *testing.T) {
	// the private Redis a T3 Net's host needs, and no schema: an evs world opens no connect client
	(&server.TestEnv{ApplyDbMigrations: false}).Run(t, func(tb testing.TB) {
		settings := fault.DefaultFs86CardsEvsSettings()
		// the peak hour whole, at the Team limit's rate, the defaults' 2.3 pushes a second
		settings.PeakDuration = time.Hour
		state, err := fault.RunFs86CardsEvs(tb, sidecart3.NetSettings(tb, sidecarDirectoryLoadLayoutSettings(tb)), settings)
		if err != nil {
			tb.Fatal(err)
		}
		if phases := state.Phases(); len(phases) != 3 {
			tb.Fatalf("%d phases ran", len(phases))
		}
	})
}
