// SIGNALS.md §1.3b: exercise the public signal, renderer and history paths
// with synthetic sources while retaining legacy identity and private-data refusal.
package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Provider routing canary if its original type escapes.
type poolRetentionProviderCanary struct{}

// Supply synthetic private source text for omission controls.
func (*poolRetentionProviderCanary) Error() string { return "private-pool-source-canary" }

// Supply a provider-only routing canary, not pool authority.
func (*poolRetentionProviderCanary) monitorVisibilityClass() string { return providerDataClass }

// Detect source-error reads after metadata preparation.
type poolRetentionMutableCanary struct {
	text  string
	reads int
}

// Count reads to detect retained source access.
func (self *poolRetentionMutableCanary) Error() string { self.reads++; return self.text }

// Exercise the public signal with one injected PG read.
func runPoolRetentionDiagnostic(t *testing.T, rows []Row, sourceErr error) (SignalSettings, Signal, Alerts, error) {
	t.Helper()
	calls := 0
	settings := syntheticSettings(&syntheticSource{postgresFn: func(string) ([]Row, error) { calls++; return rows, sourceErr }})
	signal := NewPoolRetentionSignal()
	alerts, err := signal.Run(context.Background(), settings)
	if calls != 1 {
		t.Fatal("pool observation changed single-read policy")
	}
	return settings, signal, alerts, err
}

// Compare the full legacy alert contract except its allowed suffix.
func requirePoolRetentionLegacyVisibility(t *testing.T, settings SignalSettings, signal Signal, err error, legacyText string, fields []string) Alert {
	t.Helper()
	if err == nil || err.Error() != legacyText {
		t.Fatal("legacy normalized returned error changed")
	}
	alert := visibilityAlert(settings, signal, err)
	baseline := visibilityAlert(settings, signal, errors.New(legacyText))
	if alert.Class != "cannot-observe" || alert.Target != "pg/pool-retention" || alert.Severity != SeverityWarn || alert.Sustain != 2 || !alert.unobservable {
		t.Fatal("prepared metadata changed legacy visibility identity")
	}
	if !strings.HasPrefix(alert.Observed, baseline.Observed+" ") {
		t.Fatal("legacy observation prefix changed")
	}
	trimmed := alert
	trimmed.Observed = baseline.Observed
	if !reflect.DeepEqual(trimmed, baseline) {
		t.Fatal("prepared projection changed fields outside Observed")
	}
	for _, field := range fields {
		if !strings.Contains(alert.Observed, field) {
			t.Errorf("missing finite field %s", field)
		}
	}
	legacyClassFields := 0
	for _, field := range strings.Fields(alert.Observed) {
		if strings.HasPrefix(field, "error_class=") {
			legacyClassFields++
		}
	}
	if legacyClassFields != 1 {
		t.Fatal("prepared metadata duplicated legacy error_class")
	}
	encoded, marshalErr := json.Marshal(alert)
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	for _, private := range []string{"private-pool-source-canary", "private-pool-row-canary", "secret.example", "credential-canary", "SELECT private_column"} {
		if strings.Contains(err.Error(), private) || strings.Contains(string(encoded), private) || strings.Contains(alert.Markdown(), private) {
			t.Fatal("private source or row text escaped")
		}
	}
	return alert
}

// Check returned typed facts without routing the original cause.
func TestPoolRetentionPreparedReadMetadataPreservesLegacyIdentity(t *testing.T) {
	original := errors.Join(
		&sshAdmissionUnavailableError{err: errors.New("private-pool-source-canary"), phase: sshAdmissionFailureAcquire},
		&observationBoundaryError{err: errors.New("private-pool-source-canary"), boundary: observationBoundaryNativeWait},
	)
	settings, signal, alerts, err := runPoolRetentionDiagnostic(t, nil, original)
	if len(alerts) != 0 {
		t.Fatal("read error became a completed measurement")
	}
	requirePoolRetentionLegacyVisibility(t, settings, signal, err, poolRetentionUnavailableText, []string{"pool_retention_origin=read_error", "pool_retention_source_error_class=observation-admission-unavailable", "ssh_admission_failures=acquire", "execution_boundaries=native-wait/started-joined"})
	if errors.Is(err, original) {
		t.Fatal("original joined error survived carrier")
	}
	var source *sshAdmissionUnavailableError
	if errors.As(err, &source) {
		t.Fatal("original typed cause exposed to generic routing")
	}
}

