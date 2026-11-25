//go:build sidecar_pending

// `sidecarctl compute provider` and computeservice's provider store against the env as server reaches it: MG-C2's
// compute_provider and compute_provider_price on the private test stack's Postgres, so these build under
// sidecar_pending and run through .stack/server-test.sh (server.DefaultTestEnv), under its WARP_ENV. Each subcommand
// on a database, a gate without a result refused with nothing written, a status change recorded with its gate result,
// a price read per model and hour, rows that disagree with this build refused, and the registry gating on the table.

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	operator "github.com/urnetwork/operator-core"
	"github.com/urnetwork/sidecar/computeservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/core"
)

// The four registered commands as the binary runs them against the env, with the clock fixed at now.
func runServerComputeProviderCommand(now time.Time, args ...string) (int, string, string) {
	settings := defaultComputeProviderCommandSettings()
	settings.now = func() time.Time { return now }
	commands := newSidecarctlCommandSet()
	for _, words := range []string{"compute provider ls", "compute provider gate", "compute provider suspend", "compute provider resume"} {
		registered, _ := sidecarctlCommands.find(strings.Fields(words))
		command := *registered
		switch words {
		case "compute provider ls":
			command.run = func(args []string, out io.Writer) error { return runComputeProviderLsWithSettings(args, settings, out) }
		case "compute provider gate":
			command.run = func(args []string, out io.Writer) error {
				return runComputeProviderGateWithSettings(args, settings, out)
			}
		case "compute provider suspend":
			command.run = func(args []string, out io.Writer) error {
				return runComputeProviderChangeWithSettings(args, computeservice.ComputeProviderHistorySuspend, settings, out)
			}
		case "compute provider resume":
			command.run = func(args []string, out io.Writer) error {
				return runComputeProviderChangeWithSettings(args, computeservice.ComputeProviderHistoryResume, settings, out)
			}
		}
		commands.add(&command)
	}
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	code := commands.run(args, stdout, stderr)
	return code, stdout.String(), stderr.String()
}

// The env's provider store.
func serverComputeProviderStore() *computeservice.ComputeProviderStore {
	return computeservice.NewComputeProviderStore(core.NewDb(), computeservice.DefaultComputeProviderSettings())
}

// The env's row of id, nil where it has none.
func serverComputeProviderRow(tb testing.TB, ctx context.Context, id computeservice.ComputeProviderId) *computeservice.ComputeProviderRow {
	rows, err := serverComputeProviderStore().Providers(ctx)
	if err != nil {
		tb.Fatal(err)
	}
	for _, row := range rows {
		if row.Id == id {
			return row
		}
	}
	return nil
}

// A gate without its result is refused with nothing written; each gate's result is recorded with its time, its
// operator, its evidence and its note, and moves the status: engy's own gate admits it, a failed G-PROV-any takes it
// out, and a passing G-PROV-any readmits it while its own gate's pass stands.
func TestComputeProviderGateRecordsTheStatusChangeWithItsResult(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		ctx := context.Background()
		env := server.RequireEnv()
		now := server.NowUtc().Truncate(time.Second)

		code, stdout, stderr := runServerComputeProviderCommand(now, "compute", "provider", "gate", env, "engy", "--gate=G-PROV-engy", "--by=rota-a")
		if code != 2 || stdout != "" || !strings.Contains(stderr, "--result is pass or fail") {
			tb.Fatalf("a gate without its result exited %d: %q %q", code, stdout, stderr)
		}
		if row := serverComputeProviderRow(tb, ctx, computeservice.ComputeProviderIdEngy); row != nil {
			tb.Fatalf("a refused gate wrote %+v", row)
		}

		steps := []struct {
			args   []string
			status computeservice.ComputeProviderStatus
		}{
			{args: []string{"--gate=G-PROV-engy", "--result=pass", "--by=rota-a", "--evidence=sha256:0011", "--note=btauth/1 and receipts shown"}, status: computeservice.ComputeProviderStatusGated},
			{args: []string{"--gate=G-PROV-any", "--result=fail", "--by=rota-b", "--note=receipts stopped"}, status: computeservice.ComputeProviderStatusOut},
			{args: []string{"--gate=G-PROV-any", "--result=pass", "--by=rota-a"}, status: computeservice.ComputeProviderStatusGated},
		}
		for i, step := range steps {
			at := now.Add(time.Duration(i) * time.Hour)
			code, stdout, stderr := runServerComputeProviderCommand(at, append([]string{"compute", "provider", "gate", env, "engy"}, step.args...)...)
			if code != 0 || !strings.Contains(stdout, "then "+step.status.String()) {
				tb.Fatalf("step %d exited %d: %q %q", i, code, stdout, stderr)
			}
			row := serverComputeProviderRow(tb, ctx, computeservice.ComputeProviderIdEngy)
			if row == nil || row.Status != step.status || len(row.History) != i+1 || !row.StatusTime.Equal(at) {
				tb.Fatalf("step %d left %+v", i, row)
			}
		}
		row := serverComputeProviderRow(tb, ctx, computeservice.ComputeProviderIdEngy)
		first := row.History[0]
		if first.Kind != computeservice.ComputeProviderHistoryGate || first.Gate != "G-PROV-engy" || first.Result != computeservice.ComputeProviderGatePass ||
			first.From != computeservice.ComputeProviderStatusListed || first.To != computeservice.ComputeProviderStatusGated ||
			!first.Time().Equal(now) || first.By != "rota-a" || first.Evidence != "sha256:0011" || first.Note != "btauth/1 and receipts shown" {
			tb.Fatalf("the first entry reads %+v", first)
		}
		if due := row.RecheckDue(computeservice.DefaultComputeProviderSettings().GateRecheckInterval); !due.Equal(now.Add(2*time.Hour).AddDate(0, 0, 90)) {
			tb.Fatalf("G-PROV-any falls due %s", due)
		}
		statuses, err := serverComputeProviderStore().Statuses(ctx)
		if err != nil || len(statuses) != 1 || statuses[computeservice.ComputeProviderIdEngy] != computeservice.ComputeProviderStatusGated {
			tb.Fatalf("the statuses read %v, %v", statuses, err)
		}
	})
}

