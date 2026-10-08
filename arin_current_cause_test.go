package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/netip"
	"slices"
	"testing"
	"time"

	"github.com/maxmind/mmdbwriter/mmdbtype"
	mmdb "github.com/oschwald/maxminddb-golang/v2"
)

type currentCauseOwner struct {
	snapshot  ArinShadowOwnerSnapshot
	available bool
}

func (o *currentCauseOwner) ArinShadowCurrentConnection() (ArinShadowOwnerSnapshot, bool) {
	return o.snapshot, o.available
}

func currentCauseFixture(t *testing.T) (ArinCurrentCauseRequest, *currentCauseOwner, ArinShadowCaptureFacts, *ArinCurrentCause, time.Time) {
	t.Helper()
	now := NowUtc()
	request := ArinCurrentCauseRequest{ExpectedEpoch: 1791310718, LookupNotBefore: time.Unix(1791329165, 0).UTC(), Connections: []Id{NewId()}}
	owner := &currentCauseOwner{ArinShadowOwnerSnapshot{ConnectionId: request.Connections[0], ClientId: NewId(), HandlerId: NewId(), Address: netip.MustParseAddr("192.0.2.42"), At: now}, true}
	fact := ArinShadowCaptureFacts{ConnectionId: request.Connections[0], ClientId: owner.snapshot.ClientId, HandlerId: owner.snapshot.HandlerId, ObservedAt: now, Connected: true, Present: true,
		Actual: ArinShadowActiveFacts{Epoch: request.ExpectedEpoch, At: now.Add(-time.Minute), NonQuality: true}}
	cause := &ArinCurrentCause{DatabaseBuildEpoch: request.ExpectedEpoch, State: "unknown", NonQuality: true, RegistrationAttribution: "unavailable", OriginAttribution: "present",
		Origin: ArinShadowOrigin{ASNs: []uint32{12345}, UseState: "withheld"}, OriginWithheldReason: "insufficient-origin-visibility"}
	return request, owner, fact, cause, now
}

func TestArinCurrentCauseBoundRefusalBeforeCallbacks(t *testing.T) {
	request, _, _, _, now := currentCauseFixture(t)
	for _, kind := range []string{"empty", "65", "duplicate", "zero", "epoch", "floor"} {
		t.Run(kind, func(t *testing.T) {
			candidate := request
			candidate.Connections = slices.Clone(request.Connections)
			switch kind {
			case "empty":
				candidate.Connections = nil
			case "65":
				for range 64 {
					candidate.Connections = append(candidate.Connections, NewId())
				}
			case "duplicate":
				candidate.Connections = append(candidate.Connections, candidate.Connections[0])
			case "zero":
				candidate.Connections[0] = Id{}
			case "epoch":
				candidate.ExpectedEpoch = 0
			case "floor":
				candidate.LookupNotBefore = time.Unix(candidate.ExpectedEpoch-1, 0)
			}
			called := false
			owners := func(context.Context, []Id) ([]ArinShadowCaptureTarget, error) { called = true; return nil, nil }
			facts := func(context.Context, []Id) ([]ArinShadowCaptureFacts, error) { called = true; return nil, nil }
			lookup := func(netip.Addr) (*ArinCurrentCause, error) { called = true; return nil, nil }
			if _, err := readCurrentArinCauses(t.Context(), candidate, owners, facts, lookup, func() time.Time { return now }); err == nil || called {
				t.Fatal("invalid request reached an owning callback")
			}
		})
	}
}

