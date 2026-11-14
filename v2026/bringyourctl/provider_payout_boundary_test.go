// Plain db migrate is run through its docopt command line and dispatch against a
// private test database, and through injected steps for ordering and
// cancellation. Every schedule is a synthetic production-grammar declaration.
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/docopt/docopt-go"
	"github.com/urnetwork/server/v2026"
)

// The synthetic identity uses the production grammar without live accounts;
// the cutoff is the only earning field the tests vary.
func payoutBoundaryTestSchedule(cutoffUtc string) []byte {
	return []byte(fmt.Sprintf("schema: urnetwork-provider-payout-transition-v1\ncutoff_utc: %s\nattribution: settled_contract_close_time\nlegacy_usdc: finish_pre_cutoff_obligations\nmainnet:\n  profile: mainnet\n  chain_id: 964\n  genesis_hash: %q\n  netuid: 25\n  activation: blocked\n", cutoffUtc, "0x"+strings.Repeat("11", 32)))
}

// Parses plain `db migrate` with the real usage and runs its dispatch target,
// returning its stdout. A failed step panics through server.Raise, as in main.
func runDbMigrateCommand(t testing.TB) string {
	t.Helper()
	opts, err := (&docopt.Parser{HelpHandler: docopt.NoHelpHandler}).ParseArgs(bringyourctlUsage, []string{"db", "migrate"}, "test")
	if err != nil {
		t.Fatal(err)
	}
	verbose := server.DbMigrationVerbose
	defer func() { server.DbMigrationVerbose = verbose }()
	return captureStdout(t, func() { dbMigrate(opts) })
}

// The expected confirmation, built from the observed row rather than the output.
func payoutBoundaryTestLine(state string, binding *server.ProviderEarningBoundary) string {
	return fmt.Sprintf("%s provider earning boundary: cutoff_utc=%s identity_sha256=%s initial_config_sha256=%s prepared_at=%s\n",
		state, binding.CutoffUtc, binding.IdentitySha256, binding.InitialConfigSha256, binding.PreparedAt.UTC().Format(time.RFC3339Nano))
}

// Plain db migrate prepares a missing boundary from the loaded sn.yml, prints
// its one-line confirmation, and the boundary then admits workers.
func TestDbMigratePreparesMissingBoundaryFromLoadedSchedule(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		t.Cleanup(server.Config.PushSimpleResource("sn.yml", payoutBoundaryTestSchedule("2035-01-01T00:00:00Z")))
		policy, err := server.LoadProviderPayoutTransition(ctx)
		if err != nil || policy == nil {
			t.Fatal("synthetic schedule did not load", err)
		}
		output := runDbMigrateCommand(t)
		binding, err := server.RequireProviderPayoutBoundary(ctx, policy)
		if err != nil || binding == nil || binding.InitialConfigSha256 != policy.ConfigSha256 || binding.CutoffUtc != "2035-01-01T00:00:00Z" {
			t.Fatal("plain migration did not prepare the missing boundary", binding, err)
		}
		if want := "Applying DB migrations ...\n" + payoutBoundaryTestLine("Prepared", binding); output != want {
			t.Fatalf("migration output = %q, want %q", output, want)
		}
		if _, err := server.LoadProviderPayoutEarningPolicy(ctx); err != nil {
			t.Fatal("prepared boundary did not admit workers", err)
		}
	})
}

// After an earning edit to sn.yml, plain db migrate succeeds, leaves the boundary
// and its initial digest unchanged and confirms them, while workers still
// refuse the edited identity.
func TestDbMigrateRetainsExistingBoundaryAfterScheduleEdit(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		release := server.Config.PushSimpleResource("sn.yml", payoutBoundaryTestSchedule("2035-01-01T00:00:00Z"))
		policy, err := server.LoadProviderPayoutTransition(ctx)
		if err != nil || policy == nil {
			release()
			t.Fatal("synthetic schedule did not load", err)
		}
		first, err := server.PrepareProviderPayoutBoundary(ctx, policy.ConfigSha256)
		release()
		if err != nil || first == nil {
			t.Fatal(err)
		}
		t.Cleanup(server.Config.PushSimpleResource("sn.yml", payoutBoundaryTestSchedule("2035-01-02T00:00:00Z")))
		output := runDbMigrateCommand(t)
		if want := "Applying DB migrations ...\n" + payoutBoundaryTestLine("Retained", first); output != want {
			t.Fatalf("migration output = %q, want %q", output, want)
		}
		retained, err := server.RequireProviderPayoutBoundary(ctx, policy)
		if err != nil || retained == nil || retained.IdentitySha256 != first.IdentitySha256 || retained.InitialConfigSha256 != policy.ConfigSha256 || !retained.PreparedAt.Equal(first.PreparedAt) {
			t.Fatal("migration changed the retained boundary", retained, err)
		}
		if _, err := server.LoadProviderPayoutEarningPolicy(ctx); !errors.Is(err, server.ErrProviderEarningBoundaryMismatch) {
			t.Fatal("edited earning identity was admitted against the retained boundary", err)
		}
	})
}

