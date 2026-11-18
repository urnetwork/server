package work

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/smtpedgeservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// §21.5's breaker job (abuse_breaker_work.go; ABUSE-M9-02) on MG5's tables in
// a build with sidecar_pending: the machine's answer written as abuse_action
// rows with no network, read back as the state the send gate takes, cleared by
// the machine or by a person as its level says, mailed, and re-applied by
// §19.10's restore. The samples' clock is a fake one. Addresses are RFC 5737's.

// The effects it was asked for and the records it mailed.
type testBreakerActionDeps struct {
	stateLock sync.Mutex
	effects   []*controller.AbuseEffect
	records   []*controller.AbuseRecord
}

func (self *testBreakerActionDeps) ApplyAbuseEffect(ctx context.Context, effect *controller.AbuseEffect) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.effects = append(self.effects, effect)
	return nil
}

func (self *testBreakerActionDeps) MailAbuseRecord(ctx context.Context, record *controller.AbuseRecord) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	kept := *record
	self.records = append(self.records, &kept)
	return nil
}

func (self *testBreakerActionDeps) mailed() []*controller.AbuseRecord {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.records)
}

func (self *testBreakerActionDeps) effectCount() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return len(self.effects)
}

// Installs a recording effects and mailer for the rest of the test.
func newTestBreakerActionDeps(t testing.TB) *testBreakerActionDeps {
	deps := &testBreakerActionDeps{}
	restoreEffects := controller.SetAbuseEffects(deps)
	restoreMailer := controller.SetAbuseRecordMailer(deps)
	t.Cleanup(func() {
		restoreEffects()
		restoreMailer()
	})
	return deps
}

// §21.5's thresholds as pro.yml gives them, with a clean count restarting at
// once: the rows' own times are the store's clock, not the samples'.
func testBreakerSettings() *model.MailBreakerSettings {
	settings := model.DefaultMailBreakerSettings()
	settings.RestrictRate = 1000
	settings.CommunityRate = 2000
	settings.AllRate = 3000
	settings.ClearRate = 800
	settings.ClearDays = 3
	settings.CleanRestartHoldoff = 0
	return settings
}

var (
	testBreakerStart   = time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	testBreakerAddress = netip.MustParseAddr("192.0.2.10")
)

// A sample with every measure known.
func testBreakerSample(postmaster int64, complaint int64, listed ...netip.Addr) *model.MailBreakerMeasures {
	return &model.MailBreakerMeasures{
		PostmasterRates: []model.MailRate{model.KnownMailRate(postmaster), model.KnownMailRate(postmaster)},
		ComplaintRate:   model.KnownMailRate(complaint),
		ListingKnown:    true,
		ListedAddresses: listed,
	}
}

func testBreakerPass(t testing.TB, ctx context.Context, measures *model.MailBreakerMeasures, now time.Time, settings *model.MailBreakerSettings) *MailBreakerPassResult {
	t.Helper()
	result, err := RunMailBreakerPass(ctx, measures, now, settings)
	if err != nil {
		t.Fatal(err)
	}
	if result.Held {
		t.Fatal("the pass could not read the breaker rows")
	}
	return result
}

func testBreakerState(t testing.TB, ctx context.Context) *model.MailBreakerState {
	t.Helper()
	state, ok := model.GetMailBreakerState(ctx)
	if !ok {
		t.Fatal("the breaker state is not loaded")
	}
	return state
}

func testBreakerCount(t testing.TB, ctx context.Context, query string) (count int) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, query).Scan(&count))
	})
	return
}

func skipWithoutBreakerTables(t *testing.T) {
	t.Helper()
	if !server.SidecarMg5Linked() {
		t.Skip("MG5 is pending: these cases run with -tags sidecar_pending")
	}
}