// A suspension of a provider not gated is refused with no row written; a gated one is suspended with its reason and
// resumed, and a resumption of one not suspended is refused, its status unchanged.
func TestComputeProviderSuspendAndResumeOnTheEnv(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		ctx := context.Background()
		env := server.RequireEnv()
		now := server.NowUtc().Truncate(time.Second)
		code, _, stderr := runServerComputeProviderCommand(now, "compute", "provider", "suspend", env, "hippius", "--by=rota-a", "--reason=its account was suspended")
		if code != 1 || !strings.Contains(stderr, "only a gated provider is suspended") {
			tb.Fatalf("suspending a listed provider exited %d: %q", code, stderr)
		}
		if row := serverComputeProviderRow(tb, ctx, computeservice.ComputeProviderIdHippius); row != nil {
			tb.Fatalf("a refused suspension wrote %+v", row)
		}
		for _, step := range []struct {
			args   []string
			code   int
			status computeservice.ComputeProviderStatus
		}{
			{args: []string{"gate", env, "hippius", "--gate=G-PROV-hippius", "--result=pass", "--by=rota-a"}, code: 0, status: computeservice.ComputeProviderStatusGated},
			{args: []string{"suspend", env, "hippius", "--by=rota-a", "--reason=its account was suspended"}, code: 0, status: computeservice.ComputeProviderStatusSuspended},
			{args: []string{"resume", env, "hippius", "--by=rota-b", "--note=reinstated"}, code: 0, status: computeservice.ComputeProviderStatusGated},
			{args: []string{"resume", env, "hippius", "--by=rota-b"}, code: 1, status: computeservice.ComputeProviderStatusGated},
		} {
			code, stdout, stderr := runServerComputeProviderCommand(now, append([]string{"compute", "provider"}, step.args...)...)
			row := serverComputeProviderRow(tb, ctx, computeservice.ComputeProviderIdHippius)
			if code != step.code || row == nil || row.Status != step.status {
				tb.Fatalf("%q exited %d (%q %q) leaving %+v", step.args, code, stdout, stderr, row)
			}
		}
		row := serverComputeProviderRow(tb, ctx, computeservice.ComputeProviderIdHippius)
		if len(row.History) != 3 || row.History[1].Kind != computeservice.ComputeProviderHistorySuspend || row.History[1].Note != "its account was suspended" ||
			row.History[2].Kind != computeservice.ComputeProviderHistoryResume || row.History[2].By != "rota-b" {
			tb.Fatalf("the history reads %+v", row.History)
		}
	})
}

