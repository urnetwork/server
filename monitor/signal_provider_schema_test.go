package monitor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

type providerSchemaTestCalls struct {
	schema atomic.Int32
	data   atomic.Int32
	remote atomic.Int32
}

func providerSchemaTestContracts() []probeSchemaContract {
	return []probeSchemaContract{egressSitePoolSchemaContract(), derivedLocationsSchemaContract(), hmacCutoverSchemaContract()}
}

// The old source compiles and reaches the forbidden dependent query. Only the
// new catalog-first path can return an observed missing-schema warning.
func providerSchemaFixture(t *testing.T, index int, rows []Row, schemaErr error, forbidData bool) (Signal, SignalSettings, *providerSchemaTestCalls) {
	t.Helper()
	var signal Signal
	var settings SignalSettings
	var source *syntheticSource
	switch index {
	case 0:
		source = healthyEgressSitePoolFixture().source(t)
		signal = testEgressSitePoolSignal(testEgressSitePoolContext())
		settings = testEgressSitePoolSettings(source)
	case 1:
		source = healthyDerivedLocationsFixture().source(t)
		signal = NewDerivedLocationsSignal()
		settings = syntheticSettings(source)
	case 2:
		source = syntheticHMACCutoverSource(Row{"t", "86400", "100", "0", "0", "100", "0", "0", "0", "0", "100", "0", "100"})
		signal = NewHMACCutoverSignal()
		settings = syntheticSettings(source)
	default:
		t.Fatal("unknown synthetic schema case")
	}
	calls := &providerSchemaTestCalls{}
	pg := source.postgresFn
	host := source.hostFn
	redis := source.redisFn
	source.postgresFn = func(query string) ([]Row, error) {
		if strings.Contains(query, "monitor-schema-readiness") {
			calls.schema.Add(1)
			return rows, schemaErr
		}
		calls.data.Add(1)
		if forbidData {
			return nil, errors.New("synthetic missing relation or column")
		}
		return pg(query)
	}
	source.hostFn = func(h HostSettings, command string) (string, error) {
		calls.remote.Add(1)
		if forbidData || host == nil {
			return "", errors.New("synthetic forbidden remote read")
		}
		return host(h, command)
	}
	source.redisFn = func(h HostSettings, port int, args ...string) (string, error) {
		calls.remote.Add(1)
		if forbidData || redis == nil {
			return "", errors.New("synthetic forbidden history read")
		}
		return redis(h, port, args...)
	}
	return signal, settings, calls
}

func requireProviderSchemaMissing(t *testing.T, index int) {
	t.Helper()
	signal, settings, calls := providerSchemaFixture(t, index, []Row{{"false"}}, nil, true)
	alerts, err := NewWithSignals(settings, signal).Run(context.Background())
	if err != nil {
		t.Fatalf("absent schema still aborts the bounded one-shot: %v", err)
	}
	schema := providerSchemaTestContracts()[index]
	if len(alerts) != 1 || calls.schema.Load() != 1 || calls.data.Load() != 0 || calls.remote.Load() != 0 {
		t.Fatal("schema absence was hidden or dependent source reads were issued")
	}
	alert := requireAlertClass(t, alerts, schema.class)
	if alert.Severity != SeverityWarn {
		t.Fatal("staged readiness is not a service outage PAGE")
	}
	for _, want := range []string{"signal_readiness=unknown", "dependent_data_queries=0", "not proof", "Do not apply migrations", "No healthy finding", "deployed artifacts"} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("schema warning missing %q", want)
		}
	}
}

func TestProviderSchemaEgressSiteMissingCompletesOneShot(t *testing.T) {
	requireProviderSchemaMissing(t, 0)
}
func TestProviderSchemaDerivedMissingCompletesOneShot(t *testing.T) {
	requireProviderSchemaMissing(t, 1)
}
func TestProviderSchemaHMACMissingCompletesOneShot(t *testing.T) { requireProviderSchemaMissing(t, 2) }

