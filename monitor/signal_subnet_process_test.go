package monitor

import (
	"context"
	"errors"
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

// A synthetic producer database answering the three bounded views.
type subnetProcessTestDatabase struct {
	mirror    []Row
	intents   []Row
	tasks     []Row
	failure   error
	queries   []string
	malformed bool
}

func (self *subnetProcessTestDatabase) query(query string) ([]Row, error) {
	self.queries = append(self.queries, query)
	if self.failure != nil {
		return nil, self.failure
	}
	switch {
	case strings.Contains(query, "FROM st_chain_sync"):
		return self.mirror, nil
	case strings.Contains(query, "FROM st_transaction_intent"):
		return self.intents, nil
	case strings.Contains(query, "FROM pending_task"):
		return self.tasks, nil
	}
	return nil, errors.New("unexpected producer query")
}

func subnetProcessTestDeploymentKey() string {
	return "964:" + strings.ToLower(subnetReplayTestCoordinator.Hex())
}

// A healthy producer: a current canonical mirror boundary, a scheduled sync
// task, no unresolved intents, and every scoped writer funded.
func newSubnetProcessFixture(t *testing.T) (*subnetSettlementTestChain, *subnetReplayTestNode, *subnetProcessTestDatabase, SignalSettings) {
	t.Helper()
	chain := newHealthySubnetSettlementChain(t)
	node := newSubnetReplayTestNode(t, 964, subnetReplayTestGenesis, chain.finalizedBlock())
	chain.install(node)
	for _, noId := range chain.operators {
		node.balances[chain.versions[noId].RootSigner] = big.NewInt(1_000_000_000)
		node.balances[chain.versions[noId].DepositSigner] = big.NewInt(1_000_000_000)
	}
	settings, source := subnetReplayTestSettings(t, node)
	database := &subnetProcessTestDatabase{
		mirror: []Row{{subnetProcessTestDeploymentKey(), "1441", subnetReplayTestBlockHash(1440, 0).Hex(), "30"}},
		tasks:  []Row{{"github.com/urnetwork/server/taskworker/work.StSyncChain", "-3", "0", "f"}},
	}
	source.postgresFn = database.query
	settings.SubnetReplay.WriterActionLiabilityWei = "1000000"
	settings.SubnetReplay.WriterMinimumActions = 4
	return chain, node, database, settings
}

func runSubnetProcess(t *testing.T, signal Signal, settings SignalSettings) Alerts {
	t.Helper()
	alerts, err := signal.Run(context.Background(), settings)
	if err != nil {
		t.Fatalf("process run failed: %v", err)
	}
	return alerts
}

// Only the two fixed exporter gaps remain when the producer is healthy.
func TestSubnetProcessHealthyProducerHasOnlyExporterGaps(t *testing.T) {
	_, _, database, settings := newSubnetProcessFixture(t)
	alerts := runSubnetProcess(t, NewSubnetProcessSignal(), settings)
	for _, alert := range alerts {
		if alert.Class != "subnet-monitor-coverage" || (alert.Frame != "process-telemetry" && alert.Frame != "role-liveness") {
			t.Fatalf("healthy producer alerted: %s %s %s %s", alert.Class, alert.Target, alert.Frame, alert.Observed)
		}
	}
	telemetry := requireSubnetReplayAlert(t, alerts, "subnet-monitor-coverage", "subnet-process/coverage", "process-telemetry")
	for _, class := range subnetProcessTelemetryClasses {
		if !strings.Contains(telemetry.Observed, class) {
			t.Fatalf("telemetry gap omits %s: %q", class, telemetry.Observed)
		}
	}
	for _, query := range database.queries {
		if strings.Contains(query, strings.ToLower(subnetReplayTestCoordinator.Hex())) || strings.Contains(query, "synthetic-replay-deployment") {
			t.Fatal("a producer query interpolated a deployment identity")
		}
	}
	requireSubnetReplayRedacted(t, alerts, subnetProcessTestDeploymentKey())
}

// A lagging or stalled mirror warns despite process health.
func TestSubnetProcessMirrorLagAndStallWarn(t *testing.T) {
	_, _, database, settings := newSubnetProcessFixture(t)
	database.mirror = []Row{{subnetProcessTestDeploymentKey(), "1001", subnetReplayTestBlockHash(1000, 0).Hex(), "3600"}}
	alerts := runSubnetProcess(t, NewSubnetProcessSignal(), settings)
	lag := requireSubnetReplayAlert(t, alerts, "subnet-runtime-liveness", "subnet-process/operator", "event-mirror")
	stale := requireSubnetReplayAlert(t, alerts, "subnet-runtime-liveness", "subnet-process/operator", "event-mirror-age")
	if lag.Severity != SeverityWarn || !strings.Contains(lag.Observed, "lag=440") || stale.Severity != SeverityWarn {
		t.Fatalf("mirror lag=%+v stale=%+v", lag, stale)
	}
}

// A mirror boundary that is not the canonical finalized hash pages.
func TestSubnetProcessNonCanonicalMirrorBoundaryPages(t *testing.T) {
	_, _, database, settings := newSubnetProcessFixture(t)
	database.mirror = []Row{{subnetProcessTestDeploymentKey(), "1441", subnetReplayTestBlockHash(1440, 3).Hex(), "30"}}
	alerts := runSubnetProcess(t, NewSubnetProcessSignal(), settings)
	alert := requireSubnetReplayAlert(t, alerts, "subnet-state-durability", "subnet-process/operator", "event-mirror-boundary")
	if alert.Severity != SeverityPage {
		t.Fatalf("non-canonical boundary severity=%s", alert.Severity)
	}
}

// A mirror ahead of the monitor's checkpoint is not compared.
func TestSubnetProcessMirrorAheadOfCheckpointIsNotCompared(t *testing.T) {
	_, _, database, settings := newSubnetProcessFixture(t)
	database.mirror = []Row{{subnetProcessTestDeploymentKey(), "1450", subnetReplayTestBlockHash(1449, 3).Hex(), "30"}}
	alerts := runSubnetProcess(t, NewSubnetProcessSignal(), settings)
	if len(subnetReplayAlerts(alerts, "subnet-state-durability")) != 0 {
		t.Fatal("an ahead mirror was compared against an unfinalized height")
	}
}

// Missing mirror and sync task, an overdue task and failing tasks warn.
func TestSubnetProcessTaskAndMirrorLiveness(t *testing.T) {
	_, _, database, settings := newSubnetProcessFixture(t)
	database.mirror = nil
	database.tasks = []Row{{"github.com/urnetwork/server/taskworker/work.StCommitRoot", "10", "5", "t"}}
	alerts := runSubnetProcess(t, NewSubnetProcessSignal(), settings)
	requireSubnetReplayAlert(t, alerts, "subnet-runtime-liveness", "subnet-process/operator", "event-mirror")
	requireSubnetReplayAlert(t, alerts, "subnet-runtime-liveness", "subnet-process/operator", "sync-task")
	failing := requireSubnetReplayAlert(t, alerts, "subnet-runtime-liveness", "subnet-process/operator", "task-errors")
	if !strings.Contains(failing.Observed, "failing=StCommitRoot") {
		t.Fatalf("failing tasks=%q", failing.Observed)
	}

	_, _, database, settings = newSubnetProcessFixture(t)
	database.tasks = []Row{{"github.com/urnetwork/server/taskworker/work.StSyncChain", "4000", "0", "f"}}
	alerts = runSubnetProcess(t, NewSubnetProcessSignal(), settings)
	overdue := requireSubnetReplayAlert(t, alerts, "subnet-runtime-liveness", "subnet-process/operator", "sync-task")
	if !strings.Contains(overdue.Observed, "overdue=4000s") {
		t.Fatalf("overdue sync=%q", overdue.Observed)
	}
}

// Uncertain, stale and nonce-consumed intents are unresolved writes.
func TestSubnetProcessUnresolvedIntentsWarn(t *testing.T) {
	_, node, database, settings := newSubnetProcessFixture(t)
	sender := common.HexToAddress("0x00000000000000000000000000000000000e0001")
	node.balances[sender] = big.NewInt(1_000_000_000)
	node.nonces[sender] = 10
	database.intents = []Row{
		{"synthetic-replay-deployment", "964", strings.ToLower(sender.Hex()), "11", "uncertain", "1", "60"},
		{"synthetic-replay-deployment", "964", strings.ToLower(sender.Hex()), "7", "broadcast", "2", "7200"},
		{"other-deployment", "964", strings.ToLower(sender.Hex()), "1", "broadcast", "1", "7200"},
	}
	alerts := runSubnetProcess(t, NewSubnetProcessSignal(), settings)
	for _, frame := range []string{"uncertain-intents", "stale-intents", "consumed-nonces"} {
		alert := requireSubnetReplayAlert(t, alerts, "subnet-writer-funding", "subnet-process/producer", frame)
		if !strings.Contains(alert.Observed, "observed=1") {
			t.Fatalf("%s counted another deployment's intent: %q", frame, alert.Observed)
		}
	}
	requireSubnetReplayRedacted(t, alerts, strings.ToLower(sender.Hex()))
}

// An unfunded signer warns without enrollment; a short runway needs it.
func TestSubnetProcessWriterFunding(t *testing.T) {
	chain, node, _, settings := newSubnetProcessFixture(t)
	node.balances[chain.versions[1].RootSigner] = new(big.Int)
	node.balances[chain.versions[2].DepositSigner] = big.NewInt(2_000_000)
	alerts := runSubnetProcess(t, NewSubnetProcessSignal(), settings)
	empty := requireSubnetReplayAlert(t, alerts, "subnet-writer-funding", "subnet-process/no-1", "root-signer")
	if !strings.Contains(empty.Observed, "observed=balance=0") {
		t.Fatalf("empty writer=%q", empty.Observed)
	}
	runway := requireSubnetReplayAlert(t, alerts, "subnet-writer-funding", "subnet-process/no-2", "deposit-signer")
	if !strings.Contains(runway.Observed, "observed=2-fundable-actions") {
		t.Fatalf("runway=%q", runway.Observed)
	}

	chain, node, _, settings = newSubnetProcessFixture(t)
	settings.SubnetReplay.WriterActionLiabilityWei = ""
	node.balances[chain.versions[1].RootSigner] = new(big.Int)
	node.balances[chain.versions[2].DepositSigner] = big.NewInt(2_000_000)
	alerts = runSubnetProcess(t, NewSubnetProcessSignal(), settings)
	requireSubnetReplayAlert(t, alerts, "subnet-monitor-coverage", "subnet-process/coverage", "writer-runway")
	funding := subnetReplayAlerts(alerts, "subnet-writer-funding")
	if len(funding) != 1 || funding[0].Target != "subnet-process/no-1" || funding[0].Frame != "root-signer" {
		t.Fatalf("without enrollment only the empty writer may alert: %+v", funding)
	}
}

// An unavailable producer database is unknown; the chain checks still run.
func TestSubnetProcessProducerDatabaseUnavailableIsUnknown(t *testing.T) {
	chain, node, database, settings := newSubnetProcessFixture(t)
	database.failure = errors.New("synthetic database unavailable")
	node.balances[chain.versions[1].RootSigner] = new(big.Int)
	alerts := runSubnetProcess(t, NewSubnetProcessSignal(), settings)
	gap := requireSubnetReplayAlert(t, alerts, "subnet-monitor-coverage", "subnet-process/coverage", "producer")
	if !strings.Contains(gap.Observed, "reason=producer-database-unavailable") {
		t.Fatalf("producer gap=%q", gap.Observed)
	}
	requireSubnetReplayAlert(t, alerts, "subnet-writer-funding", "subnet-process/no-1", "root-signer")
	requireAlertOmits(t, gap, "synthetic database unavailable")
}

// Malformed producer rows are unknown, never a healthy empty view.
func TestSubnetProcessMalformedProducerRowsAreUnknown(t *testing.T) {
	_, _, database, settings := newSubnetProcessFixture(t)
	database.mirror = []Row{{subnetProcessTestDeploymentKey(), "not-a-number", "", "30"}}
	alerts := runSubnetProcess(t, NewSubnetProcessSignal(), settings)
	gap := requireSubnetReplayAlert(t, alerts, "subnet-monitor-coverage", "subnet-process/coverage", "producer")
	if !strings.Contains(gap.Observed, "reason=producer-row-malformed") {
		t.Fatalf("malformed rows=%q", gap.Observed)
	}
}

// An explicitly disabled producer is not judged; its operators' chain
// signers still are.
func TestSubnetProcessDisabledProducerSkipsProducerRules(t *testing.T) {
	chain, node, database, settings := newSubnetProcessFixture(t)
	settings.STConfigStatus = STConfigurationExplicitDisabled
	settings.VerificationEnabled = false
	settings.STDeploymentKey = ""
	database.mirror = nil
	node.balances[chain.versions[1].RootSigner] = new(big.Int)
	alerts := runSubnetProcess(t, NewSubnetProcessSignal(), settings)
	if len(subnetReplayAlerts(alerts, "subnet-runtime-liveness")) != 0 || 0 < len(database.queries) {
		t.Fatal("a disabled producer was judged")
	}
	requireSubnetReplayAlert(t, alerts, "subnet-writer-funding", "subnet-process/no-1", "root-signer")
}

// Parent cancellation withholds every process finding.
func TestSubnetProcessCanceledRunWithholdsFindings(t *testing.T) {
	_, node, database, settings := newSubnetProcessFixture(t)
	database.mirror = nil
	ctx, cancel := context.WithCancel(context.Background())
	node.beforeExchange = func(exchange int) {
		if exchange == 3 {
			cancel()
		}
	}
	alerts, err := NewSubnetProcessSignal().Run(ctx, settings)
	if err == nil || len(alerts) != 0 {
		t.Fatalf("canceled process run returned alerts=%d err=%v", len(alerts), err)
	}
}

func TestSubnetProcessRegistrationMetadata(t *testing.T) {
	signal := NewSubnetProcessSignal()
	if signal.Number() != "22.9" || signal.Key() != "subnet-process" || signal.ID() != "subnet/process" || signal.Cadence() <= 0 {
		t.Fatalf("unexpected metadata: %s %s %s", signal.Number(), signal.Key(), signal.ID())
	}
}

// §22.10 acceptance fixtures for this family.
func init() {
	subnetFamilyAcceptanceCases["22.9"] = subnetFamilyAcceptanceCase{
		healthy: func(t *testing.T) (Signal, SignalSettings) {
			_, _, _, settings := newSubnetProcessFixture(t)
			return NewSubnetProcessSignal(), settings
		},
		violated: func(t *testing.T) (Signal, SignalSettings, func()) {
			_, _, database, settings := newSubnetProcessFixture(t)
			healthy := database.mirror
			database.mirror = []Row{{subnetProcessTestDeploymentKey(), "1441", subnetReplayTestBlockHash(1440, 3).Hex(), "30"}}
			return NewSubnetProcessSignal(), settings, func() { database.mirror = healthy }
		},
		violatedClass: "subnet-state-durability",
		unknown: func(t *testing.T) (Signal, SignalSettings) {
			_, _, database, settings := newSubnetProcessFixture(t)
			database.failure = errors.New("synthetic database unavailable")
			return NewSubnetProcessSignal(), settings
		},
		canceled: func(t *testing.T) (Signal, SignalSettings, context.Context) {
			_, node, database, settings := newSubnetProcessFixture(t)
			database.mirror = nil
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			node.beforeExchange = func(exchange int) {
				if exchange == 3 {
					cancel()
				}
			}
			return NewSubnetProcessSignal(), settings, ctx
		},
		fixedGaps: []string{"process-telemetry", "role-liveness"},
	}
}