func TestArinCurrentCauseOwnerAndSelectedFactFences(t *testing.T) {
	for _, kind := range []string{"qualified", "owner_unavailable", "owner_changed", "facts_unavailable", "facts_stale", "binding_mismatch", "stored_epoch", "reader_epoch", "reader_flags", "lookup_unavailable"} {
		t.Run(kind, func(t *testing.T) {
			request, owner, fact, cause, now := currentCauseFixture(t)
			want, lookupCalls := kind, 0
			switch kind {
			case "owner_unavailable":
				owner.available = false
			case "facts_unavailable":
				fact.Present = false
			case "facts_stale":
				fact.ObservedAt = now.Add(-time.Minute)
			case "binding_mismatch":
				fact.HandlerId = NewId()
			case "stored_epoch":
				fact.Actual.Epoch--
				want = "active_mismatch"
			case "reader_epoch":
				cause.DatabaseBuildEpoch--
				want = "active_mismatch"
			case "reader_flags":
				cause.State = "subscriber"
				cause.NonQuality = false
				cause.Verified = true
				want = "active_mismatch"
			}
			owners := func(context.Context, []Id) ([]ArinShadowCaptureTarget, error) {
				return []ArinShadowCaptureTarget{{request.Connections[0], owner}}, nil
			}
			facts := func(context.Context, []Id) ([]ArinShadowCaptureFacts, error) {
				return []ArinShadowCaptureFacts{fact}, nil
			}
			lookup := func(address netip.Addr) (*ArinCurrentCause, error) {
				lookupCalls++
				if address != owner.snapshot.Address {
					t.Fatal("address binding changed")
				}
				if kind == "owner_changed" {
					owner.snapshot.HandlerId = NewId()
				}
				if kind == "lookup_unavailable" {
					return nil, ErrArinShadowInput
				}
				return cause, nil
			}
			reply, err := readCurrentArinCauses(t.Context(), request, owners, facts, lookup, func() time.Time { return now })
			if err != nil || len(reply.Rows) != 1 || reply.Rows[0].ConnectionId != request.Connections[0] || reply.Rows[0].Reason != want {
				t.Fatal("current keyed refusal lost", kind, err, reply)
			}
			row := reply.Rows[0]
			if kind == "qualified" {
				if row.Cause == nil || row.Cause.OriginWithheldReason != "insufficient-origin-visibility" || !row.ActualAt.Equal(fact.Actual.At) {
					t.Fatal("cause or immutable clock lost")
				}
			} else if row.Cause != nil || row.ClientId != (Id{}) || row.HandlerId != (Id{}) || !row.ActualAt.IsZero() || !row.ObservedAt.IsZero() {
				t.Fatal("refused row retained classification evidence")
			}
			if slices.Contains([]string{"owner_unavailable", "facts_unavailable", "facts_stale", "binding_mismatch", "stored_epoch"}, kind) && lookupCalls != 0 {
				t.Fatal("unqualified source triggered a lookup")
			}
			encoded, _ := json.Marshal(reply)
			if bytes.Contains(encoded, []byte("192.0.2.")) {
				t.Fatal("address crossed private IPC")
			}
		})
	}
}

func TestArinCurrentCauseActualDecoderAndClosedReasons(t *testing.T) {
	for _, state := range []string{"subscriber", "excluded", "unknown", "ambiguous"} {
		for _, reason := range []string{"", "insufficient-origin-visibility", "rpki-invalid-origin", "outside-reviewed-countries", "registry-hosting-assignment"} {
			for _, risk := range []bool{false, true} {
				record := mmdbtype.Map{"classifier_version": mmdbtype.Uint32(1), "quality_policy_version": mmdbtype.Uint32(2),
					"quality_state": mmdbtype.String(state), "non_quality": mmdbtype.Bool(state != "subscriber"), "risk": mmdbtype.Bool(risk),
					"origin_asns": mmdbtype.Slice{mmdbtype.Uint32(12345), mmdbtype.Uint32(12346)}, "origin_use_state": mmdbtype.String("withheld"), "origin_withheld_reason": mmdbtype.String(reason),
					"org_handle": mmdbtype.String("TEST-ORG"), "net_handle": mmdbtype.String("TEST-NET"), "classification_rule": mmdbtype.String("private-192.0.2.42-rule")}
				db, err := mmdb.OpenBytes(testExceptionDatabase(t, string(schemaTypeArinDb), map[string]mmdbtype.Map{"192.0.2.0/24": record}))
				if err != nil {
					t.Fatal(err)
				}
				cause, err := currentArinCauseFromDatabase(db, netip.MustParseAddr("192.0.2.42"))
				db.Close()
				if err != nil || cause.State != state || cause.Risk != risk || cause.Verified != (state == "subscriber" && !risk) || cause.OriginWithheldReason != reason || cause.RegistrationAttribution != "single" || cause.RuleSHA256 == "" {
					t.Fatal("cause decoder altered classification or dropped provenance", err, cause)
				}
				encoded, _ := json.Marshal(cause)
				if bytes.Contains(encoded, []byte("192.0.2.")) {
					t.Fatal("rule text/address leaked")
				}
			}
		}
	}
	_, _, _, cause, _ := currentCauseFixture(t)
	cause.OriginWithheldReason = "arbitrary address or diagnostic text"
	if validArinCurrentCause(*cause) {
		t.Fatal("free-form reason accepted")
	}
	cause.OriginWithheldReason = "rpki-invalid-origin"
	cause.Origin.UseState = "subscriber"
	if validArinCurrentCause(*cause) {
		t.Fatal("inconsistent reason accepted")
	}
}

