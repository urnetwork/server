// Synthetic controls for the §22.4 reader over the deterministic world.
package monitor

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"

	"github.com/urfoundation/sn/v2026/protocol"

	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/model"
)

// The classes whose sources are not deployed stay explicitly unknown on a
// healthy deployment; nothing else alerts.
func requireOnlySubnetValidationStandingUnknowns(t *testing.T, alerts Alerts) {
	t.Helper()
	standing := map[string]string{
		subnetVerifyPoisoningClass:     "authorized-canary-not-deployed",
		subnetPoolQualityMismatchClass: "validator-decision-unpublished",
		subnetAttemptHistoryGapClass:   "validator-cuts-unpublished",
		subnetEgressAttributionClass:   "source-attribution-private",
	}
	for _, alert := range alerts {
		reason, ok := standing[alert.Class]
		if !ok || alert.Frame != "unobserved" || alert.Severity != SeverityWarn || !alert.unobservable || !strings.Contains(alert.Observed, reason) || alert.Sustain != subnetConfigurationSustain {
			t.Errorf("unexpected alert %s %s %s %s: %s", alert.Severity, alert.Class, alert.Target, alert.Frame, alert.Observed)
		}
		delete(standing, alert.Class)
	}
	for class := range standing {
		t.Errorf("standing unknown %s is missing", class)
	}
}

func TestSubnetValidationHealthyDeploymentVerifiesEveryProof(t *testing.T) {
	world := newSubnetWorld(t)
	alerts := runSubnetWorld(t, NewSubnetValidationSignal(), world.signalSettings())
	requireOnlySubnetValidationStandingUnknowns(t, alerts)
	world.requirePrivate(t, alerts)
	if len(world.requests) == 0 {
		t.Fatal("no verify route was read")
	}
	// Chain endpoints serve only the agreed checkpoint that dates the
	// published evidence; no contract, log or storage census is read.
	for call := range world.calls {
		switch method := call[strings.LastIndex(call, " ")+1:]; method {
		case "eth_call", "eth_getLogs", "eth_getStorageAt", "eth_getCode", "eth_getTransactionByHash", "state_getKeysPaged":
			t.Fatalf("the validation reader read %s from a chain endpoint", method)
		}
	}
}

// The published evidence is decided without the chain; only its frame then
// says the checkpoint is unavailable.
func TestSubnetValidationDecidesWithoutTheCheckpoint(t *testing.T) {
	world := newSubnetWorld(t)
	world.proofs.Rows[0].FinalSig[0] ^= 1
	world.override = func(endpoint, method string, params []json.RawMessage) (any, error, bool) {
		if method == "chain_getFinalizedHead" {
			return nil, errSubnetWorldSourceLoss, true
		}
		return nil, nil, false
	}
	alerts := runSubnetWorld(t, NewSubnetValidationSignal(), world.signalSettings())
	alert := requireSubnetAlert(t, alerts, subnetPathProofInvalidClass, "subnet-validation/proofs", "", SeverityPage)
	if !strings.Contains(alert.Context, "checkpoint=unavailable") || !strings.Contains(alert.Observed, "final-signature") {
		t.Fatalf("an unavailable checkpoint hid or misdated the proof failure: %s | %s", alert.Context, alert.Observed)
	}
}