// A trip writes one row with no network, mailed like any action, and the state
// the send gate reads follows; three clean days later the machine lifts it. No
// network gets a row, a strike or a notice from a breaker.
func TestMailBreakerPassTripsAndClearsLevelOne(t *testing.T) {
	skipWithoutBreakerTables(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		deps := newTestBreakerActionDeps(t)
		settings := testBreakerSettings()
		now := testBreakerStart

		if state := testBreakerState(t, ctx); state.Level != model.MailBreakerLevelNone || len(state.ListedAddresses) != 0 {
			t.Fatalf("with no rows: %+v", state)
		}
		// a near miss writes nothing
		result := testBreakerPass(t, ctx, testBreakerSample(0, 999), now, settings)
		if result.AppliedCount != 0 || testBreakerCount(t, ctx, `SELECT COUNT(*) FROM abuse_action`) != 0 {
			t.Fatalf("a near miss: %+v", result)
		}

		result = testBreakerPass(t, ctx, testBreakerSample(0, 1000), now, settings)
		if result.PreviousLevel != model.MailBreakerLevelNone || result.Level != model.MailBreakerLevelRestrict || result.AppliedCount != 1 {
			t.Fatalf("the trip: %+v", result)
		}
		if state := testBreakerState(t, ctx); state.Level != model.MailBreakerLevelRestrict {
			t.Fatalf("after the trip: %+v", state)
		}
		records := deps.mailed()
		if len(records) != 1 || records[0].Kind != model.AbuseActionKindMailBreaker || records[0].NetworkId != nil ||
			records[0].Target != model.MailBreakerLevelTarget(model.MailBreakerLevelRestrict) {
			t.Fatalf("mailed %+v", records)
		}
		// the row is its whole effect: no owner's hook is asked
		if count := deps.effectCount(); count != 0 {
			t.Errorf("a breaker row asked for %d effects", count)
		}
		if count := testBreakerCount(t, ctx, `SELECT COUNT(*) FROM abuse_action WHERE network_id IS NOT NULL OR applied_time IS NULL OR mailed_time IS NULL`); count != 0 {
			t.Errorf("%d breaker rows name a network or are owed", count)
		}
		if count := testBreakerCount(t, ctx, `SELECT COUNT(*) FROM abuse_case WHERE network_id IS NOT NULL OR join_kind <> 'breaker'`); count != 0 {
			t.Errorf("%d cases that are no breaker's", count)
		}
		if count := testBreakerCount(t, ctx, `SELECT COUNT(*) FROM network_abuse_state`); count != 0 {
			t.Errorf("a breaker wrote %d network standing rows", count)
		}
		if count := testBreakerCount(t, ctx, `SELECT COUNT(*) FROM account_message_outbox WHERE template_name = 'abuse_action_notice'`); count != 0 {
			t.Errorf("a breaker owed %d notices", count)
		}

		// the same sample again writes nothing more
		now = now.Add(5 * time.Minute)
		result = testBreakerPass(t, ctx, testBreakerSample(0, 1000), now, settings)
		if result.AppliedCount != 0 || result.LiftedCount != 0 {
			t.Fatalf("a second pass at the same rate: %+v", result)
		}
		// under the trigger and over the clear rate: held, no count
		now = now.Add(time.Hour)
		result = testBreakerPass(t, ctx, testBreakerSample(0, 900), now, settings)
		if result.AppliedCount != 0 || result.LiftedCount != 0 || result.Level != model.MailBreakerLevelRestrict {
			t.Fatalf("at 0.09%%: %+v", result)
		}

		// a clean sample starts the count, as a row
		now = now.Add(time.Hour)
		cleanStart := now
		result = testBreakerPass(t, ctx, testBreakerSample(0, 0), now, settings)
		if result.AppliedCount != 1 {
			t.Fatalf("a clean sample: %+v", result)
		}
		if state := testBreakerState(t, ctx); !state.CleanSinceTime.Equal(cleanStart) || state.Level != model.MailBreakerLevelRestrict {
			t.Fatalf("the count: %+v", state)
		}
		// two days in, a sample at 0.09% restarts it
		now = cleanStart.Add(2 * 24 * time.Hour)
		result = testBreakerPass(t, ctx, testBreakerSample(0, 900), now, settings)
		if result.LiftedCount != 1 {
			t.Fatalf("a sample at 0.09%% two days in: %+v", result)
		}
		if state := testBreakerState(t, ctx); !state.CleanSinceTime.IsZero() || state.Level != model.MailBreakerLevelRestrict {
			t.Fatalf("after the restart: %+v", state)
		}
		now = now.Add(time.Hour)
		cleanStart = now
		testBreakerPass(t, ctx, testBreakerSample(0, 0), now, settings)
		// three days from the first count would be now: the restarted count holds
		now = cleanStart.Add(3*24*time.Hour - time.Minute)
		result = testBreakerPass(t, ctx, testBreakerSample(0, 0), now, settings)
		if result.Level != model.MailBreakerLevelRestrict || result.LiftedCount != 0 {
			t.Fatalf("a minute short of three clean days: %+v", result)
		}
		now = cleanStart.Add(3 * 24 * time.Hour)
		result = testBreakerPass(t, ctx, testBreakerSample(0, 0), now, settings)
		if result.Level != model.MailBreakerLevelNone || result.LiftedCount != 2 {
			t.Fatalf("three clean days: %+v", result)
		}
		if state := testBreakerState(t, ctx); state.Level != model.MailBreakerLevelNone || !state.CleanSinceTime.IsZero() {
			t.Fatalf("cleared: %+v", state)
		}
		// every row and every lift was mailed
		if owed := testBreakerCount(t, ctx, `SELECT COUNT(*) FROM abuse_action WHERE mailed_time IS NULL`); owed != 0 {
			t.Errorf("%d rows unmailed", owed)
		}
		if rows, mailed := testBreakerCount(t, ctx, `SELECT COUNT(*) FROM abuse_action`), len(deps.mailed()); rows != mailed {
			t.Errorf("%d rows and %d records mailed", rows, mailed)
		}
	})
}

