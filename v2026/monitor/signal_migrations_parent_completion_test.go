package monitor

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026"
)

// Supplies the same complete current-head artifacts and immutable identities
// used by the ordinary healthy control, through the actual public pg seam.
type migrationsCompletionSource struct {
	*syntheticSource
	t             *testing.T
	artifactRows  []Row
	catalogRows   []Row
	artifactReads int
	catalogReads  int
	catalogReturn func(context.Context) ([]Row, error)
}

func newMigrationsCompletionSource(t *testing.T) *migrationsCompletionSource {
	t.Helper()
	head := server.MigrationCount()
	catalog := syntheticMigrationCatalogRows(head)
	if len(catalog) != head {
		t.Fatal("completion fixture lacks the complete identity catalog")
	}
	for index, row := range catalog {
		if len(row) != 2 {
			t.Fatal("completion fixture contains an invalid identity row")
		}
		matches, err := server.MigrationIdentityMatches(index, row[1])
		if err != nil || !matches {
			t.Fatal("completion fixture does not match the source-known identity prefix")
		}
	}
	return &migrationsCompletionSource{
		syntheticSource: &syntheticSource{}, t: t,
		artifactRows: []Row{syntheticMigrationArtifactRow(head)}, catalogRows: catalog,
	}
}

func (self *migrationsCompletionSource) PostgreSQL(ctx context.Context, query string) ([]Row, error) {
	if strings.Contains(query, "FROM migration_catalog") {
		self.catalogReads++
		if self.artifactReads != 1 || self.catalogReads != 1 {
			self.t.Fatal("completion fixture did not reach exactly one final catalog read")
		}
		if self.catalogReturn != nil {
			return self.catalogReturn(ctx)
		}
		return self.catalogRows, nil
	}
	if !strings.Contains(query, "WITH version AS") || !strings.Contains(query, "FROM migration_audit") {
		self.t.Fatal("completion fixture received an unexpected source query")
	}
	self.artifactReads++
	if self.artifactReads != 1 || self.catalogReads != 0 {
		self.t.Fatal("completion fixture did not begin with exactly one artifact read")
	}
	return self.artifactRows, nil
}

func (self *migrationsCompletionSource) settings(scoped bool) SignalSettings {
	settings := syntheticSettings(self)
	settings.Hosts = []HostSettings{{Name: "synthetic-primary.example", Roles: []string{"pg-primary"}}}
	if scoped {
		settings.Hosts = append(settings.Hosts, HostSettings{Name: "synthetic-excluded.example"})
		settings.ExcludedHosts = []string{"synthetic-excluded.example"}
	}
	return settings
}

func (self *migrationsCompletionSource) requireCompleteReads() {
	self.t.Helper()
	if self.artifactReads != 1 || self.catalogReads != 1 {
		self.t.Fatal("migration completion did not consume exactly one artifact and one identity catalog read")
	}
}

func TestMigrationsParentCompletionLiveCoherentHead(t *testing.T) {
	for _, scoped := range []bool{false, true} {
		source := newMigrationsCompletionSource(t)
		ctx, cancel := context.WithCancel(context.Background())
		alerts, err := NewMigrationsSignal().Run(ctx, source.settings(scoped))
		source.requireCompleteReads()
		if err != nil || len(alerts) != 0 || ctx.Err() != nil {
			t.Fatal("complete current-head live control did not remain healthy")
		}
		cancel()
	}
}

func TestMigrationsParentCompletionRejectsLateValidCatalog(t *testing.T) {
	for _, scoped := range []bool{false, true} {
		source := newMigrationsCompletionSource(t)
		ctx, cancel := context.WithCancel(context.Background())
		endedAtReturn := false
		source.catalogReturn = func(sourceCtx context.Context) ([]Row, error) {
			cancel()
			<-sourceCtx.Done()
			endedAtReturn = errors.Is(sourceCtx.Err(), context.Canceled)
			return source.catalogRows, nil
		}
		alerts, err := NewMigrationsSignal().Run(ctx, source.settings(scoped))
		source.requireCompleteReads()
		if !endedAtReturn || !errors.Is(ctx.Err(), context.Canceled) {
			t.Fatal("late-catalog control did not actually end its owning parent")
		}
		if !errors.Is(err, context.Canceled) || len(alerts) != 0 {
			t.Fatal("ended migrations parent accepted late valid catalog as healthy completion")
		}
		cancel()
	}
}

