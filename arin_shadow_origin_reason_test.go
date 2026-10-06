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

// JSON projection keeps the same controls compilable against the old decoder.
// The old result must fail for missing evidence, rather than fail to compile.
func originReasonProjection(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var row map[string]json.RawMessage
	if err := json.Unmarshal(data, &row); err != nil {
		t.Fatal(err)
	}
	var reason string
	if raw, present := row["withheld_reason"]; present {
		if err := json.Unmarshal(raw, &reason); err != nil {
			t.Fatal(err)
		}
	}
	return reason
}

func originReasonFromJSON(t *testing.T, state, reason string) ArinShadowOrigin {
	t.Helper()
	data, err := json.Marshal(map[string]any{"asns": []uint32{12345, 12346}, "use_state": state, "withheld_reason": reason})
	if err != nil {
		t.Fatal(err)
	}
	var origin ArinShadowOrigin
	if err := json.Unmarshal(data, &origin); err != nil {
		t.Fatal(err)
	}
	return origin
}

func TestArinOriginWithheldReasonActualDecoder(t *testing.T) {
	for _, reason := range []string{"insufficient-origin-visibility", "rpki-invalid-origin", "outside-reviewed-countries", "registry-hosting-assignment"} {
		for _, state := range []string{"subscriber", "unknown", "excluded", "ambiguous"} {
			for _, risk := range []bool{false, true} {
				record := mmdbtype.Map{
					"classifier_version": mmdbtype.Uint32(1), "quality_policy_version": mmdbtype.Uint32(2),
					"quality_state": mmdbtype.String(state), "non_quality": mmdbtype.Bool(state != "subscriber"), "risk": mmdbtype.Bool(risk),
					"origin_asns": mmdbtype.Slice{mmdbtype.Uint32(12345), mmdbtype.Uint32(12346)}, "origin_use_state": mmdbtype.String("withheld"),
					"origin_withheld_reason": mmdbtype.String(reason),
				}
				data := testExceptionDatabase(t, string(schemaTypeArinDb), map[string]mmdbtype.Map{"192.0.2.0/24": record})
				db, err := mmdb.OpenBytes(data)
				if err != nil {
					t.Fatal(err)
				}
				facts, info, err := shadowFacts(db, netip.MustParseAddr("192.0.2.42"))
				db.Close()
				if err != nil || facts.state != state || facts.risk != risk || info.QualityVerified() != (state == "subscriber" && !risk) || !slices.Equal(facts.origin.ASNs, []uint32{12345, 12346}) {
					t.Fatal("diagnostic evidence changed classifier or routing identity", err)
				}
				if got := originReasonProjection(t, facts.origin); got != reason {
					t.Fatalf("MMDB withholding discriminator lost: got %q want %q", got, reason)
				}
			}
		}
	}
}

func TestArinOriginWithheldReasonAggregatePartition(t *testing.T) {
	r, _, _ := captureFixture(t)
	s, err := r.NewCurrentCaptureStream(context.Background(), []string{"all"})
	if err != nil {
		t.Fatal(err)
	}
	reasons := []string{"", "insufficient-origin-visibility", "outside-reviewed-countries", "registry-hosting-assignment", "rpki-invalid-origin"}
	for _, reason := range reasons {
		s.addOrigin(arinShadowFacts{state: "unknown", origin: originReasonFromJSON(t, "withheld", reason)})
	}
	s.addOrigin(arinShadowFacts{state: "excluded", risk: true, origin: originReasonFromJSON(t, "withheld", "registry-hosting-assignment")})
	s.finishOrigins()
	if len(s.report.OriginCounts) != len(reasons) || s.report.OriginUnattributedConnections != 0 || s.report.Providers != 0 || s.report.CapturedConnections != 0 {
		t.Fatal("withholding reasons collapsed or attribution invented population")
	}
	seen := map[string]bool{}
	connections := int64(0)
	for _, row := range s.report.OriginCounts {
		reason := originReasonProjection(t, row)
		if seen[reason] || !slices.Contains(reasons, reason) || row.UseState != "withheld" || !slices.Equal(row.ASNs, []uint32{12345, 12346}) || row.Subscriber != 0 {
			t.Fatal("reason/ASN set attribution changed")
		}
		seen[reason] = true
		connections += row.Connections
		if reason == "registry-hosting-assignment" && (row.Connections != 2 || row.Unknown != 1 || row.Excluded != 1 || row.Risk != 1) {
			t.Fatal("independent exclusion/risk facts lost")
		}
	}
	if connections != 6 {
		t.Fatal("connection partition is not exact")
	}
}