// A listed sending address trips level 2 and leaves the sending set by its
// own row; clean rates start no count while it is listed; the delisting lifts
// its row, and three clean days after that clear the level.
func TestMailBreakerPassListsAnAddressThenNeedsDelistingAndCleanDays(t *testing.T) {
	skipWithoutBreakerTables(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		newTestBreakerActionDeps(t)
		settings := testBreakerSettings()
		now := testBreakerStart

		result := testBreakerPass(t, ctx, testBreakerSample(0, 0, testBreakerAddress), now, settings)
		if result.Level != model.MailBreakerLevelCommunity || result.AppliedCount != 2 || result.ListedCount != 1 {
			t.Fatalf("the listing: %+v", result)
		}
		state := testBreakerState(t, ctx)
		if state.Level != model.MailBreakerLevelCommunity || !slices.Equal(state.ListedAddresses, []netip.Addr{testBreakerAddress}) {
			t.Fatalf("after the listing: %+v", state)
		}
		// ten days listed with clean rates: nothing counts
		now = now.Add(10 * 24 * time.Hour)
		result = testBreakerPass(t, ctx, testBreakerSample(0, 0, testBreakerAddress), now, settings)
		if result.AppliedCount != 0 || result.LiftedCount != 0 {
			t.Fatalf("ten days listed: %+v", result)
		}
		// the listing source down: the address stays out
		now = now.Add(time.Hour)
		result = testBreakerPass(t, ctx, &model.MailBreakerMeasures{
			PostmasterRates: []model.MailRate{model.KnownMailRate(0)},
			ComplaintRate:   model.KnownMailRate(0),
		}, now, settings)
		if result.AppliedCount != 0 || result.LiftedCount != 0 || result.ListedCount != 1 {
			t.Fatalf("an unknown listing: %+v", result)
		}

		// delisted: the address returns, and the count starts
		now = now.Add(time.Hour)
		delisted := now
		result = testBreakerPass(t, ctx, testBreakerSample(0, 0), now, settings)
		if result.LiftedCount != 1 || result.AppliedCount != 1 || result.ListedCount != 0 || result.Level != model.MailBreakerLevelCommunity {
			t.Fatalf("the delisting: %+v", result)
		}
		state = testBreakerState(t, ctx)
		if len(state.ListedAddresses) != 0 || !state.CleanSinceTime.Equal(delisted) || state.Level != model.MailBreakerLevelCommunity {
			t.Fatalf("after the delisting: %+v", state)
		}
		now = delisted.Add(3*24*time.Hour - time.Minute)
		if result = testBreakerPass(t, ctx, testBreakerSample(0, 0), now, settings); result.Level != model.MailBreakerLevelCommunity {
			t.Fatalf("short of three clean days: %+v", result)
		}
		now = delisted.Add(3 * 24 * time.Hour)
		if result = testBreakerPass(t, ctx, testBreakerSample(0, 0), now, settings); result.Level != model.MailBreakerLevelNone {
			t.Fatalf("three clean days after the delisting: %+v", result)
		}
		if state = testBreakerState(t, ctx); state.Level != model.MailBreakerLevelNone {
			t.Fatalf("cleared: %+v", state)
		}
	})
}