func TestSubnetValidationServerKeyHistory(t *testing.T) {
	other := ed25519.NewKeyFromSeed(subnetWorldHash("other-server-key").Bytes())
	world := newSubnetWorld(t)
	keys := world.routes["/verify/keys"]
	world.routes["/verify/keys"] = func(request *http.Request) *http.Response {
		if request.URL.Host == "api-two.private.example.test" {
			encoded, _ := json.Marshal(controller.GetVerifyKeysResult{Keys: []*controller.VerifyKey{{ServerKeyId: 1, PublicKey: other.Public().(ed25519.PublicKey)}}})
			return subnetWorldJsonResponse(request, encoded)
		}
		return keys(request)
	}
	alerts := runSubnetWorld(t, NewSubnetValidationSignal(), world.signalSettings())
	alert := requireSubnetAlert(t, alerts, subnetPathProofInvalidClass, "subnet-validation/keys", "", SeverityPage)
	if !strings.Contains(alert.Observed, "origins-differ") {
		t.Fatal(alert.Observed)
	}
	for _, test := range []struct {
		name   string
		keys   []*controller.VerifyKey
		reason string
	}{
		{"rewritten", []*controller.VerifyKey{{ServerKeyId: 1, PublicKey: other.Public().(ed25519.PublicKey)}}, "server-key-history-rewritten"},
		{"dropped", []*controller.VerifyKey{{ServerKeyId: 2, PublicKey: other.Public().(ed25519.PublicKey)}}, "server-key-history-dropped"},
	} {
		t.Run(test.name, func(t *testing.T) {
			world := newSubnetWorld(t)
			signal := NewSubnetValidationSignal()
			settings := world.signalSettings()
			requireOnlySubnetValidationStandingUnknowns(t, runSubnetWorld(t, signal, settings))
			world.verifyKeys = test.keys
			alerts := runSubnetWorld(t, signal, settings)
			alert := requireSubnetAlert(t, alerts, subnetPathProofInvalidClass, "subnet-validation/keys", "", SeverityPage)
			if !strings.Contains(alert.Observed, test.reason) {
				t.Fatal(alert.Observed)
			}
		})
	}
	// A rotation that keeps every historical key is healthy.
	world = newSubnetWorld(t)
	signal := NewSubnetValidationSignal()
	settings := world.signalSettings()
	requireOnlySubnetValidationStandingUnknowns(t, runSubnetWorld(t, signal, settings))
	world.verifyKeys = append([]*controller.VerifyKey{{ServerKeyId: 2, PublicKey: other.Public().(ed25519.PublicKey)}}, world.verifyKeys...)
	requireOnlySubnetValidationStandingUnknowns(t, runSubnetWorld(t, signal, settings))
}

func TestSubnetValidationInvalidPublishedEvidencePages(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*subnetWorld)
		reason string
	}{
		{"final-signature", func(world *subnetWorld) { world.proofs.Rows[0].FinalSig[0] ^= 1 }, "final-signature"},
		{"verifier-signature", func(world *subnetWorld) { world.proofs.Rows[0].VerifierSig[0] ^= 1 }, "verifier-signature"},
		{"depth", func(world *subnetWorld) {
			world.proofs.Rows[0] = subnetWorldProofRow(subnetWorldValidatorKey(1), 1000, 6, true, world.proofs.Rows[0].CreateTime)
		}, "proof-depth-differs-from-policy"},
		{"key-unpublished", func(world *subnetWorld) { world.proofs.Rows[0].ServerKeyId = 9 }, "proof-key-unpublished"},
		{"fork", func(world *subnetWorld) {
			fork := *world.proofs.Rows[1]
			fork.TrailId = world.proofs.Rows[0].TrailId
			world.proofs.Rows = append(world.proofs.Rows, &fork)
		}, "proof-fork"},
		{"hop-order", func(world *subnetWorld) {
			var hops []connect.VerifyProofHop
			_ = json.Unmarshal(world.proofs.Rows[0].Hops, &hops)
			hops[1].ClientId = hops[0].ClientId
			world.proofs.Rows[0].Hops, _ = json.Marshal(hops)
		}, "proof-hop-order"},
		{"transition-signature", func(world *subnetWorld) { world.proofs.OriginalTransitions[0].Signature[0] ^= 1 }, "transition-invalid"},
		{"transition-scope", func(world *subnetWorld) {
			original := world.deployment.DeploymentId
			world.deployment.DeploymentId = "another-deployment"
			world.proofs.OriginalTransitions[0] = world.transition(subnetWorldValidatorKey(1), 1)
			world.deployment.DeploymentId = original
		}, "transition-scope"},
	} {
		t.Run(test.name, func(t *testing.T) {
			world := newSubnetWorld(t)
			test.mutate(world)
			alerts := runSubnetWorld(t, NewSubnetValidationSignal(), world.signalSettings())
			alert := requireSubnetAlert(t, alerts, subnetPathProofInvalidClass, "subnet-validation/proofs", "", SeverityPage)
			if !strings.Contains(alert.Observed, test.reason) || alert.Sustain != subnetProvedSustain {
				t.Fatal(alert.Observed)
			}
			world.requirePrivate(t, alerts)
		})
	}
}