// ls prints every provider with the env's rows; an env the process does not run as is refused, with nothing written.
func TestComputeProviderLsAndAnotherEnvOnTheEnv(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		ctx := context.Background()
		env := server.RequireEnv()
		now := server.NowUtc().Truncate(time.Second)
		if code, _, stderr := runServerComputeProviderCommand(now, "compute", "provider", "gate", env, "saygm", "--gate=G-PROV-saygm", "--result=pass", "--by=rota-a"); code != 0 {
			tb.Fatal(stderr)
		}
		code, stdout, stderr := runServerComputeProviderCommand(now, "compute", "provider", "ls", env)
		if code != 0 {
			tb.Fatal(stderr)
		}
		for _, want := range []string{"compute providers in " + env, "saygm", "gated", now.AddDate(0, 0, 90).Format(time.RFC3339), "chutes", "no row"} {
			if !strings.Contains(stdout, want) {
				tb.Errorf("ls printed %q, want %q in it", stdout, want)
			}
		}
		other := "another-env"
		code, _, stderr = runServerComputeProviderCommand(now, "compute", "provider", "gate", other, "engy", "--gate=G-PROV-engy", "--result=pass", "--by=rota-a")
		if code != 1 || !strings.Contains(stderr, "nothing was read or written in "+other) {
			tb.Fatalf("another env exited %d: %q", code, stderr)
		}
		if row := serverComputeProviderRow(tb, ctx, computeservice.ComputeProviderIdEngy); row != nil {
			tb.Fatalf("another env's gate wrote %+v", row)
		}
	})
}

// One read a model or shape and hour: a later read in the hour replaces the earlier, an earlier never the later, the
// next hour's is its own; the newest of each model reads back, with its capacity known or not.
func TestComputeProviderStorePriceReadPerModelAndHour(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		ctx := context.Background()
		store := serverComputeProviderStore()
		hour := time.Date(2026, 11, 24, 13, 0, 0, 0, time.UTC)
		read := func(itemId string, minutes int, input int64, capacity int64) *computeservice.ComputeProviderPriceRead {
			return &computeservice.ComputeProviderPriceRead{
				Provider: computeservice.ComputeProviderIdEngy, Item: computeservice.ComputeProviderPriceItemModel, ItemId: itemId,
				ReadTime: hour.Add(time.Duration(minutes) * time.Minute), GammaNanoUsd: 1_050_000_000,
				InputNanoGammaPerMillion: input, OutputNanoGammaPerMillion: 4 * input, Capacity: capacity, CapacityKnown: 0 <= capacity,
			}
		}
		for _, reads := range [][]*computeservice.ComputeProviderPriceRead{
			{read("model-a", 10, 100, 3), read("model-b", 10, 200, -1)},
			{read("model-a", 40, 110, 4)},
			{read("model-a", 5, 90, 1)},
			{read("model-a", 65, 120, 5)},
		} {
			if err := store.WritePriceReads(ctx, reads); err != nil {
				tb.Fatal(err)
			}
		}
		shape := &computeservice.ComputeProviderPriceRead{
			Provider: computeservice.ComputeProviderIdHippius, Item: computeservice.ComputeProviderPriceItemShape, ItemId: "cpu-2",
			ReadTime: hour.Add(time.Minute), GammaNanoUsd: 1_000_000_000, HourNanoGamma: 40_000_000, EgressNanoGammaPerGib: 10_000_000,
		}
		if err := store.WritePriceReads(ctx, []*computeservice.ComputeProviderPriceRead{shape}); err != nil {
			tb.Fatal(err)
		}

		reads, err := store.PriceReads(ctx, computeservice.ComputeProviderIdEngy, hour, hour.Add(2*time.Hour))
		if err != nil {
			tb.Fatal(err)
		}
		got := []string{}
		for _, r := range reads {
			got = append(got, fmt.Sprintf("%s@%s=%d", r.ItemId, r.ReadHour().Format("15"), r.InputNanoGammaPerMillion))
		}
		if strings.Join(got, " ") != "model-a@13=110 model-b@13=200 model-a@14=120" {
			tb.Fatalf("the hours read %v", got)
		}
		if !reads[0].ReadTime.Equal(hour.Add(40*time.Minute)) || !reads[0].CapacityKnown || reads[0].Capacity != 4 || reads[1].CapacityKnown {
			tb.Fatalf("the reads read %+v %+v", reads[0], reads[1])
		}
		latest, err := store.LatestPriceReads(ctx, computeservice.ComputeProviderIdEngy)
		if err != nil || len(latest) != 2 || latest[0].ItemId != "model-a" || latest[0].InputNanoGammaPerMillion != 120 ||
			latest[1].ItemId != "model-b" || latest[1].GammaNanoUsd != 1_050_000_000 {
			tb.Fatalf("the newest reads %+v, %v", latest, err)
		}
		shapes, err := store.LatestPriceReads(ctx, computeservice.ComputeProviderIdHippius)
		if err != nil || len(shapes) != 1 || shapes[0].Item != computeservice.ComputeProviderPriceItemShape || shapes[0].HourNanoGamma != 40_000_000 ||
			shapes[0].EgressNanoGammaPerGib != 10_000_000 || shapes[0].InputNanoGammaPerMillion != 0 {
			tb.Fatalf("hippius' shape reads %+v, %v", shapes, err)
		}
	})
}