// Level 3 clears only by a person: no clean stretch lifts its row, and a
// person's lift through §21's lift path does. A missing measure holds any
// level, however long it is missing.
func TestMailBreakerLevelThreeClearsOnlyByAPerson(t *testing.T) {
	skipWithoutBreakerTables(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		newTestBreakerActionDeps(t)
		settings := testBreakerSettings()
		now := testBreakerStart

		result := testBreakerPass(t, ctx, testBreakerSample(3000, 0), now, settings)
		if result.Level != model.MailBreakerLevelAll || result.AppliedCount != 1 {
			t.Fatalf("the trip: %+v", result)
		}
		for range 12 {
			now = now.Add(24 * time.Hour)
			result = testBreakerPass(t, ctx, testBreakerSample(0, 0), now, settings)
			if result.Level != model.MailBreakerLevelAll || result.AppliedCount != 0 || result.LiftedCount != 0 {
				t.Fatalf("clean samples to %s: %+v", now, result)
			}
		}
		// a sample with nothing known, for a month
		for range 30 {
			now = now.Add(24 * time.Hour)
			result = testBreakerPass(t, ctx, &model.MailBreakerMeasures{}, now, settings)
			if result.Level != model.MailBreakerLevelAll || result.AppliedCount != 0 || result.LiftedCount != 0 {
				t.Fatalf("unknown samples to %s: %+v", now, result)
			}
		}

		rows, ok := model.GetMailBreakerRows(ctx, settings)
		if !ok || len(rows.LevelActions) != 1 {
			t.Fatalf("the standing rows: %+v, %t", rows, ok)
		}
		level := rows.LevelActions[0]
		if !level.ByMachine || level.ReviewState != model.AbuseReviewStatePending {
			t.Errorf("the level's row is %+v", level)
		}
		reviewerId := server.NewId()
		if _, err := controller.LiftAbuseAction(ctx, &model.AbuseLiftArgs{
			ActionId:       server.NewId(),
			CaseId:         level.CaseId,
			LiftedActionId: level.ActionId,
			ReviewState:    model.AbuseReviewStateLifted,
			ReviewerId:     &reviewerId,
			ReviewReason:   "the complaint source was a single misfiled loop",
		}); err != nil {
			t.Fatal(err)
		}
		if state := testBreakerState(t, ctx); state.Level != model.MailBreakerLevelNone {
			t.Fatalf("after the person's lift: %+v", state)
		}
		// and the machine trips it again if the measure still says so
		now = now.Add(time.Hour)
		if result = testBreakerPass(t, ctx, testBreakerSample(3000, 0), now, settings); result.Level != model.MailBreakerLevelAll {
			t.Fatalf("a sample still at 0.30%%: %+v", result)
		}
	})
}

