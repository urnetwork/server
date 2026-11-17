package model

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// §21.9's legal requests (abuse_legal_model.go; ABUSE-M5-17): filing, its refusals and clocks, the two sign-offs and
// closing. On the private stack, in a build with sidecar_pending.

// A request of instrument naming networkIds, filed now.
func testFileLegalRequest(t testing.TB, ctx context.Context, instrument LegalInstrument, sealed bool, networkIds ...server.Id) *LegalRequest {
	t.Helper()
	request, inserted, err := FileLegalRequest(ctx, &LegalRequestArgs{
		RequestId:  server.NewId(),
		Instrument: instrument,
		Country:    "US",
		NetworkIds: networkIds,
		Sealed:     sealed,
	}, server.NowUtc(), DefaultLegalSettings())
	if err != nil || !inserted {
		t.Fatalf("filing: %v (inserted %t)", err, inserted)
	}
	return request
}

// An EU production order is due in 10 days, 8 h in an emergency, and earlier when counsel says so; another
// instrument takes counsel's time or none.
func TestLegalRequestDueTimes(t *testing.T) {
	settings := DefaultLegalSettings()
	now := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	due := func(args *LegalRequestArgs) *time.Time {
		return legalRequestDueTime(args, now, settings)
	}
	if got := due(&LegalRequestArgs{Instrument: LegalInstrumentEuProductionOrder}); got == nil || !got.Equal(now.Add(10*24*time.Hour)) {
		t.Fatalf("a production order: %v", got)
	}
	if got := due(&LegalRequestArgs{Instrument: LegalInstrumentEuProductionOrder, Emergency: true}); got == nil || !got.Equal(now.Add(8*time.Hour)) {
		t.Fatalf("an emergency: %v", got)
	}
	counsel := now.Add(3 * 24 * time.Hour)
	if got := due(&LegalRequestArgs{Instrument: LegalInstrumentEuProductionOrder, DueTime: &counsel}); got == nil || !got.Equal(counsel) {
		t.Fatalf("counsel's earlier time: %v", got)
	}
	late := now.Add(30 * 24 * time.Hour)
	if got := due(&LegalRequestArgs{Instrument: LegalInstrumentEuProductionOrder, DueTime: &late}); got == nil || !got.Equal(now.Add(10*24*time.Hour)) {
		t.Fatalf("counsel's later time: %v", got)
	}
	if got := due(&LegalRequestArgs{Instrument: LegalInstrumentSubpoena, DueTime: &counsel}); got == nil || !got.Equal(counsel) {
		t.Fatalf("a subpoena with counsel's time: %v", got)
	}
	if got := due(&LegalRequestArgs{Instrument: LegalInstrumentSubpoena}); got != nil {
		t.Fatalf("a subpoena with none: %v", got)
	}
}