// Keep provider and SSH candidates on the legacy pool path.
func TestPoolRetentionPreparedSourceCannotActivateSpecialRouting(t *testing.T) {
	for _, original := range []error{
		&poolRetentionProviderCanary{},
		errors.New("synthetic-edge.example: exit status 255: kex_exchange_identification: read: Connection reset by peer\nConnection reset by 192.0.2.91 port 22"),
	} {
		settings, signal, alerts, err := runPoolRetentionDiagnostic(t, nil, original)
		if len(alerts) != 0 {
			t.Fatal("failure unexpectedly returned measurements")
		}
		if visibilityAlert(settings, signal, original).Class == "cannot-observe" {
			t.Fatal("fixture does not exercise provider or SSH special routing")
		}
		requirePoolRetentionLegacyVisibility(t, settings, signal, err, poolRetentionUnavailableText, []string{"pool_retention_origin=read_error"})
		if errors.Is(err, original) {
			t.Fatal("original error re-exposed")
		}
	}
}

// Reject forged text and malformed typed metadata.
func TestPoolRetentionPreparedUntypedAndMalformedMarkersStayFinite(t *testing.T) {
	for _, original := range []error{
		errors.New("private-pool-source-canary https://secret.example/?key=credential-canary SELECT private_column native-wait/started-joined ssh_admission_failures=acquire"),
		&warpctlLogQueryError{class: "private-pool-source-canary", err: errors.New("private-pool-source-canary")},
		&observationBoundaryError{err: &sshAdmissionUnavailableError{err: errors.New("private-pool-source-canary"), phase: sshAdmissionFailurePhase(255)}, boundary: observationBoundary(255)},
	} {
		settings, signal, _, err := runPoolRetentionDiagnostic(t, nil, original)
		alert := requirePoolRetentionLegacyVisibility(t, settings, signal, err, poolRetentionUnavailableText, []string{"pool_retention_origin=read_error"})
		if strings.Contains(alert.Observed, "native-wait/started-joined") || strings.Contains(alert.Observed, "ssh_admission_failures=acquire") {
			t.Fatal("malformed or textual marker became native proof")
		}
	}
}

// Separate row rejection from read failure without exposing cells.
func TestPoolRetentionPreparedRejectedRowsPreserveLegacyErrors(t *testing.T) {
	for _, rows := range [][]Row{
		nil,
		{{"private-pool-row-canary"}},
		poolRetentionRows("1021", "300", "200", "private-pool-row-canary", "20", "1", "0", "10"),
		poolRetentionRows("1021", "300", "200", "-1", "20", "1", "0", "10"),
		poolRetentionRows("1021", "300", "200", "190", "20", "1", "0", "10"),
		{{"1"}, {"2"}},
	} {
		converted := make([]pgRow, len(rows))
		for i, row := range rows {
			converted[i] = pgRow(row)
		}
		_, legacy := parsePoolRetentionRows(converted)
		if legacy == nil {
			t.Fatal("fixture must reject before measurement")
		}
		settings, signal, alerts, err := runPoolRetentionDiagnostic(t, rows, nil)
		if len(alerts) != 0 {
			t.Fatal("rejected row became a measurement")
		}
		alert := requirePoolRetentionLegacyVisibility(t, settings, signal, err, legacy.Error(), []string{"pool_retention_origin=response_rejected"})
		for _, forbidden := range []string{"pool_retention_source_error_class=", "ssh_admission_failures=", "execution_boundaries="} {
			if strings.Contains(alert.Observed, forbidden) {
				t.Fatal("row rejection borrowed source failure metadata")
			}
		}
	}
}