// A breaker row is refused anywhere but a breaker's case, and with a target
// that is no level, address or clean time. A standing row this build cannot
// read leaves the state not loaded, which the send gate answers TRY_LATER,
// and a pass holding.
func TestMailBreakerRowsAreRefusedOrUnknownNeverGuessed(t *testing.T) {
	skipWithoutBreakerTables(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		newTestBreakerActionDeps(t)
		settings := testBreakerSettings()
		abuseSettings := model.DefaultAbuseSettings()
		now := testBreakerStart

		networkId := server.NewId()
		var networkCaseId, breakerCaseId server.Id
		var err error
		server.Tx(ctx, func(tx server.PgTx) {
			networkCaseId, _, err = model.OpenAbuseCaseInTx(ctx, tx, &model.AbuseCaseArgs{
				NetworkId:     &networkId,
				Category:      model.AbuseCategorySpam,
				Target:        model.AbuseTarget{Kind: model.AbuseTargetKindHost, Name: "shop.root.example"},
				ReporterClass: model.AbuseReporterClassWebRisk,
				JoinKind:      model.AbuseJoinKindHost,
				JoinKey:       "shop.root.example",
			}, now, abuseSettings)
			if err == nil {
				breakerCaseId, err = model.EnsureMailBreakerCaseInTx(ctx, tx, now, abuseSettings)
			}
		})
		if err != nil {
			t.Fatal(err)
		}
		// the breaker case is one case, found again
		server.Tx(ctx, func(tx server.PgTx) {
			again, err := model.EnsureMailBreakerCaseInTx(ctx, tx, now, abuseSettings)
			if err != nil || again != breakerCaseId {
				t.Errorf("the breaker case: %s, %v, want %s", again, err, breakerCaseId)
			}
		})
		refused := []*model.AbuseActionArgs{
			{ActionId: server.NewId(), CaseId: networkCaseId, Kind: model.AbuseActionKindMailBreaker, Target: "level:1"},
			{ActionId: server.NewId(), CaseId: breakerCaseId, Kind: model.AbuseActionKindMailBreaker, Target: "level:0"},
			{ActionId: server.NewId(), CaseId: breakerCaseId, Kind: model.AbuseActionKindMailBreaker, Target: "level:4"},
			{ActionId: server.NewId(), CaseId: breakerCaseId, Kind: model.AbuseActionKindMailBreaker, Target: "shop.root.example"},
			{ActionId: server.NewId(), CaseId: breakerCaseId, Kind: model.AbuseActionKindMailBreaker, Target: "address:not-an-address"},
			{ActionId: server.NewId(), CaseId: breakerCaseId, Kind: model.AbuseActionKindMailBreaker, Target: "clean:yesterday"},
		}
		// and no kill or halt hangs off a breaker's case, which names no network
		refused = append(refused, &model.AbuseActionArgs{ActionId: server.NewId(), CaseId: breakerCaseId, Kind: model.AbuseActionKindNoticeForward, Target: "mail-breaker"})
		for _, args := range refused {
			if _, err := controller.ApplyAbuseAction(ctx, args); !errors.Is(err, model.ErrAbuseActionInvalid) {
				t.Errorf("%s on case %s: %v", args.Target, args.CaseId, err)
			}
		}
		if count := testBreakerCount(t, ctx, `SELECT COUNT(*) FROM abuse_action`); count != 0 {
			t.Fatalf("%d rows from refused actions", count)
		}

		testBreakerPass(t, ctx, testBreakerSample(0, 1000), now, settings)
		// a row a later build wrote, which this one cannot read
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `
				INSERT INTO abuse_action (action_id, case_id, category, kind, target, by_machine, review_state, create_time)
				VALUES ($1, $2, 'spam', 'mail_breaker', 'level:9', true, 'pending', $3)
			`, server.NewId(), breakerCaseId, server.NowUtc()))
		})
		if state, ok := model.GetMailBreakerState(ctx); ok || state != nil {
			t.Fatalf("an unreadable row read as %+v", state)
		}
		result, err := RunMailBreakerPass(ctx, testBreakerSample(0, 0), now.Add(30*24*time.Hour), settings)
		if err != nil || !result.Held || result.AppliedCount != 0 || result.LiftedCount != 0 {
			t.Fatalf("a pass over an unreadable row: %+v, %v", result, err)
		}
	})
}

// The breakers' rows are mailed like any action, and §19.10's restore
// re-applies them from the mailbox: the same level, the same listed address
// and the same clean count on a store that lost them, with a lifted row
// staying lifted.
func TestMailBreakerRecordsAreRestored(t *testing.T) {
	skipWithoutBreakerTables(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		deps := newTestBreakerActionDeps(t)
		settings := testBreakerSettings()
		dumpTime := server.NowUtc().Add(-time.Hour)
		now := testBreakerStart

		// level 1, then level 2 with two listed addresses, then one of them delisted
		testBreakerPass(t, ctx, testBreakerSample(0, 1000), now, settings)
		now = now.Add(time.Hour)
		testBreakerPass(t, ctx, testBreakerSample(0, 0, testBreakerAddress, netip.MustParseAddr("192.0.2.11")), now, settings)
		now = now.Add(time.Hour)
		testBreakerPass(t, ctx, testBreakerSample(0, 0, testBreakerAddress), now, settings)
		before := *testBreakerState(t, ctx)
		if before.Level != model.MailBreakerLevelCommunity || !slices.Equal(before.ListedAddresses, []netip.Addr{testBreakerAddress}) {
			t.Fatalf("before the loss: %+v", before)
		}
		beforeRows, _ := model.GetMailBreakerRows(ctx, settings)

		// the site loss: the rows after the dump are gone, the mailbox holds every record
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM abuse_action`))
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM abuse_case`))
		})
		if state := testBreakerState(t, ctx); state.Level != model.MailBreakerLevelNone {
			t.Fatalf("after the loss: %+v", state)
		}
		mailbox := &controller.AbuseMailboxRecords{}
		for i, record := range deps.mailed() {
			mailbox.Records = append(mailbox.Records, &controller.AbuseRestoreRecord{Index: i, Record: record})
		}
		restoreNow := server.NowUtc()
		plan := controller.PlanAbuseRestore(mailbox, dumpTime, restoreNow)
		if len(plan.Items) != len(mailbox.Records) || len(plan.Items) != 5 {
			t.Fatalf("the plan re-applies %d of %d records", len(plan.Items), len(mailbox.Records))
		}
		restored, err := controller.ApplyAbuseRestorePlan(ctx, plan, true, io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		for i, outcome := range restored.Outcomes {
			if outcome != controller.AbuseRestoreOutcomeApplied {
				t.Errorf("record %d: %s", i, outcome)
			}
		}
		after := testBreakerState(t, ctx)
		if after.Level != before.Level || !slices.Equal(after.ListedAddresses, before.ListedAddresses) || !after.CleanSinceTime.Equal(before.CleanSinceTime) {
			t.Fatalf("restored %+v, want %+v", after, before)
		}
		afterRows, _ := model.GetMailBreakerRows(ctx, settings)
		if len(afterRows.LevelActions) != len(beforeRows.LevelActions) || len(afterRows.AddressActions) != len(beforeRows.AddressActions) {
			t.Errorf("restored %d level rows and %d address rows, want %d and %d",
				len(afterRows.LevelActions), len(afterRows.AddressActions), len(beforeRows.LevelActions), len(beforeRows.AddressActions))
		}
		for i, action := range afterRows.LevelActions {
			if action.ActionId != beforeRows.LevelActions[i].ActionId {
				t.Errorf("level row %d restored under another id", i)
			}
		}
		// a second run changes nothing
		again, err := controller.ApplyAbuseRestorePlan(ctx, plan, true, io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		for i, outcome := range again.Outcomes {
			if outcome != controller.AbuseRestoreOutcomeAlready {
				t.Errorf("record %d on a second run: %s", i, outcome)
			}
		}
		// and the job goes on from the restored rows: the second address stays delisted
		result := testBreakerPass(t, ctx, testBreakerSample(0, 0, testBreakerAddress), now.Add(time.Hour), settings)
		if result.AppliedCount != 0 || result.LiftedCount != 0 || result.Level != model.MailBreakerLevelCommunity {
			t.Fatalf("the pass after the restore: %+v", result)
		}
	})
}