func TestArinCurrentCauseAuthenticatedWireAndNoColdReaders(t *testing.T) {
	request, owner, fact, cause, _ := currentCauseFixture(t)
	owners := func(context.Context, []Id) ([]ArinShadowCaptureTarget, error) {
		return []ArinShadowCaptureTarget{{request.Connections[0], owner}}, nil
	}
	facts := func(context.Context, []Id) ([]ArinShadowCaptureFacts, error) {
		fact.ObservedAt = NowUtc()
		return []ArinShadowCaptureFacts{fact}, nil
	}
	key, run, identity := [32]byte{19}, NewId(), shadowRPCTestIdentity()
	service, err := NewArinShadowRPCService(t.Context(), run, key, identity, func(ctx context.Context, method string, input json.RawMessage) (any, error) {
		if method != ArinCurrentCauseMethod {
			t.Fatal("wrong method")
		}
		var got ArinCurrentCauseRequest
		if DecodeArinShadowRPC(input, &got) != nil {
			return nil, ErrArinShadowInput
		}
		return readCurrentArinCauses(ctx, got, owners, facts, func(netip.Addr) (*ArinCurrentCause, error) { return cause, nil }, NowUtc)
	})
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewArinShadowRPCClient(run, key, identity, shadowRPCTestSocket(t, service))
	if err != nil {
		t.Fatal(err)
	}
	reply, err := CallArinCurrentCauses(t.Context(), client, request)
	if err != nil || len(reply.Rows) != 1 || reply.Rows[0].Reason != "qualified" || reply.Rows[0].Cause == nil {
		t.Fatal("authenticated point capture failed", err)
	}
	// Legacy capture remains its existing strict schema, with no added field.
	legacy, _ := json.Marshal(ArinShadowOrigin{ASNs: []uint32{12345}, UseState: "withheld"})
	if bytes.Contains(legacy, []byte("withheld_reason")) {
		t.Fatal("new method changed legacy capture wire")
	}
}

func TestArinCurrentCauseAggregationRemovesPrivateKeys(t *testing.T) {
	request, owner, fact, cause, now := currentCauseFixture(t)
	missing := NewId()
	reply := ArinCurrentCauseReply{ExpectedEpoch: request.ExpectedEpoch, LookupNotBefore: request.LookupNotBefore, Rows: []ArinCurrentCauseRow{
		{ConnectionId: request.Connections[0], ClientId: owner.snapshot.ClientId, HandlerId: owner.snapshot.HandlerId, ActualAt: fact.Actual.At, ObservedAt: now, CapturedAt: now, Reason: "qualified", Cause: cause},
		{ConnectionId: missing, CapturedAt: now, Reason: "owner_unavailable"},
	}}
	report, err := AggregateArinCurrentCauses(reply)
	if err != nil || report.RequestedConnections != 2 || report.QualifiedConnections != 1 || report.Reasons["owner_unavailable"] != 1 || len(report.Causes) != 1 || report.Causes[0].Connections != 1 {
		t.Fatal("aggregate discarded missing rows or changed partitions", err, report)
	}
	encoded, _ := json.Marshal(report)
	for _, private := range []string{request.Connections[0].String(), owner.snapshot.ClientId.String(), owner.snapshot.HandlerId.String(), missing.String(), "192.0.2.42"} {
		if bytes.Contains(encoded, []byte(private)) {
			t.Fatal("private identity escaped public aggregate")
		}
	}
	reply.Rows[1].Cause = cause
	if _, err := AggregateArinCurrentCauses(reply); err == nil {
		t.Fatal("unqualified cause counted")
	}
}