// A row whose code, slug or kind disagree with this build refuses its provider: Statuses leaves it out and Providers
// fails rather than list around it.
func TestComputeProviderStoreRefusesRowsThatDisagree(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		ctx := context.Background()
		db := core.NewDb()
		err := db.Tx(ctx, func(tx operator.DbTx) error {
			_, err := tx.Exec(ctx, `
				INSERT INTO compute_provider (provider, slug, kind, status, status_time, history)
				VALUES (2, 'saygm', 1, 2, $1, '[]'::jsonb)
			`, server.NowUtc())
			return err
		})
		if err != nil {
			tb.Fatal(err)
		}
		statuses, err := serverComputeProviderStore().Statuses(ctx)
		if err != nil || len(statuses) != 0 {
			tb.Fatalf("a foreign row read as %v, %v", statuses, err)
		}
		if rows, err := serverComputeProviderStore().Providers(ctx); err == nil || !errors.Is(err, computeservice.ErrComputeProviderUnknown) {
			tb.Fatalf("a foreign row listed %v, %v", rows, err)
		}
	})
}

// Two gate results recorded at once for one provider are both kept, each under the row's lock: neither entry is lost.
func TestComputeProviderStoreKeepsConcurrentGateResults(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		ctx := context.Background()
		now := server.NowUtc().Truncate(time.Second)
		var waitGroup sync.WaitGroup
		errs := make([]error, 2)
		for i, by := range []string{"rota-a", "rota-b"} {
			waitGroup.Add(1)
			go func() {
				defer waitGroup.Done()
				_, errs[i] = serverComputeProviderStore().RecordGate(ctx, computeservice.ComputeProviderIdLium,
					&computeservice.ComputeProviderGateResult{Gate: "G-PROV-lium", Passed: true, By: by}, now)
			}()
		}
		waitGroup.Wait()
		for _, err := range errs {
			if err != nil {
				tb.Fatal(err)
			}
		}
		row := serverComputeProviderRow(tb, ctx, computeservice.ComputeProviderIdLium)
		if row == nil || row.Status != computeservice.ComputeProviderStatusGated || len(row.History) != 2 || row.History[0].By == row.History[1].By {
			tb.Fatalf("lium's row reads %+v", row)
		}
	})
}

// A registry over the env's table serves the reference provider in local, a development env, once its gate passes,
// and stops serving it once it is suspended; a provider whose settings do not enable it serves nothing, gated or not.
func TestComputeProviderRegistryGatesOnTheEnvsTable(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		ctx := context.Background()
		env := server.RequireEnv()
		now := server.NowUtc().Truncate(time.Second)
		settings := computeservice.DefaultComputeProviderSettings()
		settings.Environment = env
		settings.StatusMaxAge = 0
		registry, err := computeservice.NewComputeProviderRegistry(&computeservice.ComputeProviderRegistryArgs{
			Settings: settings,
			Db:       func() (operator.Db, error) { return core.NewDb(), nil },
		})
		if err != nil {
			tb.Fatal(err)
		}
		if adapter, err := registry.Enabled(ctx, computeservice.ComputeProviderIdReference); err == nil || adapter != nil {
			tb.Fatalf("a reference provider with no row served: %v", adapter)
		}
		for _, args := range [][]string{
			{"gate", env, "reference", "--gate=G-PROV-reference", "--result=pass", "--by=rota-a"},
			{"gate", env, "engy", "--gate=G-PROV-engy", "--result=pass", "--by=rota-a"},
		} {
			if code, _, stderr := runServerComputeProviderCommand(now, append([]string{"compute", "provider"}, args...)...); code != 0 {
				tb.Fatal(stderr)
			}
		}
		adapter, err := registry.Enabled(ctx, computeservice.ComputeProviderIdReference)
		if err != nil || adapter.Id() != computeservice.ComputeProviderIdReference {
			tb.Fatalf("a gated reference provider in %s did not serve: %v", env, err)
		}
		if adapter, err := registry.Enabled(ctx, computeservice.ComputeProviderIdEngy); err == nil || adapter != nil {
			tb.Fatalf("a gated engy the settings do not enable served: %v", adapter)
		}
		if code, _, stderr := runServerComputeProviderCommand(now, "compute", "provider", "suspend", env, "reference", "--by=rota-a", "--reason=staging teardown"); code != 0 {
			tb.Fatal(stderr)
		}
		if adapter, err := registry.Enabled(ctx, computeservice.ComputeProviderIdReference); err == nil || adapter != nil {
			tb.Fatalf("a suspended reference provider served: %v", adapter)
		}
		if adapter, err := registry.Reachable(ctx, computeservice.ComputeProviderIdReference); err != nil || adapter == nil {
			tb.Fatalf("a suspended reference provider is not reachable for its VMs: %v", err)
		}
	})
}