// The job is scheduled a pass interval ahead, once, in a binary that links
// MG5 and MG9; with no outside sources it samples the store's complaint rate
// alone, unknown with nothing sent, and changes nothing.
func TestAbuseBreakersIsScheduledAndHoldsWithNoMeasurer(t *testing.T) {
	skipWithoutBreakerTables(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		newTestBreakerActionDeps(t)
		clientSession := session.NewLocalClientSession(ctx, "", nil)
		defer clientSession.Cancel()
		before := server.NowUtc()
		server.Tx(ctx, func(tx server.PgTx) {
			ScheduleAbuseBreakers(clientSession, tx)
			ScheduleAbuseBreakers(clientSession, tx)
		})
		runAt, found := testAbusePendingRunAt(t, ctx, "abuse_breakers")
		if found != server.SidecarMg9Linked() {
			t.Fatalf("scheduled %t, and MG9 is linked %t", found, server.SidecarMg9Linked())
		}
		if found && runAt.Before(before.Add(model.DefaultMailBreakerSettings().PassInterval-time.Second)) {
			t.Fatalf("the pass runs at %s", runAt)
		}

		result, err := AbuseBreakers(&AbuseBreakersArgs{}, clientSession)
		if err != nil || result.Held || result.Level != 0 || result.AppliedCount != 0 || result.LiftedCount != 0 {
			t.Fatalf("with no outside sources and nothing sent: %+v, %v", result, err)
		}
		// a measurer's sample is what the job applies, under pro.yml's thresholds
		defer SetMailBreakerMeasurer(func(ctx context.Context, now time.Time, settings *model.MailBreakerSettings) *model.MailBreakerMeasures {
			return &model.MailBreakerMeasures{ListedAddresses: []netip.Addr{testBreakerAddress}}
		})()
		result, err = AbuseBreakers(&AbuseBreakersArgs{}, clientSession)
		if err != nil {
			t.Fatal(err)
		}
		if server.SidecarMg9Linked() {
			if result.Level != int(model.MailBreakerLevelCommunity) || result.ListedCount != 1 {
				t.Fatalf("a listed address: %+v", result)
			}
		} else if result.AppliedCount != 0 {
			t.Fatalf("a binary without MG9 ran the breakers: %+v", result)
		}
	})
}

