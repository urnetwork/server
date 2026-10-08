package perfvar

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	clientconnect "github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"
)

// A single scenario feature resolves both endpoints. Missing settings fail
// closed when a binary is compiled against an older Connect revision.
func perfvarUdpTransferPolicy(features []string) (noAck, explicit bool, err error) {
	ack := slices.Contains(features, perfvarFeatureUdpTransferAck)
	noAck = slices.Contains(features, perfvarFeatureUdpTransferNoAck)
	if ack && noAck {
		return false, false, fmt.Errorf("conflicting UDP policies: %s and %s", perfvarFeatureUdpTransferAck, perfvarFeatureUdpTransferNoAck)
	}
	return noAck, ack || noAck, nil
}

// The measured arms share this clean application-level gate, using real
// provider UDP sockets and each production P2P carrier in both directions.
func TestFullTunMatchedUdpTransferPolicies(t *testing.T) {
	if testing.Short() {
		return
	}
	testEnvironment := &server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}
	testEnvironment.Run(t, func(t testing.TB) {
		for _, feature := range []string{perfvarFeatureUdpTransferAck, perfvarFeatureUdpTransferNoAck} {
			for _, route := range []fullTunRoute{fullTunRouteP2pFast, fullTunRouteP2pLegacy} {
				func() {
					ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
					defer cancel()
					profile := initialNetworkProfiles(20260810)["clean-lan"]
					environment := newRouteEnvironmentWithNetworkPeers(ctx, t, profile, false)
					defer environment.close()
					resources := defaultTunResourceProfile()
					resources.Features = []string{feature}
					path := newFullTunPathWithResources(ctx, t, environment, route, false, resources)
					defer path.close()
					for _, upload := range []bool{true, false} {
						result, err := measureFullTunUDPDirection(ctx, path, upload, 50*time.Millisecond, 2_000_000, 800)
						if err != nil || result.OfferedPacketCount == 0 || result.DeliveredPacketCount != result.OfferedPacketCount || result.DuplicatePacketCount != 0 || result.CorruptPacketCount != 0 {
							t.Fatalf("%s/%s/upload=%t UDP correctness: offered=%d delivered=%d duplicate=%d corrupt=%d error=%v", feature, route, upload, result.OfferedPacketCount, result.DeliveredPacketCount, result.DuplicatePacketCount, result.CorruptPacketCount, err)
						}
					}
				}()
			}
		}
	})
}

func TestPerfvarUdpTransferPolicyPairsFullMatrixTraces(t *testing.T) {
	var arms [2][]perfvarScenario
	for index, feature := range []string{perfvarFeatureUdpTransferAck, perfvarFeatureUdpTransferNoAck} {
		values := map[string]string{
			"CONNECT_PERFVAR_FEATURE":   feature,
			"CONNECT_PERFVAR_ROUTE":     "p2p-fast,p2p-legacy",
			"CONNECT_PERFVAR_PROFILE":   "clean-lan," + cellEdge5mDown1mUpName + "," + cellEdge1mDown250kUpName,
			"CONNECT_PERFVAR_WORKLOAD":  "udp",
			"CONNECT_PERFVAR_DIRECTION": "upload,download",
			"CONNECT_PERFVAR_TOPOLOGY":  "one-hop",
			"CONNECT_PERFVAR_RUN_COUNT": "5",
		}
		config, err := loadPerfvarConfig(func(name string) string { return values[name] })
		if err != nil {
			t.Fatal(err)
		}
		arms[index], err = resolvePerfvarScenarios(config)
		if err != nil || len(arms[index]) != 12 {
			t.Fatalf("arm %s: scenarios=%d err=%v", feature, len(arms[index]), err)
		}
	}
	for cell, ack := range arms[0] {
		noAck := arms[1][cell]
		ackHash, err := ack.hash()
		if err != nil {
			t.Fatal(err)
		}
		noAckHash, err := noAck.hash()
		if err != nil {
			t.Fatal(err)
		}
		if ackHash == noAckHash {
			t.Fatal("distinct UDP arms share one scenario record identity")
		}
		seen := map[string]bool{}
		for repetition := 1; repetition <= 5; repetition++ {
			first, err := perfvarTraceForRun(ack, repetition)
			if err != nil {
				t.Fatal(err)
			}
			second, err := perfvarTraceForRun(noAck, repetition)
			if err != nil {
				t.Fatal(err)
			}
			if first != second || seen[first.IdentityHash] {
				t.Fatalf("cell %d repetition %d has unmatched or repeated traces", cell, repetition)
			}
			seen[first.IdentityHash] = true
		}
	}
}

