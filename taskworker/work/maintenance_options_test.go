// The scheduled maintenance run takes the priority index gate from
// db_maintenance.yml when it starts.
package work

import (
	"slices"
	"testing"

	"github.com/urnetwork/server"
)

// A run rebuilds bloated priority indexes only when db_maintenance.yml enables
// it, and otherwise keeps the default steps and its epoch's progress.
func TestDbMaintenanceOptionsRebuildPriorityIndexesOnlyWhenConfigured(t *testing.T) {
	args := &DbMaintenanceArgs{
		Epoch:       3,
		DoneTables:  []string{"synthetic_done_table"},
		DoneIndexes: []string{"synthetic_done_index"},
	}
	for _, c := range []struct {
		config string
		want   bool
	}{
		{config: "min_connections: 0\nmax_connections: 32\n", want: false},
		{config: "priority_reindex_enabled: false\n", want: false},
		{config: "min_connections: 0\nmax_connections: 32\npriority_reindex_enabled: true\n", want: true},
	} {
		func() {
			defer server.Config.PushSimpleResource(server.MaintenancePgConfigResourceName, []byte(c.config))()
			opts := dbMaintenanceOptions(args)
			if opts.PriorityReindex != c.want {
				t.Errorf("config %q: PriorityReindex = %t, want %t", c.config, opts.PriorityReindex, c.want)
			}
			if !opts.Reindex || !opts.Cleanup || !opts.Analyze ||
				!slices.Equal(opts.DoneTables, args.DoneTables) || !slices.Equal(opts.DoneIndexes, args.DoneIndexes) {
				t.Errorf("config %q: options %+v lost the default steps or the epoch's progress", c.config, *opts)
			}
		}()
	}
}