// A send record of networkId's from 192.0.2.10 with recipientCount proven
// recipients, starting at start.
func testBreakerSend(networkId sidecar.Id, start time.Time, recipientCount uint32) *smtpedgeservice.SmtpSendRecord {
	sendId := make([]byte, smtpedgeservice.SmtpSendIdByteCount)
	if _, err := rand.Read(sendId); err != nil {
		panic(err)
	}
	return &smtpedgeservice.SmtpSendRecord{
		SendId:           sendId,
		NetworkId:        networkId,
		ClientId:         sidecar.NewId(),
		SendingAddress:   testBreakerAddress,
		RecipientDomain:  "recipient.example",
		MxHost:           "mx.recipient.example",
		StartTime:        start,
		EndTime:          start.Add(time.Minute),
		ByteCount:        4096,
		MessageIdHash:    smtpedgeservice.SmtpMessageIdHash("<" + server.NewId().String() + "@quietnet.root.example>"),
		XUrRefKeyId:      1,
		RecipientCount:   recipientCount,
		MessageByteCount: 2048,
		TlsVersion:       0x0304,
		TlsLevel:         smtpedgeservice.SmtpTlsLevelDane,
		Result:           "sent",
	}
}

// The job's own measurer (ABUSE-M9-03's measures): with no outside sources it
// samples the store's complaint rate, so complaints at 0.10% of the day's
// recipients trip level 1, and since Postmaster Tools, SNDS and the listing
// stay unknown, clean days of complaints clear nothing by machine. Sources a
// process installs join the sample.
func TestMailBreakerPassSamplesTheStoresComplaintRate(t *testing.T) {
	skipWithoutBreakerTables(t)
	if !server.SidecarMg9Linked() {
		t.Skip("MG9 is pending: these cases run with -tags sidecar_pending")
	}
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		newTestBreakerActionDeps(t)
		settings := testBreakerSettings()
		now := server.NowUtc()

		// 1,000 recipients across two networks, and one complaint at high confidence
		networkId := sidecar.NewId()
		sends := []*smtpedgeservice.SmtpSendRecord{
			testBreakerSend(networkId, now.Add(-2*time.Hour), 600),
			testBreakerSend(sidecar.NewId(), now.Add(-time.Hour), 400),
		}
		if result, err := model.InsertSmtpSends(ctx, sends); err != nil || result.StoredCount != len(sends) {
			t.Fatalf("the sends: %+v, %v", result, err)
		}
		measures := currentMailBreakerMeasurer()(ctx, now, settings)
		if measures.ComplaintRate != model.KnownMailRate(0) || len(measures.PostmasterRates) != 0 || measures.ListingKnown {
			t.Fatalf("with no complaint: %+v", measures)
		}
		if err := model.InsertSmtpComplaint(ctx, &smtpedgeservice.SmtpComplaint{
			ComplaintId:   sidecar.NewId(),
			NetworkId:     networkId,
			Reporter:      "reporter.example",
			Type:          smtpedgeservice.SmtpComplaintTypeAbuse,
			ArrivalTime:   now.Add(-30 * time.Minute),
			Domain:        "quietnet.root.example",
			MessageIdHash: sends[0].MessageIdHash,
			JoinResult:    string(smtpedgeservice.SmtpConfidenceHigh),
		}); err != nil {
			t.Fatal(err)
		}
		measures = currentMailBreakerMeasurer()(ctx, now, settings)
		if measures.ComplaintRate != model.KnownMailRate(1000) {
			t.Fatalf("one complaint over 1,000 recipients: %+v", measures)
		}
		result := testBreakerPass(t, ctx, measures, now, settings)
		if result.Level != model.MailBreakerLevelRestrict || result.AppliedCount != 1 {
			t.Fatalf("the trip on the store's rate: %+v", result)
		}

		// a week on, nothing complained of and nothing sent: the rate is unknown, and the level holds
		later := now.Add(7 * 24 * time.Hour)
		measures = currentMailBreakerMeasurer()(ctx, later, settings)
		if measures.ComplaintRate.Known {
			t.Fatalf("a week on: %+v", measures)
		}
		if result := testBreakerPass(t, ctx, measures, later, settings); result.Level != model.MailBreakerLevelRestrict || result.AppliedCount != 0 || result.LiftedCount != 0 {
			t.Fatalf("a week of unknown samples: %+v", result)
		}

		// installed sources join the store's rate: a listed sending address trips level 2
		listing := &testBreakerListing{listed: []netip.Addr{testBreakerAddress}}
		defer SetMailBreakerMeasurer(MailBreakerMeasurerOfSources(&model.MailBreakerSources{
			SendingAddresses: []netip.Addr{testBreakerAddress},
			Listing:          listing,
			Snds:             listing,
		}))()
		measures = currentMailBreakerMeasurer()(ctx, now, settings)
		if !measures.ListingKnown || !slices.Equal(measures.ListedAddresses, []netip.Addr{testBreakerAddress}) || measures.ComplaintRate != model.KnownMailRate(1000) {
			t.Fatalf("with the sources installed: %+v", measures)
		}
		if result := testBreakerPass(t, ctx, measures, now, settings); result.Level != model.MailBreakerLevelCommunity || result.ListedCount != 1 {
			t.Fatalf("a listing from the installed sources: %+v", result)
		}
	})
}