// Without sn.yml (local or test environments without sn) plain db migrate
// prepares nothing, prints only its ordinary line and succeeds.
func TestDbMigrateWithoutSchedulePreparesNothing(t *testing.T) {
	t.Setenv("URNETWORK_ST_PROFILE", "")
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		if policy, err := server.LoadProviderPayoutTransition(ctx); err != nil || policy != nil {
			t.Fatal("fixture requires an environment without sn.yml", policy, err)
		}
		if output := runDbMigrateCommand(t); output != "Applying DB migrations ...\n" {
			t.Fatalf("migration without a schedule printed %q", output)
		}
		// With no schedule, any boundary row would be a mismatch.
		if binding, err := server.RequireProviderPayoutBoundary(ctx, nil); err != nil || binding != nil {
			t.Fatal("migration without a schedule prepared a boundary", binding, err)
		}
	})
}

// The removed transition option is absent from usage and refused by the
// parser, while plain db migrate still parses.
func TestDbMigrateUsageHasNoScheduleOption(t *testing.T) {
	if strings.Contains(bringyourctlUsage, "sn-schedule-sha256") {
		t.Fatal("usage still offers the removed schedule option")
	}
	parser := &docopt.Parser{HelpHandler: docopt.NoHelpHandler}
	if _, err := parser.ParseArgs(bringyourctlUsage, []string{"db", "migrate", "--sn-schedule-sha256=" + strings.Repeat("ab", 32)}, "test"); err == nil {
		t.Fatal("parser accepted the removed schedule option")
	}
	opts, err := parser.ParseArgs(bringyourctlUsage, []string{"db", "migrate"}, "test")
	if migrate, _ := opts.Bool("migrate"); err != nil || !migrate {
		t.Fatal("plain db migrate no longer parses", err)
	}
}

// Migrations precede the boundary step, whose prepared or retained result is
// the one confirmation line; no boundary writes nothing.
func TestPayoutMigrationOrdersBoundaryAfterSchemaWrites(t *testing.T) {
	binding := &server.ProviderEarningBoundary{IdentitySha256: strings.Repeat("cd", 32), InitialConfigSha256: strings.Repeat("ab", 32),
		CutoffUtc: "2035-01-01T00:00:00Z", PreparedAt: time.Date(2035, 1, 2, 0, 0, 0, 0, time.UTC)}
	for _, c := range []struct {
		binding  *server.ProviderEarningBoundary
		prepared bool
		want     string
	}{
		{binding: binding, prepared: true, want: payoutBoundaryTestLine("Prepared", binding)},
		{binding: binding, prepared: false, want: payoutBoundaryTestLine("Retained", binding)},
		{binding: nil, prepared: false, want: ""},
	} {
		var events []string
		var output bytes.Buffer
		err := migrateWithPayoutBoundary(t.Context(), &output,
			func(context.Context) { events = append(events, "migrate") },
			func(context.Context) (*server.ProviderEarningBoundary, bool, error) {
				events = append(events, "ensure")
				return c.binding, c.prepared, nil
			})
		if err != nil || !slices.Equal(events, []string{"migrate", "ensure"}) || output.String() != c.want {
			t.Errorf("migration prepared=%v: err=%v events=%v output=%q want %q", c.prepared, err, events, output.String(), c.want)
		}
	}
}

// A boundary step failure fails the command after its migrations and writes
// no confirmation.
func TestPayoutMigrationPropagatesBoundaryFailure(t *testing.T) {
	cause := errors.New("synthetic boundary refusal")
	migrated := false
	var output bytes.Buffer
	err := migrateWithPayoutBoundary(t.Context(), &output,
		func(context.Context) { migrated = true },
		func(context.Context) (*server.ProviderEarningBoundary, bool, error) {
			if !migrated {
				t.Error("boundary step ran before migrations")
			}
			return nil, false, cause
		})
	if !errors.Is(err, cause) || !migrated || output.Len() != 0 {
		t.Errorf("boundary failure: err=%v migrated=%v output=%q", err, migrated, output.String())
	}
}

// A canceled or missing owner context stops before schema writes, and
// cancellation during migration stops before the boundary step.
func TestPayoutMigrationCanceledOwnerStopsBeforeEachStep(t *testing.T) {
	for _, cancelAt := range []string{"before-migrate", "during-migrate"} {
		ctx, cancel := context.WithCancel(t.Context())
		if cancelAt == "before-migrate" {
			cancel()
		}
		var events []string
		var output bytes.Buffer
		err := migrateWithPayoutBoundary(ctx, &output,
			func(context.Context) {
				events = append(events, "migrate")
				cancel()
			},
			func(context.Context) (*server.ProviderEarningBoundary, bool, error) {
				events = append(events, "ensure")
				return nil, false, nil
			})
		cancel()
		want := map[string][]string{"before-migrate": nil, "during-migrate": {"migrate"}}[cancelAt]
		if !errors.Is(err, context.Canceled) || !slices.Equal(events, want) || output.Len() != 0 {
			t.Errorf("cancel %s: err=%v events=%v output=%q", cancelAt, err, events, output.String())
		}
	}
	called := false
	err := migrateWithPayoutBoundary(nil, &bytes.Buffer{},
		func(context.Context) { called = true },
		func(context.Context) (*server.ProviderEarningBoundary, bool, error) {
			called = true
			return nil, false, nil
		})
	if err == nil || called {
		t.Errorf("missing owner context: err=%v called=%v", err, called)
	}
}