func TestNoAckSendTrackerExactOwnerRecoveryOnly(t *testing.T) {
	for _, test := range []struct {
		name                         string
		err                          error
		recovered, overflow, failure bool
	}{
		{"exact refused input recovered", clientconnect.ErrNoAckSendNotAdmitted, true, false, false},
		{"refused input lost", clientconnect.ErrNoAckSendNotAdmitted, false, false, true},
		{"owner history overflow", clientconnect.ErrNoAckSendNotAdmitted, true, true, true},
		{"accepted write failure", errors.New("write failed"), true, false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			tracker := newNoAckSendTracker()
			defer tracker.close()
			observer := tracker.newObserver()
			identity := clientconnect.NoAckSendObservation{Phase: clientconnect.NoAckSendPhaseStarted, ClientId: clientconnect.NewId(), Token: 1}
			observer(identity)
			identity.Phase, identity.Err = clientconnect.NoAckSendPhaseCompleted, test.err
			identity.RecoveredByOwner, identity.OwnerTrackingOverflow = test.recovered, test.overflow
			observer(identity)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if _, ok := tracker.boundary(ctx); !ok {
				t.Fatal("failed to join observer boundary")
			}
			if got := tracker.failures() != 0; got != test.failure {
				t.Fatalf("failure=%t want=%t", got, test.failure)
			}
		})
	}
}

func TestNoAckSendTrackerInvalidBoundaryHasCause(t *testing.T) {
	tracker := newNoAckSendTracker()
	defer tracker.close()
	observer := tracker.newObserver()
	// A completion without its Started event must fail even while ctx lives.
	observer(clientconnect.NoAckSendObservation{Phase: clientconnect.NoAckSendPhaseCompleted})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, ok := tracker.boundary(ctx); ok || ctx.Err() != nil {
		t.Fatal("invalid observation did not fail independently of its live context")
	}
	err := tracker.boundaryError(ctx)
	if err == nil || err.Error() != "invalid NoAck observation history: started=0 completed=0" {
		t.Fatalf("invalid boundary lost its diagnostic cause: %v", err)
	}
}

func applyPerfvarUdpTransferPolicy(settings any, features []string) error {
	noAck, explicit, err := perfvarUdpTransferPolicy(features)
	if err != nil || !explicit {
		return err
	}
	if err := setPerfvarBoolField(settings, "UdpTransferNoAck", noAck); err != nil {
		return fmt.Errorf("matched UDP policy: %w", err)
	}
	// The old client collapse switch also demotes UDP. A matched policy arm
	// must not inherit that unrelated compatibility override.
	if _, exists := perfvarBoolField(settings, "UdpCollapsePrevention"); exists {
		return setPerfvarBoolField(settings, "UdpCollapsePrevention", false)
	}
	return nil
}

func TestPerfvarUdpTransferPolicyMatchedEndpoints(t *testing.T) {
	for _, feature := range []string{perfvarFeatureUdpTransferAck, perfvarFeatureUdpTransferNoAck} {
		t.Run(feature, func(t *testing.T) {
			client := clientconnect.DefaultMultiClientSettings()
			client.UdpCollapsePrevention = true // Explicit arm must defeat old override.
			provider := clientconnect.DefaultRemoteUserNatProviderSettings()
			for _, settings := range []any{client, provider} {
				if err := applyPerfvarUdpTransferPolicy(settings, []string{feature}); err != nil {
					t.Fatal(err)
				}
				value, exists := perfvarBoolField(settings, "UdpTransferNoAck")
				if !exists || value != (feature == perfvarFeatureUdpTransferNoAck) {
					t.Fatalf("unmatched %T policy: exists=%t noack=%t", settings, exists, value)
				}
			}
			if client.UdpCollapsePrevention {
				t.Fatal("legacy collapse setting changed an explicit UDP arm")
			}
		})
	}
}

func TestPerfvarUdpTransferPolicyConflictAndCompatibility(t *testing.T) {
	if _, _, err := perfvarUdpTransferPolicy([]string{perfvarFeatureUdpTransferAck, perfvarFeatureUdpTransferNoAck}); err == nil {
		t.Fatal("conflicting UDP policies were accepted")
	}
	if err := applyPerfvarUdpTransferPolicy(&struct{}{}, []string{perfvarFeatureUdpTransferAck}); err == nil {
		t.Fatal("an unsupported endpoint silently fell back to its old policy")
	}
	settings := clientconnect.DefaultMultiClientSettings()
	settings.UdpTransferNoAck, settings.UdpCollapsePrevention = false, true
	if err := applyPerfvarUdpTransferPolicy(settings, nil); err != nil || settings.UdpTransferNoAck || !settings.UdpCollapsePrevention {
		t.Fatal("an ordinary non-experiment scenario changed its settings")
	}
	_, err := loadPerfvarConfig(func(name string) string {
		if name == "CONNECT_PERFVAR_FEATURE" {
			return perfvarFeatureUdpTransferAck + "," + perfvarFeatureUdpTransferNoAck
		}
		return ""
	})
	if err == nil {
		t.Fatal("environment parser accepted conflicting UDP arms")
	}
}