// A listing and SNDS source answering one fixed list.
type testBreakerListing struct {
	listed []netip.Addr
}

func (self *testBreakerListing) ListedAddresses(ctx context.Context, addresses []netip.Addr, now time.Time) ([]netip.Addr, bool) {
	return slices.Clone(self.listed), true
}

func (self *testBreakerListing) RedAddresses(ctx context.Context, addresses []netip.Addr) ([]netip.Addr, bool) {
	return []netip.Addr{}, true
}

// QD-1069's install from a taskworker's files: with none, every outside source is missing and each is said, so a
// sample reads the store's complaint rate alone; with smtpedge.yml, both credentials and the feed mirror's copy,
// every source is built, over <root> and the shared sending addresses alone. Nothing here asks a built source
// anything, since the clients over the vault's credentials would reach their services.
func TestMailBreakerSourcesOfFiles(t *testing.T) {
	sources, missing := mailBreakerSourcesOfFiles(&MailBreakerSourcesArgs{})
	if sources.Root != "" || len(sources.SendingAddresses) != 0 || sources.Postmaster != nil || sources.Snds != nil || sources.Listing != nil || len(missing) != 4 {
		t.Fatalf("with no files: %+v, missing %q", sources, missing)
	}

	defer server.Config.PushSimpleResource("smtpedge.yml", []byte(
		"root: root.example\nreport_host: rpt.report.example\nhelo_domain: out.report.example\n"+
			"sending_addresses:\n  - {address: 192.0.2.10, helo: o1.out.report.example}\n  - {address: 192.0.2.11, helo: o2.out.report.example}\n",
	))()
	defer server.Vault.PushSimpleResource(server.PostmasterToolsCredentialFileName, []byte(
		"client_id: test-client\nclient_secret: test-secret-value\nrefresh_token: test-refresh-value\n",
	))()
	defer server.Vault.PushSimpleResource(server.SndsCredentialFileName, []byte("key: test-key-value\n"))()
	sources, missing = mailBreakerSourcesOfFiles(&MailBreakerSourcesArgs{FeedDirectory: "../../testdata/mail_measures/mirror"})
	if len(missing) != 0 {
		t.Fatalf("with every file: missing %q", missing)
	}
	if sources.Root != "root.example" || !slices.Equal(sources.SendingAddresses, []netip.Addr{netip.MustParseAddr("192.0.2.10"), netip.MustParseAddr("192.0.2.11")}) {
		t.Fatalf("the root and addresses: %q, %v", sources.Root, sources.SendingAddresses)
	}
	if sources.Postmaster == nil || sources.Snds == nil || sources.Listing == nil {
		t.Fatalf("a source was not built: %+v", sources)
	}
	// the listing over the mirror's copy reads the fixture's zones, current at its oldest file's time: 192.0.2.11 is
	// on SBL
	paths, err := filepath.Glob("../../testdata/mail_measures/mirror/*.rbldnsd")
	if err != nil || len(paths) == 0 {
		t.Fatalf("no mirror fixture: %v", err)
	}
	var copyTime time.Time
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if copyTime.IsZero() || info.ModTime().Before(copyTime) {
			copyTime = info.ModTime()
		}
	}
	listed, known := sources.Listing.ListedAddresses(context.Background(), sources.SendingAddresses, copyTime.Add(time.Minute))
	if !known || !slices.Equal(listed, []netip.Addr{netip.MustParseAddr("192.0.2.11")}) {
		t.Fatalf("the mirror's copy lists %v, known %t", listed, known)
	}

	// a credential that is not usable is said, its source left out, and the error holds no value
	defer server.Vault.PushSimpleResource(server.SndsCredentialFileName, []byte("key: \"\"\n"))()
	sources, missing = mailBreakerSourcesOfFiles(&MailBreakerSourcesArgs{FeedDirectory: "../../testdata/mail_measures/mirror"})
	if sources.Snds != nil || len(missing) != 1 || strings.Contains(missing[0], "test-key-value") {
		t.Fatalf("an empty key: %+v, missing %q", sources, missing)
	}
}
