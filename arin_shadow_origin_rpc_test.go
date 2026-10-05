//go:build linux

package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/maxmind/mmdbwriter/mmdbtype"
	mmdb "github.com/oschwald/maxminddb-golang/v2"
)

func TestArinOriginCurrentOwnerRPCPreservesUnknownForResearch(t *testing.T) {
	for _, originState := range []string{"unknown", "withheld"} {
		t.Run(originState, func(t *testing.T) { testArinOriginCurrentOwnerResearchRPC(t, originState) })
	}
}

func testArinOriginCurrentOwnerResearchRPC(t *testing.T, originState string) {
	r, actual := shadowFixture(t, false)
	data := testExceptionDatabase(t, string(schemaTypeArinDb), map[string]mmdbtype.Map{
		"192.0.2.0/24": {
			"classifier_version": mmdbtype.Uint32(1), "quality_policy_version": mmdbtype.Uint32(2),
			"quality_state": mmdbtype.String("unknown"), "non_quality": mmdbtype.Bool(true), "risk": mmdbtype.Bool(false),
			"origin_asns": mmdbtype.Slice{mmdbtype.Uint32(12345), mmdbtype.Uint32(12346)}, "origin_use_state": mmdbtype.String(originState),
		},
	})
	candidate, err := mmdb.OpenBytes(data)
	if err != nil {
		t.Fatal(err)
	}
	r.candidate.Close()
	r.candidate = candidate
	sum := sha256.Sum256(data)
	r.candidateHash = hex.EncodeToString(sum[:])
	id, clientId, handlerId := NewId(), NewId(), NewId()
	owner := &shadowCaptureTestOwner{available: true, snapshot: ArinShadowOwnerSnapshot{ConnectionId: id, ClientId: clientId, HandlerId: handlerId, Address: netip.MustParseAddr("192.0.2.42"), At: NowUtc()}}
	key, run, identity := [32]byte{7}, NewId(), shadowRPCTestIdentity()
	service, err := NewArinShadowRPCService(context.Background(), run, key, identity, func(ctx context.Context, method string, input json.RawMessage) (any, error) {
		return r.CaptureRPC(ctx, input, func(context.Context, []Id) ([]ArinShadowCaptureTarget, error) {
			return []ArinShadowCaptureTarget{{id, owner}}, nil
		}, func(context.Context, []Id) ([]ArinShadowCaptureFacts, error) {
			return []ArinShadowCaptureFacts{{ConnectionId: id, ClientId: clientId, HandlerId: handlerId, ObservedAt: NowUtc(), Connected: true, Present: true, Actual: actual}}, nil
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	transport := shadowRPCTestSocket(t, service)
	client, err := NewArinShadowRPCClient(run, key, identity, func(ctx context.Context, request []byte) ([]byte, error) {
		reply, err := transport(ctx, request)
		if bytes.Contains(request, []byte("192.0.2.")) || bytes.Contains(reply, []byte("192.0.2.")) {
			t.Error("origin attribution exported the provider address")
		}
		return reply, err
	})
	if err != nil {
		t.Fatal(err)
	}
	cohort := ArinShadowCaptureCohort{GenerationSHA256: strings.Repeat("ab", 32), ObservedAt: NowUtc(), Providers: 1, Connections: 1}
	collector, err := r.NewCurrentCaptureCollector(context.Background(), cohort, []string{"all", "us"})
	if err != nil {
		t.Fatal(err)
	}
	batch, err := r.CallCapture(context.Background(), client, []Id{id})
	if err != nil {
		t.Fatal(err)
	}
	page := ArinShadowCapturePage{GenerationSHA256: cohort.GenerationSHA256, Records: []ArinShadowCaptureRecord{{
		ConnectionId: id, HandlerId: handlerId,
		Provider: &ArinShadowCaptureProvider{ClientId: clientId, ExpectedConnections: 1, Buckets: []string{"all", "us"}, BaseQuality: true, BaseSpeed: true, ActiveQuality: true, ActiveSpeed: true},
	}}}
	if err = collector.ConsumeCaptured(page, batch); err != nil {
		t.Fatal(err)
	}
	report, err := collector.Finish(cohort, true)
	if err != nil || !report.ObservationComplete || !report.CensusComplete || report.ActualMainCoverage || report.Providers != 1 || len(report.OriginCounts) != 1 || report.OriginCounts[0].Unknown != 1 || report.OriginCounts[0].Connections != 1 || !slices.Equal(report.OriginCounts[0].ASNs, []uint32{12345, 12346}) || report.RegistrationUnattributedConnections != 1 || report.OriginUnattributedConnections != 0 {
		t.Fatal("current unknown network lost the research join or population boundary", err)
	}
	if report.OriginCounts[0].UseState != originState {
		t.Fatal("unreviewed or withheld origin identity changed in transport")
	}
	for _, bucket := range report.Buckets {
		if bucket.QualityRemoved != 1 || bucket.CandidateQuality != 0 || bucket.CandidateSpeed != 1 || bucket.Unknown != 1 {
			t.Fatal("origin identity changed Quality/Speed eligibility")
		}
	}
	encoded, _ := json.Marshal(report)
	for _, private := range []string{"192.0.2.", id.String(), clientId.String(), handlerId.String()} {
		if bytes.Contains(encoded, []byte(private)) {
			t.Fatal("private source identity escaped aggregate")
		}
	}
}

// Coordinator-first rollout accepts the exact old row schema as unattributed.
// The old strict decoder must refuse the new origin field rather than silently
// discard research identity; this documents the required producer rollout order.
func TestArinOriginCoordinatorFirstWireCompatibility(t *testing.T) {
	type legacyRow struct {
		ConnectionId Id                     `json:"connection_id"`
		ClientId     Id                     `json:"client_id"`
		HandlerId    Id                     `json:"handler_id"`
		ActualAt     time.Time              `json:"actual_at"`
		ObservedAt   time.Time              `json:"observed_at"`
		CapturedAt   time.Time              `json:"captured_at"`
		State        string                 `json:"state"`
		Risk         bool                   `json:"risk"`
		ProxyRisk    bool                   `json:"proxy_risk"`
		Verified     bool                   `json:"verified"`
		Reason       string                 `json:"reason"`
		Registration ArinShadowRegistration `json:"registration"`
	}
	type legacyReply struct {
		Pins ArinShadowResourcePins `json:"pins"`
		Rows []legacyRow            `json:"rows"`
	}
	now := NowUtc()
	old := legacyReply{Pins: ArinShadowResourcePins{ActiveSHA256: strings.Repeat("ab", 32), CandidateSHA256: strings.Repeat("cd", 32), ActiveEpoch: 1, CandidateEpoch: 2}, Rows: []legacyRow{{ConnectionId: NewId(), ClientId: NewId(), HandlerId: NewId(), ActualAt: now, ObservedAt: now, CapturedAt: now, State: "unknown", Reason: "qualified"}}}
	legacyBytes, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	var received arinShadowCaptureRPCReply
	if DecodeArinShadowRPC(legacyBytes, &received) != nil || received.Pins != old.Pins || len(received.Rows) != 1 || received.Rows[0].ConnectionId != old.Rows[0].ConnectionId || !validArinShadowOrigin(received.Rows[0].Origin) || len(received.Rows[0].Origin.ASNs) != 0 || received.Rows[0].Origin.UseState != "" {
		t.Fatal("new coordinator rejected or invented legacy origin evidence")
	}
	received.Rows[0].Origin = newArinShadowOrigin([]uint32{12345, 12346}, "unknown")
	currentBytes, err := json.Marshal(received)
	if err != nil {
		t.Fatal(err)
	}
	var current arinShadowCaptureRPCReply
	if DecodeArinShadowRPC(currentBytes, &current) != nil || !slices.Equal(current.Rows[0].Origin.ASNs, []uint32{12345, 12346}) || current.Rows[0].Origin.UseState != "unknown" {
		t.Fatal("new coordinator lost typed origin identity")
	}
	var oldConsumer legacyReply
	if DecodeArinShadowRPC(currentBytes, &oldConsumer) != ErrArinShadowInput {
		t.Fatal("old strict coordinator accepted an unrecognized origin field")
	}
}