func TestMigrationsParentCompletionAlreadyEndedHasNoSourceEntry(t *testing.T) {
	for _, scoped := range []bool{false, true} {
		source := newMigrationsCompletionSource(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		alerts, err := NewMigrationsSignal().Run(ctx, source.settings(scoped))
		if !errors.Is(err, context.Canceled) || len(alerts) != 0 || source.artifactReads != 0 || source.catalogReads != 0 {
			t.Fatal("already-ended migrations parent entered a source or accepted completion")
		}
	}
}

func TestMigrationsParentCompletionLiveSourceErrorsRetainIdentity(t *testing.T) {
	for _, scoped := range []bool{false, true} {
		for _, sourceErr := range []error{errors.New("synthetic-private-migration-source-error"), context.Canceled} {
			source := newMigrationsCompletionSource(t)
			ctx, cancel := context.WithCancel(context.Background())
			source.catalogReturn = func(context.Context) ([]Row, error) { return nil, sourceErr }
			alerts, err := NewMigrationsSignal().Run(ctx, source.settings(scoped))
			source.requireCompleteReads()
			if !errors.Is(err, sourceErr) || len(alerts) != 0 || ctx.Err() != nil {
				t.Fatal("live source error lost identity, became health, or ended the parent")
			}
			cancel()
		}
	}
}

func TestMigrationsParentCompletionEndedSourceErrorKeepsPrivateCause(t *testing.T) {
	for _, scoped := range []bool{false, true} {
		source := newMigrationsCompletionSource(t)
		ctx, cancel := context.WithCancel(context.Background())
		private := errors.New("synthetic-private-final-migration-error")
		source.catalogReturn = func(sourceCtx context.Context) ([]Row, error) {
			cancel()
			<-sourceCtx.Done()
			return source.catalogRows, private
		}
		alerts, err := NewMigrationsSignal().Run(ctx, source.settings(scoped))
		source.requireCompleteReads()
		if !errors.Is(err, context.Canceled) || !errors.Is(err, private) || len(alerts) != 0 {
			t.Fatal("ended migrations source error lost parent or private source cause")
		}
		if err.Error() != context.Canceled.Error() {
			t.Fatal("ended migrations completion rendered private source text")
		}
		cancel()
	}
}

// Healthy and interrupted executions use identical bounded rows. The owning
// probe may retain independently established faults, but both live-entry public
// adapters withhold publication after their owner ends. Live controls keep the
// original positive/unknown classes, severity and sustain unchanged.
func TestMigrationsParentCompletionPreservesPositiveAndUnknownControls(t *testing.T) {
	for _, kind := range []string{"artifact-fault", "identity-fault", "unknown-successor"} {
		for _, scoped := range []bool{false, true} {
			for _, interrupted := range []bool{false, true} {
				source := newMigrationsCompletionSource(t)
				wantClass, wantSeverity, wantSustain := "migration-schema-drift", SeverityPage, 1
				switch kind {
				case "artifact-fault":
					source.artifactRows = []Row{syntheticMigrationMissingArtifactRow(t, server.MigrationCount(), "transfer_escrow_balance_contract")}
				case "identity-fault":
					source.catalogRows[0][1] = strings.Repeat("0", 64)
				case "unknown-successor":
					source.artifactRows = []Row{syntheticMigrationArtifactRow(server.MigrationCount() + 1)}
					source.catalogRows = migrationFutureSchemaCatalogRows()
					wantClass, wantSeverity, wantSustain = "cannot-observe", SeverityWarn, 2
				}
				ctx, cancel := context.WithCancel(context.Background())
				if interrupted {
					source.catalogReturn = func(sourceCtx context.Context) ([]Row, error) {
						cancel()
						<-sourceCtx.Done()
						return source.catalogRows, nil
					}
				}
				alerts, err := NewMigrationsSignal().Run(ctx, source.settings(scoped))
				source.requireCompleteReads()
				if interrupted {
					if !errors.Is(err, context.Canceled) || len(alerts) != 0 {
						t.Fatal("live-entry migration cancellation policy changed")
					}
				} else {
					if err != nil || len(alerts) != 1 {
						t.Fatal("migrations completion erased an independent positive or unknown")
					}
					alert := requireAlertClass(t, alerts, wantClass)
					if alert.Severity != wantSeverity || alert.Sustain != wantSustain {
						t.Fatal("migrations completion changed positive or unknown severity/sustain")
					}
				}
				cancel()
			}
		}
	}
}

func TestMigrationsParentCompletionInterruptedPositiveHasNoHealthySentinel(t *testing.T) {
	source := newMigrationsCompletionSource(t)
	source.artifactRows = []Row{syntheticMigrationMissingArtifactRow(t, server.MigrationCount(), "transfer_escrow_balance_contract")}
	ctx, cancel := context.WithCancel(context.Background())
	source.catalogReturn = func(sourceCtx context.Context) ([]Row, error) {
		cancel()
		<-sourceCtx.Done()
		return source.catalogRows, nil
	}
	env, err := newProbeEnv(source.settings(false))
	if err != nil {
		t.Fatal("interrupted positive fixture could not construct the owning environment")
	}
	findings, err := (migrationsProbe{}).check(ctx, env)
	source.requireCompleteReads()
	if err != nil {
		t.Fatal("interrupted owning reduction erased its independent drift finding")
	}
	drift := false
	for _, finding := range findings {
		if finding.healthy {
			t.Fatal("interrupted migrations reduction retained a healthy sentinel beside a positive")
		}
		drift = drift || finding.class == "migration-schema-drift"
	}
	if !drift || len(findings) != 1 {
		t.Fatal("interrupted owning reduction erased or changed its independent drift finding")
	}
	cancel()
}