func TestArinOriginWithheldReasonWireAndBounds(t *testing.T) {
	for _, bad := range []struct{ state, reason string }{
		{"subscriber", "rpki-invalid-origin"}, {"unknown", "outside-reviewed-countries"},
		{"withheld", "192.0.2.42"}, {"withheld", "arbitrary diagnostic text"},
		{"withheld", strings.Repeat("a", 4096)},
	} {
		if validArinShadowOrigin(originReasonFromJSON(t, bad.state, bad.reason)) {
			t.Fatal("inconsistent or free-form provenance accepted")
		}
	}
	var legacy ArinShadowOrigin
	if DecodeArinShadowRPC([]byte(`{"asns":[12345],"use_state":"withheld"}`), &legacy) != nil || !validArinShadowOrigin(legacy) || originReasonProjection(t, legacy) != "" {
		t.Fatal("new coordinator rejected or invented omitted legacy evidence")
	}
	current := originReasonFromJSON(t, "withheld", "insufficient-origin-visibility")
	data, _ := json.Marshal(current)
	var old struct {
		ASNs     []uint32 `json:"asns"`
		UseState string   `json:"use_state"`
	}
	if DecodeArinShadowRPC(data, &old) != ErrArinShadowInput {
		t.Fatal("old strict coordinator accepted new evidence; coordinator-first boundary lost")
	}
	now := time.Now().UTC()
	row := arinShadowCaptureWireRow{ConnectionId: NewId(), ClientId: NewId(), HandlerId: NewId(), ActualAt: now, ObservedAt: now, CapturedAt: now, State: "ambiguous", Reason: "qualified", Registration: newArinShadowRegistration(strings.Repeat("A", 64), strings.Repeat("N", 64), strings.Repeat("C", 64), strings.Repeat("R", 64)), Origin: current}
	row.Origin.ASNs = []uint32{4294967288, 4294967289, 4294967290, 4294967291, 4294967292, 4294967293, 4294967294, 4294967295}
	reply := arinShadowCaptureRPCReply{Rows: make([]arinShadowCaptureWireRow, ArinShadowCaptureBatchLimit)}
	for i := range reply.Rows {
		reply.Rows[i] = row
	}
	data, err := json.Marshal(reply)
	if err != nil || len(data)+4096 > ArinShadowRPCResponseLimit {
		t.Fatalf("maximum typed response exceeds unchanged cap: %d", len(data))
	}
}

func TestArinOriginWithheldReasonAuthenticatedCapture(t *testing.T) {
	r, actual := shadowFixture(t, false)
	const reason = "registry-hosting-assignment"
	data := testExceptionDatabase(t, string(schemaTypeArinDb), map[string]mmdbtype.Map{"192.0.2.0/24": {
		"classifier_version": mmdbtype.Uint32(1), "quality_policy_version": mmdbtype.Uint32(2),
		"quality_state": mmdbtype.String("unknown"), "non_quality": mmdbtype.Bool(true), "risk": mmdbtype.Bool(false),
		"origin_asns": mmdbtype.Slice{mmdbtype.Uint32(12345), mmdbtype.Uint32(12346)}, "origin_use_state": mmdbtype.String("withheld"),
		"origin_withheld_reason": mmdbtype.String(reason),
	}})
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
			t.Error("provider address crossed RPC")
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
	if err != nil || len(batch.rows) != 1 || batch.rows[0].reason != "qualified" || batch.rows[0].facts.state != "unknown" || batch.rows[0].facts.risk || batch.rows[0].facts.verified {
		t.Fatal("authenticated current-owner classification changed", err)
	}
	if originReasonProjection(t, batch.rows[0].facts.origin) != reason {
		t.Fatal("typed reason lost across authenticated capture or sealed batch")
	}
	page := ArinShadowCapturePage{GenerationSHA256: cohort.GenerationSHA256, Records: []ArinShadowCaptureRecord{{ConnectionId: id, HandlerId: handlerId, Provider: &ArinShadowCaptureProvider{ClientId: clientId, ExpectedConnections: 1, Buckets: []string{"all", "us"}, BaseQuality: true, BaseSpeed: true, ActiveQuality: true, ActiveSpeed: true}}}}
	if err = collector.ConsumeCaptured(page, batch); err != nil {
		t.Fatal(err)
	}
	report, err := collector.Finish(cohort, true)
	if err != nil || !report.ObservationComplete || report.ActualMainCoverage || len(report.OriginCounts) != 1 || originReasonProjection(t, report.OriginCounts[0]) != reason {
		t.Fatal("reducer dropped reason or asserted Main evidence", err)
	}
	for _, bucket := range report.Buckets {
		if bucket.QualityRemoved != 1 || bucket.CandidateQuality != 0 || bucket.CandidateSpeed != 1 || bucket.Unknown != 1 {
			t.Fatal("provenance altered eligibility")
		}
	}
	encoded, _ := json.Marshal(report)
	for _, private := range []string{"192.0.2.", id.String(), clientId.String(), handlerId.String()} {
		if bytes.Contains(encoded, []byte(private)) {
			t.Fatal("private identity escaped aggregate")
		}
	}
}