// A filing opens its own case, barred while sealed and naming its network when it names one; a retry adds nothing,
// another request under its id is refused, and every malformed filing is refused before anything is written.
func TestFileLegalRequestRecordsAndRefuses(t *testing.T) {
	requireLegalHoldTables(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		Testing_CreateNetwork(ctx, networkId, "legal-request-network", server.NewId())
		otherNetworkId := server.NewId()
		Testing_CreateNetwork(ctx, otherNetworkId, "legal-request-other", server.NewId())
		settings := DefaultLegalSettings()
		now := server.NowUtc()

		args := &LegalRequestArgs{
			RequestId:  server.NewId(),
			Instrument: LegalInstrumentEuProductionOrder,
			Country:    "DE",
			NetworkIds: []server.Id{networkId},
			Sealed:     true,
			Emergency:  true,
		}
		request, inserted, err := FileLegalRequest(ctx, args, now, settings)
		if err != nil || !inserted {
			t.Fatalf("filing: %v", err)
		}
		if request.ProductionState != LegalProductionStatePending || request.DueTime == nil ||
			request.DueTime.Sub(now.UTC()) != settings.EuProductionEmergencyDue || !request.SealedAt(now) || request.SignedOff() {
			t.Fatalf("the request: %+v", request)
		}
		abuseCase := GetAbuseCase(ctx, request.CaseId)
		if abuseCase == nil || abuseCase.NetworkId == nil || *abuseCase.NetworkId != networkId || !abuseCase.Barred ||
			abuseCase.Category != AbuseCategoryOther || abuseCase.ReporterClass != AbuseReporterClassLawEnforcement {
			t.Fatalf("its case: %+v", abuseCase)
		}
		retry, inserted, err := FileLegalRequest(ctx, args, now, settings)
		if err != nil || inserted || retry.RequestId != request.RequestId {
			t.Fatalf("the retry: %+v %t %v", retry, inserted, err)
		}
		conflict := *args
		conflict.Country = "FR"
		if _, _, err := FileLegalRequest(ctx, &conflict, now, settings); !errors.Is(err, ErrLegalRequestConflict) {
			t.Fatalf("another request under the id: %v", err)
		}

		// two networks, unsealed: a case naming no network, not barred
		pair := testFileLegalRequest(t, ctx, LegalInstrumentSubpoena, false, networkId, otherNetworkId)
		pairCase := GetAbuseCase(ctx, pair.CaseId)
		if pairCase.NetworkId != nil || pairCase.Barred || pair.CaseId == request.CaseId || len(pair.NetworkIds) != 2 {
			t.Fatalf("a two-network request: %+v, case %+v", pair, pairCase)
		}
		// a request on a case counsel names
		onCase, _, err := FileLegalRequest(ctx, &LegalRequestArgs{
			RequestId: server.NewId(), CaseId: &request.CaseId, Instrument: LegalInstrumentPreservation, Country: "US",
			NetworkIds: []server.Id{networkId},
		}, now, settings)
		if err != nil || onCase.CaseId != request.CaseId {
			t.Fatalf("a request on a named case: %+v %v", onCase, err)
		}

		until := now.Add(time.Hour)
		for name, bad := range map[string]*LegalRequestArgs{
			"an unknown instrument":  {RequestId: server.NewId(), Instrument: "letter", Country: "US"},
			"a country in lowercase": {RequestId: server.NewId(), Instrument: LegalInstrumentSubpoena, Country: "us"},
			"no request id":          {Instrument: LegalInstrumentSubpoena, Country: "US"},
			"an emergency subpoena":  {RequestId: server.NewId(), Instrument: LegalInstrumentSubpoena, Country: "US", Emergency: true},
			"a seal's end, no seal":  {RequestId: server.NewId(), Instrument: LegalInstrumentSubpoena, Country: "US", SealUntil: &until},
			"an account twice":       {RequestId: server.NewId(), Instrument: LegalInstrumentSubpoena, Country: "US", NetworkIds: []server.Id{networkId, networkId}},
			"no such account":        {RequestId: server.NewId(), Instrument: LegalInstrumentSubpoena, Country: "US", NetworkIds: []server.Id{server.NewId()}},
		} {
			if _, _, err := FileLegalRequest(ctx, bad, now, settings); !errors.Is(err, ErrLegalRequestInvalid) {
				t.Errorf("%s: %v", name, err)
			}
		}
		missingCase := server.NewId()
		if _, _, err := FileLegalRequest(ctx, &LegalRequestArgs{RequestId: server.NewId(), CaseId: &missingCase, Instrument: LegalInstrumentSubpoena, Country: "US"}, now, settings); !errors.Is(err, ErrAbuseCaseNotFound) {
			t.Fatalf("no such case: %v", err)
		}
	})
}