// All original healthy data paths still execute; the preflight cannot replace
// their real data with synthetic healthy aggregates.
func TestProviderSchemaPresentHealthyControls(t *testing.T) {
	for index := range providerSchemaTestContracts() {
		signal, settings, calls := providerSchemaFixture(t, index, []Row{{"true"}}, nil, false)
		alerts, err := signal.Run(context.Background(), settings)
		if err != nil || len(alerts) != 0 || calls.data.Load() == 0 {
			t.Fatalf("ready healthy control changed at %d: alerts=%d error=%v", index, len(alerts), err)
		}
	}
}

func TestProviderSchemaPresentRetainsViolations(t *testing.T) {
	signal, settings, _ := providerSchemaFixture(t, 2, []Row{{"true"}}, nil, false)
	source := settings.Source.(*syntheticSource)
	base := source.postgresFn
	source.postgresFn = func(query string) ([]Row, error) {
		if strings.Contains(query, "monitor-schema-readiness") {
			return base(query)
		}
		return []Row{{"t", "86400", "40", "20", "4", "20", "0", "20", "20", "0", "20", "0", "20"}}, nil
	}
	alerts, err := signal.Run(context.Background(), settings)
	if err != nil || requireAlertClass(t, alerts, "contract-hmac-incompatible").Severity != SeverityPage {
		t.Fatal("schema readiness suppressed an established cohort violation")
	}
}

func TestProviderSchemaMalformedPreflightFailsClosed(t *testing.T) {
	for index := range providerSchemaTestContracts() {
		for _, rows := range [][]Row{nil, {{"true"}, {"false"}}, {{"true", "extra"}}, {{"synthetic-private-content"}}} {
			signal, settings, calls := providerSchemaFixture(t, index, rows, nil, true)
			alerts, err := signal.Run(context.Background(), settings)
			if err == nil || len(alerts) != 0 || calls.data.Load() != 0 || calls.remote.Load() != 0 ||
				strings.Contains(err.Error(), "synthetic-private-content") {
				t.Fatal("malformed catalog response became absence, health, data access or raw disclosure")
			}
		}
	}
}

func TestProviderSchemaReadErrorsRemainErrors(t *testing.T) {
	for index := range providerSchemaTestContracts() {
		expected := errors.New("synthetic catalog unavailable")
		signal, settings, calls := providerSchemaFixture(t, index, nil, expected, true)
		alerts, err := NewWithSignals(settings, signal).Run(context.Background())
		if !errors.Is(err, expected) || calls.data.Load() != 0 || calls.remote.Load() != 0 {
			t.Fatal("catalog read error was converted into an observed schema state")
		}
		for _, alert := range alerts {
			if alert.Class == providerSchemaTestContracts()[index].class {
				t.Fatal("unreadable metadata was called absent")
			}
		}
	}
}

func TestProviderSchemaCancellationStopsDependentReads(t *testing.T) {
	for index := range providerSchemaTestContracts() {
		signal, settings, calls := providerSchemaFixture(t, index, []Row{{"true"}}, nil, true)
		ctx, cancel := context.WithCancel(context.Background())
		source := settings.Source.(*syntheticSource)
		base := source.postgresFn
		source.postgresFn = func(query string) ([]Row, error) {
			rows, err := base(query)
			cancel()
			return rows, err
		}
		alerts, err := signal.Run(ctx, settings)
		cancel()
		if !errors.Is(err, context.Canceled) || len(alerts) != 0 || calls.data.Load() != 0 || calls.remote.Load() != 0 {
			t.Fatal("canceled preflight admitted dependent reads or health")
		}
	}
}