func TestSubnetValidationAttemptHistory(t *testing.T) {
	world := newSubnetWorld(t)
	fork := world.transition(subnetWorldValidatorKey(1), 1)
	var body protocol.ProviderAttemptReceiptBody
	_ = json.Unmarshal(fork.Body, &body)
	body.RecoveryMs += 5
	encoded, _ := json.Marshal(body)
	fork = &model.VerifyOriginalTransition{Body: encoded, Signature: ed25519.Sign(subnetWorldServerKey(), append([]byte(protocol.ProviderAttemptReceiptDomain), encoded...))}
	world.proofs.OriginalTransitions = append(world.proofs.OriginalTransitions, fork)
	alerts := runSubnetWorld(t, NewSubnetValidationSignal(), world.signalSettings())
	alert := requireSubnetAlert(t, alerts, subnetAttemptHistoryGapClass, "subnet-validation/attempts", "", SeverityPage)
	if !strings.Contains(alert.Observed, "transition-fork") {
		t.Fatal(alert.Observed)
	}
	world = newSubnetWorld(t)
	world.proofs.OriginalTruncated = true
	alerts = runSubnetWorld(t, NewSubnetValidationSignal(), world.signalSettings())
	alert = requireSubnetAlert(t, alerts, subnetAttemptHistoryGapClass, "subnet-validation/attempts", "unobserved", SeverityWarn)
	if !strings.Contains(alert.Observed, "transition-census-truncated") {
		t.Fatal(alert.Observed)
	}
}

func TestSubnetValidationEgressEvidence(t *testing.T) {
	world := newSubnetWorld(t)
	var hops []map[string]any
	_ = json.Unmarshal(world.proofs.Rows[0].Hops, &hops)
	hops[0]["source"] = "192.0.2.17"
	world.proofs.Rows[0].Hops, _ = json.Marshal(hops)
	alerts := runSubnetWorld(t, NewSubnetValidationSignal(), world.signalSettings())
	alert := requireSubnetAlert(t, alerts, subnetEgressAttributionClass, "subnet-validation/egress", "", SeverityPage)
	if !strings.Contains(alert.Observed, "raw-address-in-public-evidence") || !strings.Contains(alert.Observed, "unexpected-public-hop-field") {
		t.Fatal(alert.Observed)
	}
	requireAlertOmits(t, alert, "192.0.2.17")
	world = newSubnetWorld(t)
	var proofHops []connect.VerifyProofHop
	_ = json.Unmarshal(world.proofs.Rows[0].Hops, &proofHops)
	proofHops[2].EgressIpHash = [32]byte{}
	world.proofs.Rows[0].Hops, _ = json.Marshal(proofHops)
	alerts = runSubnetWorld(t, NewSubnetValidationSignal(), world.signalSettings())
	alert = requireSubnetAlert(t, alerts, subnetEgressAttributionClass, "subnet-validation/egress", "", SeverityPage)
	if !strings.Contains(alert.Observed, "egress-hash-missing") {
		t.Fatal(alert.Observed)
	}
}