// A production needs a legal and an engineering sign-off by two people: one alone refuses, the same person twice
// refuses, a second person in a signed role refuses, two pass; a closed request takes no sign-off, and a request
// never produced closes declined.
func TestSignLegalProductionNeedsTwoPeople(t *testing.T) {
	requireLegalHoldTables(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		Testing_CreateNetwork(ctx, networkId, "legal-signoff-network", server.NewId())
		request := testFileLegalRequest(t, ctx, LegalInstrumentSearchWarrant, false, networkId)
		reader, err := NewAbuseReader(server.NewId(), DefaultAbuseSettings())
		if err != nil {
			t.Fatal(err)
		}
		produce := func() error {
			_, err := reader.ProduceLegalRequest(ctx, request.RequestId, server.NowUtc(), DefaultAbuseProductionSettings())
			return err
		}
		if err := produce(); !errors.Is(err, ErrLegalProductionUnsigned) {
			t.Fatalf("no sign-off: %v", err)
		}
		counsel := server.NewId()
		signed, err := SignLegalProduction(ctx, request.RequestId, counsel, LegalSignoffRoleLegal)
		if err != nil || signed.LegalSignoffId == nil || *signed.LegalSignoffId != counsel {
			t.Fatalf("the legal sign-off: %+v %v", signed, err)
		}
		if err := produce(); !errors.Is(err, ErrLegalProductionUnsigned) {
			t.Fatalf("one sign-off: %v", err)
		}
		if _, err := SignLegalProduction(ctx, request.RequestId, counsel, LegalSignoffRoleEngineering); !errors.Is(err, ErrLegalSignoffRefused) {
			t.Fatalf("one person both roles: %v", err)
		}
		if _, err := SignLegalProduction(ctx, request.RequestId, server.NewId(), LegalSignoffRoleLegal); !errors.Is(err, ErrLegalSignoffRefused) {
			t.Fatalf("a second legal sign-off: %v", err)
		}
		if again, err := SignLegalProduction(ctx, request.RequestId, counsel, LegalSignoffRoleLegal); err != nil || *again.LegalSignoffId != counsel {
			t.Fatalf("the same sign-off again: %v", err)
		}
		engineer := server.NewId()
		signed, err = SignLegalProduction(ctx, request.RequestId, engineer, LegalSignoffRoleEngineering)
		if err != nil || !signed.SignedOff() {
			t.Fatalf("the engineering sign-off: %+v %v", signed, err)
		}
		if err := produce(); err != nil {
			t.Fatalf("two sign-offs: %v", err)
		}
		closed, err := CloseLegalRequest(ctx, request.RequestId, server.NowUtc())
		if err != nil || closed.CloseTime == nil || closed.ProductionState != LegalProductionStateProduced {
			t.Fatalf("closing a produced request: %+v %v", closed, err)
		}
		if _, err := SignLegalProduction(ctx, request.RequestId, server.NewId(), LegalSignoffRoleLegal); !errors.Is(err, ErrLegalRequestClosed) {
			t.Fatalf("a sign-off on a closed request: %v", err)
		}
		if err := produce(); !errors.Is(err, ErrLegalRequestClosed) {
			t.Fatalf("a production of a closed request: %v", err)
		}
		declined := testFileLegalRequest(t, ctx, LegalInstrumentSubpoena, false, networkId)
		if closed, err := CloseLegalRequest(ctx, declined.RequestId, server.NowUtc()); err != nil || closed.ProductionState != LegalProductionStateDeclined {
			t.Fatalf("closing an unproduced request: %+v %v", closed, err)
		}
		if _, err := SignLegalProduction(ctx, server.NewId(), counsel, LegalSignoffRoleLegal); !errors.Is(err, ErrLegalRequestNotFound) {
			t.Fatalf("no such request: %v", err)
		}
	})
}

// The production clock lists a request past its due time until it closes, and no other.
func TestListOverdueLegalRequests(t *testing.T) {
	requireLegalHoldTables(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		Testing_CreateNetwork(ctx, networkId, "legal-overdue-network", server.NewId())
		now := server.NowUtc()
		order, _, err := FileLegalRequest(ctx, &LegalRequestArgs{
			RequestId: server.NewId(), Instrument: LegalInstrumentEuProductionOrder, Country: "NL", NetworkIds: []server.Id{networkId}, Emergency: true,
		}, now, DefaultLegalSettings())
		if err != nil {
			t.Fatal(err)
		}
		testFileLegalRequest(t, ctx, LegalInstrumentSubpoena, false, networkId)
		if overdue := ListOverdueLegalRequests(ctx, now.Add(7*time.Hour), 10); len(overdue) != 0 {
			t.Fatalf("overdue before its 8 h: %d", len(overdue))
		}
		overdue := ListOverdueLegalRequests(ctx, now.Add(9*time.Hour), 10)
		if len(overdue) != 1 || overdue[0].RequestId != order.RequestId {
			t.Fatalf("overdue past its 8 h: %+v", overdue)
		}
		if _, err := CloseLegalRequest(ctx, order.RequestId, now.Add(10*time.Hour)); err != nil {
			t.Fatal(err)
		}
		if overdue := ListOverdueLegalRequests(ctx, now.Add(11*time.Hour), 10); len(overdue) != 0 {
			t.Fatalf("a closed request overdue: %d", len(overdue))
		}
	})
}