func TestProviderSchemaUnavailableDoesNotResolveTickets(t *testing.T) {
	classes := []string{"egress-prober-fault", "derive-supply-gone", "contract-hmac-incompatible"}
	for index, schema := range providerSchemaTestContracts() {
		manager := newTicketManager("synthetic", &ticketEscalationEmitter{})
		manager.resolveTicks = 3
		original := finding{probeId: schema.probeId, tier: tierWarn, class: classes[index], target: "synthetic-target", sustain: 1}
		manager.ingest(context.Background(), []finding{original})
		signal, settings, _ := providerSchemaFixture(t, index, []Row{{"false"}}, nil, true)
		env, err := newProbeEnv(settings.withDefaults())
		if err != nil {
			t.Fatal(err)
		}
		for range manager.resolveTicks {
			findings, err := signal.(*signalAdapter).probe.check(context.Background(), env)
			if err != nil {
				t.Fatal("known schema absence is not a complete observation")
			}
			for _, finding := range findings {
				if finding.healthy {
					t.Fatal("schema absence fabricated a healthy sentinel")
				}
			}
			manager.ingest(context.Background(), findings)
		}
		open := false
		for _, ticket := range manager.tickets {
			open = open || ticket.open && ticket.class == classes[index]
		}
		if !open {
			t.Fatal("schema absence resolved an unobserved prior incident")
		}
	}
}

func TestProviderSchemaReadyCannotHideDataFailure(t *testing.T) {
	for index := range providerSchemaTestContracts() {
		signal, settings, calls := providerSchemaFixture(t, index, []Row{{"true"}}, nil, true)
		alerts, err := signal.Run(context.Background(), settings)
		if err == nil || len(alerts) != 0 || calls.data.Load() != 1 {
			t.Fatal("positive catalog preflight hid a later source/DDL-race failure")
		}
	}
}

func TestProviderSchemaPrerequisitesIncludeLaterDependencies(t *testing.T) {
	for _, control := range []struct {
		schema probeSchemaContract
		want   []string
	}{
		{schema: egressSitePoolSchemaContract(), want: []string{"provider_egress_site_tally", "provider_egress_place_tally", "next_due_at", "consecutive_failures", "class_results", "incompatible"}},
		{schema: derivedLocationsSchemaContract(), want: []string{"derived_location", "network_ping", "create_time", "network_ping_hour_tally", "cosign_reason", "beyond_half_planet_count", "pending_task", "network_extender"}},
		{schema: hmacCutoverSchemaContract(), want: []string{"consecutive_failures", "first_failed_at", "failure", "checked_at", "description"}},
	} {
		query := probeSchemaQuery(control.schema)
		for _, want := range control.want {
			if !strings.Contains(query, "'"+want+"'") {
				t.Fatalf("missing prerequisite %q", want)
			}
		}
		if strings.Contains(query, "::regclass") || strings.Contains(query, "FROM provider_") || strings.Contains(query, "FROM derived_location") {
			t.Fatal("preflight itself references optional data or strict relation casts")
		}
	}
}

// Execute the exact metadata predicate against invented catalog rows in the
// attested local PostgreSQL session. No real table/column metadata is read, no
// DDL is issued, and each expected column is removed in turn (partial rollout).
func TestProviderSchemaSqlRejectsMissingPartialAndLookalikes(t *testing.T) {
	if os.Getenv("WARP_ENV") != "local" {
		t.Fatal("schema SQL controls require the attested local environment")
	}
	for _, schema := range providerSchemaTestContracts() {
		query := strings.ReplaceAll(probeSchemaQuery(schema), "information_schema.columns", "synthetic_columns")
		values := make([]string, len(schema.columns))
		for i, column := range schema.columns {
			values[i] = fmt.Sprintf("('public', '%s', '%s', '%s')", column.table, column.column, column.dataType)
		}
		read := func(rows []string, want bool) {
			t.Helper()
			input := "SELECT NULL::text, NULL::text, NULL::text, NULL::text WHERE false"
			if len(rows) > 0 {
				input = "VALUES " + strings.Join(rows, ",")
			}
			sql := "WITH synthetic_columns(table_schema,table_name,column_name,data_type) AS (" + input + ") " + query
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			var actual bool
			server.Db(ctx, func(conn server.PgConn) {
				if err := conn.QueryRow(ctx, sql).Scan(&actual); err != nil {
					t.Fatal(err)
				}
			}, server.OptNoRetry(), server.OptReadOnly())
			if actual != want {
				t.Fatal("metadata shape control changed readiness")
			}
		}
		read(values, true)
		read(nil, false)
		for missing := range values {
			partial := append([]string{}, values[:missing]...)
			partial = append(partial, values[missing+1:]...)
			read(partial, false)
		}
		for _, wrong := range []string{
			"('private', 'synthetic', 'synthetic', 'text')",
			fmt.Sprintf("('public', '%s', '%s', 'synthetic-wrong-type')", schema.columns[0].table, schema.columns[0].column),
			fmt.Sprintf("('private', '%s', '%s', '%s')", schema.columns[0].table, schema.columns[0].column, schema.columns[0].dataType),
		} {
			lookalike := append([]string{}, values...)
			lookalike[0] = wrong
			read(lookalike, false)
		}
	}
}