// Check immutable snapshots and absence of error-chain hooks.
func TestPoolRetentionPreparedSnapshotForgetsOriginalError(t *testing.T) {
	original := &poolRetentionMutableCanary{text: "private-pool-source-canary"}
	settings, signal, _, err := runPoolRetentionDiagnostic(t, nil, original)
	reads := original.reads
	original.text = "timeout private-pool-source-canary"
	alert := requirePoolRetentionLegacyVisibility(t, settings, signal, err, poolRetentionUnavailableText, []string{"pool_retention_source_error_class=observation-unclassified"})
	if original.reads != reads || strings.Contains(alert.Observed, "observation-timeout") {
		t.Fatal("prepared projection reread mutable source")
	}
	type unwrapper interface{ Unwrap() error }
	type multiUnwrapper interface{ Unwrap() []error }
	type asOverride interface{ As(any) bool }
	type isOverride interface{ Is(error) bool }
	if _, ok := err.(unwrapper); ok {
		t.Fatal("carrier has Unwrap")
	}
	if _, ok := err.(multiUnwrapper); ok {
		t.Fatal("carrier has multi-Unwrap")
	}
	if _, ok := err.(asOverride); ok {
		t.Fatal("carrier has As")
	}
	if _, ok := err.(isOverride); ok {
		t.Fatal("carrier has Is")
	}
}