func TestSubnetValidationCoverage(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*subnetWorld)
		reason string
	}{
		{"stale", func(world *subnetWorld) {
			for _, row := range world.stats.Rows {
				row.PeriodStart, row.PeriodEnd = world.now.Add(-4*time.Hour), world.now.Add(-3*time.Hour)
			}
		}, "stats-stale"},
		{"wilson", func(world *subnetWorld) { world.stats.Rows[0].ReliabilityPPM += 1 }, "stats-reliability-differs"},
		{"egress-key", func(world *subnetWorld) { world.stats.EgressHashKeyId = "another-key" }, "egress-key-differs"},
		{"a-min", func(world *subnetWorld) {
			for _, row := range world.stats.Rows[1:] {
				row.Assignments, row.Confirmations = 3, 3
				row.ReliabilityPPM = protocol.ReliabilityPPM(3, 3, world.policy.Verify.ReliabilityAMin)
			}
		}, "eligible-coverage-low"},
		{"validator-absent", func(world *subnetWorld) {
			kept := world.proofs.Rows[:0]
			for _, row := range world.proofs.Rows {
				if string(row.Vpk) != string(subnetWorldValidatorKey(2).Public().(ed25519.PublicKey)) {
					kept = append(kept, row)
				}
			}
			world.proofs.Rows = kept
		}, "validator-2-absent"},
	} {
		t.Run(test.name, func(t *testing.T) {
			world := newSubnetWorld(t)
			test.mutate(world)
			alerts := runSubnetWorld(t, NewSubnetValidationSignal(), world.signalSettings())
			alert := requireSubnetAlert(t, alerts, subnetMeasurementCoverageClass, "subnet-validation/coverage", "", SeverityWarn)
			if !strings.Contains(alert.Observed, test.reason) || alert.Sustain != subnetTransientSustain {
				t.Fatal(alert.Observed)
			}
		})
	}
}

func TestSubnetValidationCompletionBias(t *testing.T) {
	world := newSubnetWorld(t)
	world.settings.Limits.BiasMinTrails = 10
	world.settings.Limits.BiasZScoreTenths = 20
	world.proofs.Rows = world.proofs.Rows[:0]
	for trail := 0; trail < 30; trail++ {
		world.proofs.Rows = append(world.proofs.Rows,
			subnetWorldProofRow(subnetWorldValidatorKey(1), uint64(1000+trail), 8, true, world.now.Add(-time.Duration(40-trail)*time.Minute)),
			subnetWorldProofRow(subnetWorldValidatorKey(2), uint64(2000+trail), 8, trail%2 == 0, world.now.Add(-time.Duration(40-trail)*time.Minute)))
	}
	alerts := runSubnetWorld(t, NewSubnetValidationSignal(), world.signalSettings())
	alert := requireSubnetAlert(t, alerts, subnetMeasurementBiasClass, "subnet-validation/bias", "", SeverityWarn)
	if !strings.Contains(alert.Observed, "outliers=2") {
		t.Fatal(alert.Observed)
	}
	if !strings.Contains(alert.Evidence, "Correlation does not prove collusion") {
		t.Fatal("bias alert lost its attribution limit")
	}
	// Equal completion is the healthy control.
	world.proofs.Rows = world.proofs.Rows[:0]
	for trail := 0; trail < 30; trail++ {
		world.proofs.Rows = append(world.proofs.Rows,
			subnetWorldProofRow(subnetWorldValidatorKey(1), uint64(1000+trail), 8, trail%3 != 0, world.now.Add(-time.Duration(40-trail)*time.Minute)),
			subnetWorldProofRow(subnetWorldValidatorKey(2), uint64(2000+trail), 8, trail%3 != 1, world.now.Add(-time.Duration(40-trail)*time.Minute)))
	}
	requireOnlySubnetValidationStandingUnknowns(t, runSubnetWorld(t, NewSubnetValidationSignal(), world.signalSettings()))
}

func TestSubnetValidationSourceLossIsUnknown(t *testing.T) {
	world := newSubnetWorld(t)
	world.routes["/verify/proofs"] = func(request *http.Request) *http.Response {
		return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: http.Header{}, Body: http.NoBody, Request: request}
	}
	alerts := runSubnetWorld(t, NewSubnetValidationSignal(), world.signalSettings())
	requireSubnetAlert(t, alerts, subnetPathProofInvalidClass, "subnet-validation/proofs", "unobserved", SeverityWarn)
	for _, alert := range alerts {
		if alert.Severity == SeverityPage {
			t.Fatalf("source loss produced a proved violation: %+v", alert)
		}
	}
	requireSubnetAlert(t, alerts, subnetMeasurementBiasClass, "subnet-validation/bias", "unobserved", SeverityWarn)
}