func TestProviderSchemaCatalogPreservesStagedUnknown(t *testing.T) {
	data, err := os.ReadFile("SIGNALS.md")
	if err != nil {
		t.Fatal(err)
	}
	catalog := string(data)
	for _, schema := range providerSchemaTestContracts() {
		start := strings.Index(catalog, "### "+schema.section+" ")
		if start < 0 {
			t.Fatal("missing catalog section")
		}
		end := strings.Index(catalog[start+1:], "\n### ")
		if end < 0 {
			t.Fatal("missing next catalog boundary")
		}
		section := strings.Join(strings.Fields(catalog[start:start+1+end]), " ")
		for _, want := range []string{schema.class, "readiness unknown", "No dependent healthy sentinel", "not permission to migrate", "Read errors, malformed output and cancellation still fail closed"} {
			if !strings.Contains(section, want) {
				t.Fatalf("section %s missing %q", schema.section, want)
			}
		}
	}
}

func TestProviderSchemaReadyKeepsMimirVisibilityUnknown(t *testing.T) {
	signal, settings, _ := providerSchemaFixture(t, 0, []Row{{"true"}}, nil, false)
	settings.Source.(*syntheticSource).hostFn = func(HostSettings, string) (string, error) {
		return "", errors.New("synthetic Mimir unavailable")
	}
	env, err := newProbeEnv(settings.withDefaults())
	if err != nil {
		t.Fatal(err)
	}
	findings, err := signal.(*signalAdapter).probe.check(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	schemaReady, metricsUnknown := false, false
	for _, f := range findings {
		schemaReady = schemaReady || f.class == "egress-site-pool-schema-unavailable" && f.healthy
		metricsUnknown = metricsUnknown || f.class == "egress-site-pool-unobservable" && !f.healthy
		if f.healthy && (f.class == "egress-prober-fault" || f.class == "egress-backfill-sustained") {
			t.Fatal("physical schema readiness certified unknown Mimir-dependent health")
		}
	}
	if !schemaReady || !metricsUnknown {
		t.Fatal("schema-only recovery lost the independent visibility gap")
	}
}

func TestProviderSchemaReadyKeepsRedisHistoryUnknown(t *testing.T) {
	signal, settings, _ := providerSchemaFixture(t, 1, []Row{{"true"}}, nil, false)
	settings.Source.(*syntheticSource).redisFn = func(HostSettings, int, ...string) (string, error) {
		return "", errors.New("synthetic Redis history unavailable")
	}
	env, err := newProbeEnv(settings.withDefaults())
	if err != nil {
		t.Fatal(err)
	}
	findings, err := signal.(*signalAdapter).probe.check(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	schemaReady, historyUnknown := false, false
	for _, f := range findings {
		schemaReady = schemaReady || f.class == "derived-locations-schema-unavailable" && f.healthy
		historyUnknown = historyUnknown || f.class == "derive-run-history-unobservable" && !f.healthy
		if f.healthy && (f.class == "derive-published-collapse" || f.class == "derive-residual-not-improving" || f.class == "derive-capacity") {
			t.Fatal("physical schema readiness certified unknown history-dependent health")
		}
	}
	if !schemaReady || !historyUnknown {
		t.Fatal("schema-only recovery lost the independent history gap")
	}
}