func TestArinCurrentCauseMissingRecordAndAttributionBounds(t *testing.T) {
	record := mmdbtype.Map{"classifier_version": mmdbtype.Uint32(1), "quality_policy_version": mmdbtype.Uint32(2),
		"quality_state": mmdbtype.String("ambiguous"), "non_quality": mmdbtype.Bool(true), "risk": mmdbtype.Bool(false),
		"multiple_registration_owners": mmdbtype.Bool(true), "org_handle": mmdbtype.String("TEST-ORG"),
		"origin_use_state": mmdbtype.String("ambiguous"), "origin_asns": mmdbtype.Slice{}}
	for n := uint32(1); n <= 9; n++ {
		record["origin_asns"] = append(record["origin_asns"].(mmdbtype.Slice), mmdbtype.Uint32(n))
	}
	db, err := mmdb.OpenBytes(testExceptionDatabase(t, string(schemaTypeArinDb), map[string]mmdbtype.Map{"192.0.2.0/24": record}))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cause, err := currentArinCauseFromDatabase(db, netip.MustParseAddr("192.0.2.42"))
	if err != nil || cause.RegistrationAttribution != "multiple" || cause.Registration != (ArinShadowRegistration{}) || cause.OriginAttribution != "overflow" || len(cause.Origin.ASNs) != 0 {
		t.Fatal("ambiguous ownership or origin overflow became a definite identity", err, cause)
	}
	if _, err := currentArinCauseFromDatabase(db, netip.MustParseAddr("198.51.100.1")); err == nil {
		t.Fatal("absent record became definite cause")
	}
	if _, err := currentArinCauseFromDatabase(nil, netip.MustParseAddr("192.0.2.42")); err == nil {
		t.Fatal("missing reader became definite cause")
	}
}

func TestArinCurrentCauseMax64AndMissingOwnersStayUnknown(t *testing.T) {
	request, _, _, cause, now := currentCauseFixture(t)
	request.Connections = nil
	entries := []ArinCurrentCauseSampleEntry{}
	for range ArinCurrentCauseLimit {
		connection := NewId()
		request.Connections = append(request.Connections, connection)
		entries = append(entries, ArinCurrentCauseSampleEntry{ClientId: NewId(), ConnectionId: connection, HandlerId: NewId()})
	}
	if !validArinCurrentCauseRequest(request, now) {
		t.Fatal("exact64 boundary refused")
	}
	report, err := CollectArinCurrentCauseSample(t.Context(), entries, nil, request.ExpectedEpoch, request.LookupNotBefore)
	if err != nil || report.RequestedConnections != 64 || report.QualifiedConnections != 0 || report.Reasons["owner_unavailable"] != 64 || len(report.Causes) != 0 {
		t.Fatal("missing64 owners became clean or disappeared", err, report)
	}
	// The fixed response cap also covers64 large typed attribution rows.
	cause.Origin.ASNs = []uint32{4294967288, 4294967289, 4294967290, 4294967291, 4294967292, 4294967293, 4294967294, 4294967295}
	reply := ArinCurrentCauseReply{ExpectedEpoch: request.ExpectedEpoch, LookupNotBefore: request.LookupNotBefore}
	for _, entry := range entries {
		reply.Rows = append(reply.Rows, ArinCurrentCauseRow{ConnectionId: entry.ConnectionId, ClientId: entry.ClientId, HandlerId: entry.HandlerId, ActualAt: now, ObservedAt: now, CapturedAt: now, Reason: "qualified", Cause: cause})
	}
	encoded, err := json.Marshal(reply)
	if err != nil || len(encoded)+4096 > ArinShadowRPCResponseLimit {
		t.Fatal("64-row cause reply exceeds unchanged response cap", err, len(encoded))
	}
	entries[1].ClientId = entries[0].ClientId
	if _, err := CollectArinCurrentCauseSample(t.Context(), entries, nil, request.ExpectedEpoch, request.LookupNotBefore); err == nil {
		t.Fatal("one provider duplicated in sample")
	}
}