// Preserve cancellation precedence and late-publication refusal.
func TestPoolRetentionPreparedLifecycleBranchesRemainLegacy(t *testing.T) {
	for _, original := range []error{context.Canceled, context.DeadlineExceeded} {
		settings, signal, _, err := runPoolRetentionDiagnostic(t, nil, original)
		if !errors.Is(err, original) {
			t.Fatal("legacy lifecycle cause changed")
		}
		alert := visibilityAlert(settings, signal, err)
		if strings.Contains(alert.Observed, "pool_retention_") {
			t.Fatal("lifecycle branch acquired prepared suffix")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	settings := syntheticSettings(&syntheticSource{postgresFn: func(string) ([]Row, error) { cancel(); return nil, errors.New("private-pool-source-canary") }})
	alerts, err := NewPoolRetentionSignal().Run(ctx, settings)
	cancel()
	if !errors.Is(err, context.Canceled) || len(alerts) != 0 {
		t.Fatal("late canceled owner published data or lost lifecycle")
	}
	if strings.Contains(visibilityAlert(settings, NewPoolRetentionSignal(), err).Observed, "pool_retention_") {
		t.Fatal("wrapped late context borrowed prepared metadata")
	}
}

// Refuse wrapped, wrong-signal and malformed authority.
func TestPoolRetentionPreparedCarrierOnlyAugmentsDirectPoolIdentity(t *testing.T) {
	settings := syntheticSettings(&syntheticSource{})
	carrier := preparePoolRetentionReadError(errors.New("private-pool-source-canary"))
	for _, wrapped := range []error{errors.Join(carrier, errors.New("safe sibling")), &signalContextError{contextErr: context.Canceled, sourceErr: carrier}} {
		if strings.Contains(visibilityAlert(settings, NewPoolRetentionSignal(), wrapped).Observed, "pool_retention_") {
			t.Fatal("non-direct carrier was searched for metadata")
		}
	}
	wrong := visibilityAlert(settings, NewContractRateSignal(), carrier)
	if strings.Contains(wrong.Observed, "pool_retention_") {
		t.Fatal("wrong signal accepted pool metadata")
	}
	malformed := &poolRetentionPreparedError{legacyText: "private-pool-source-canary", fields: poolRetentionPreparedFields{origin: poolRetentionOrigin(255), sourceClass: "private-pool-source-canary", admission: [3]bool{true}}}
	alert := visibilityAlert(settings, NewPoolRetentionSignal(), malformed)
	if alert.Observed != "error_class=observation-unclassified pool_retention_origin=unknown" {
		t.Fatal("malformed carrier fabricated or exposed metadata")
	}
	requireAlertOmits(t, alert, "private-pool-source-canary")
	var absent *poolRetentionPreparedError
	if got := visibilityAlert(settings, NewPoolRetentionSignal(), absent); got.Observed != "error_class=observation-unclassified" {
		t.Fatal("nil carrier fabricated metadata")
	}
}

// Keep complete measurements free of failure metadata.
func TestPoolRetentionPreparedHealthyAndConcreteResultsUnchanged(t *testing.T) {
	for _, rows := range [][]Row{
		poolRetentionRows("1021", "360", "270", "250", "12", "8", "0", "590"),
		poolRetentionRows("1021", "714", "608", "600", "4", "4", "384", "1700"),
	} {
		_, _, alerts, err := runPoolRetentionDiagnostic(t, rows, nil)
		if err != nil {
			t.Fatal("complete summary acquired error carrier")
		}
		if rows[0][3] == "250" && len(alerts) != 0 {
			t.Fatal("healthy summary changed")
		}
		if rows[0][3] == "600" && (len(alerts) != 1 || alerts[0].Class != "pgbouncer-idle-retention" || alerts[0].Sustain != 10) {
			t.Fatal("concrete retention predicate changed")
		}
		for _, alert := range alerts {
			if strings.Contains(alert.Observed, "pool_retention_") {
				t.Fatal("successful summary acquired failure metadata")
			}
		}
	}
}

// Check actual standing Failed and sustain behavior.
func TestPoolRetentionPreparedPublicExecutionRemainsFailed(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	settings := syntheticSettings(&syntheticSource{postgresFn: func(string) ([]Row, error) { return nil, errors.New("private-pool-source-canary") }})
	calls := 0
	err := NewWithSignals(settings, NewPoolRetentionSignal()).RunExecutionLoop(ctx, RunLoopOptions{}, func(_ context.Context, _ Signal, execution SignalExecution) error {
		calls++
		if !execution.Failed || len(execution.Alerts) != 0 || len(execution.HeldAlerts) != 1 || !strings.Contains(execution.HeldAlerts[0].Observed, "pool_retention_origin=read_error") {
			t.Error("public failed/held execution behavior changed")
		}
		cancel()
		return nil
	})
	if err != nil || calls != 1 {
		t.Fatal("public failure was not delivered once")
	}
}

// Require complete healthy runs before clearing the incident.
func TestPoolRetentionPreparedUnknownCannotClearPriorRetention(t *testing.T) {
	signal := NewPoolRetentionSignal()
	gate := newCadenceAlertGate()
	notifier := &SlackAlertNotifier{settings: SlackAlertNotifierSettings{ClearAfterCompleteRuns: 2}, keyIncidents: map[string]*slackIncident{}, signalKeyCompleteRuns: map[string]int{}}
	raised, cleared := 0, 0
	for run := 0; run < 14; run++ {
		rows := poolRetentionRows("1021", "714", "608", "600", "4", "4", "384", "1700")
		var sourceErr error
		if run == 10 || run == 11 {
			sourceErr = errors.New("private-pool-source-canary")
		}
		if run >= 12 {
			rows = poolRetentionRows("1021", "360", "270", "250", "12", "8", "0", "590")
		}
		settings, _, alerts, err := runPoolRetentionDiagnostic(t, rows, sourceErr)
		if err != nil {
			alerts = append(alerts, visibilityAlert(settings, signal, err))
		}
		ready, held := gate.split(signal, alerts)
		for _, transition := range notifier.observeWithLock(settings.Now().Add(time.Duration(run)*time.Minute), signal.Key(), SignalExecution{Alerts: ready, HeldAlerts: held, Failed: err != nil}) {
			if transition.alert.Class != "pgbouncer-idle-retention" {
				continue
			}
			if transition.kind == slackRaised {
				raised++
			}
			if transition.kind == slackCleared {
				cleared++
				if run != 13 {
					t.Fatal("unknown or first healthy run cleared retention")
				}
			}
		}
	}
	if raised != 1 || cleared != 1 {
		t.Fatal("retention history/healthy clearing changed")
	}
}

// Check the public joined error and alert serialization.
func TestPoolRetentionPreparedMonitorJoinedErrorStaysPrivate(t *testing.T) {
	original := &poolRetentionProviderCanary{}
	settings := syntheticSettings(&syntheticSource{postgresFn: func(string) ([]Row, error) { return nil, original }})
	alerts, err := NewWithSignals(settings, NewPoolRetentionSignal()).Run(context.Background())
	if err == nil || len(alerts) != 1 || alerts[0].Class != "cannot-observe" || !strings.Contains(alerts[0].Observed, "pool_retention_origin=read_error") {
		t.Fatal("public Monitor error/visibility path changed")
	}
	if errors.Is(err, original) || strings.Contains(err.Error(), "private-pool-source-canary") {
		t.Fatal("joined Monitor error retained original source")
	}
	encoded, marshalErr := json.Marshal(alerts)
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	if strings.Contains(string(encoded), "private-pool-source-canary") {
		t.Fatal("public Monitor alert leaked original source")
	}
}