func TestSubnetValidationPolicyGates(t *testing.T) {
	world := newSubnetWorld(t)
	world.settings.PolicyDocument = ""
	alerts := runSubnetWorld(t, NewSubnetValidationSignal(), world.signalSettings())
	requireSubnetAlert(t, alerts, subnetMeasurementCoverageClass, "subnet-validation/coverage", "unobserved", SeverityWarn)
	if len(subnetAlerts(alerts, subnetPathProofInvalidClass, "")) != 0 {
		t.Fatal("proof signatures were not verified without the policy document")
	}
	world = newSubnetWorld(t)
	world.proofs.PolicyHash = "0x" + strings.Repeat("ab", 32)
	world.routes["/verify/proofs"] = func(request *http.Request) *http.Response {
		encoded, _ := json.Marshal(world.proofs)
		return subnetWorldJsonResponse(request, encoded)
	}
	alerts = runSubnetWorld(t, NewSubnetValidationSignal(), world.signalSettings())
	alert := requireSubnetAlert(t, alerts, subnetPathProofInvalidClass, "subnet-validation/proofs", "publication", SeverityWarn)
	if !strings.Contains(alert.Observed, "verify-policy-differs") {
		t.Fatal(alert.Observed)
	}
}

func TestSubnetValidationRegistrationMetadata(t *testing.T) {
	signal := NewSubnetValidationSignal()
	if signal.Number() != "22.4" || signal.Key() != "subnet-validation" || signal.ID() != "subnet/validation" || signal.Cadence() <= 0 {
		t.Fatalf("unexpected metadata %s %s %s", signal.Number(), signal.Key(), signal.ID())
	}
}

// §22.10 acceptance: a published Final with a broken server signature is a
// proved finalized-proof counterexample; restoring it is the healthy read.
// The validators' private measurement, cuts, attribution and canary remain
// standing gaps of a healthy run.
func init() {
	subnetFamilyAcceptanceCases["22.4"] = subnetFamilyAcceptanceCase{
		healthy: func(t *testing.T) (Signal, SignalSettings) {
			return NewSubnetValidationSignal(), newSubnetWorld(t).signalSettings()
		},
		violated: func(t *testing.T) (Signal, SignalSettings, func()) {
			world := newSubnetWorld(t)
			world.proofs.Rows[0].FinalSig[0] ^= 1
			return NewSubnetValidationSignal(), world.signalSettings(), func() {
				world.proofs.Rows[0].FinalSig[0] ^= 1
			}
		},
		violatedClass: subnetPathProofInvalidClass,
		unknown: func(t *testing.T) (Signal, SignalSettings) {
			world := newSubnetWorld(t)
			world.routes["/verify/proofs"] = func(request *http.Request) *http.Response {
				return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: http.Header{}, Body: http.NoBody, Request: request}
			}
			return NewSubnetValidationSignal(), world.signalSettings()
		},
		canceled: func(t *testing.T) (Signal, SignalSettings, context.Context) {
			world := newSubnetWorld(t)
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			proofs := world.routes["/verify/proofs"]
			world.routes["/verify/proofs"] = func(request *http.Request) *http.Response {
				cancel()
				return proofs(request)
			}
			return NewSubnetValidationSignal(), world.signalSettings(), ctx
		},
		fixedGaps: []subnetFamilyAcceptanceGap{
			{class: subnetVerifyPoisoningClass, frame: "unobserved"},
			{class: subnetPoolQualityMismatchClass, frame: "unobserved"},
			{class: subnetAttemptHistoryGapClass, frame: "unobserved"},
			{class: subnetEgressAttributionClass, frame: "unobserved"},
		},
	}
}
