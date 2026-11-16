package controller

import (
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/evsservice"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// §21.9's holds as api and the abuse worker run them (abuse_hold_controller.go; ABUSE-M5-14): the legal-request
// record in either build, and placement, renewal, the staff kinds, the record's task and the pass on MG5's tables
// over a FakeBlobs first cluster and a FakeTasks queue, in a build with sidecar_pending.

// Day 0 of the pass's schedules.
var testHoldDay0 = time.Date(2026, 3, 2, 12, 0, 0, 0, time.UTC)

func testHoldDay(day int) time.Time {
	return testHoldDay0.AddDate(0, 0, day)
}

// A legal-request record reads back as written, under its MAC; a changed field, another key, a missing MAC or
// a missing key does not.
func TestLegalHoldRecordRoundTripsUnderItsMac(t *testing.T) {
	record := &LegalHoldRecord{
		Event:      LegalHoldEventRenewed,
		CaseId:     server.NewId(),
		NetworkId:  server.NewId(),
		Kind:       sidecar.HoldKindPreserve2703f,
		Scope:      sidecar.HoldScopeRepos(sidecar.NewId(), sidecar.NewId()),
		PlaceTime:  testHoldDay0,
		ExpireTime: testHoldDay(180),
	}
	body, err := EncodeLegalHoldRecord(record, testAbuseRecordMacKey)
	if err != nil {
		t.Fatal(err)
	}
	keys := []string{}
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.Token()
	for decoder.More() {
		key, _ := decoder.Token()
		keys = append(keys, key.(string))
		var value json.RawMessage
		decoder.Decode(&value)
	}
	if !slices.Equal(keys, legalHoldRecordKeys) {
		t.Fatalf("keys %v", keys)
	}
	parsed, err := ParseLegalHoldRecord(body, testAbuseRecordMacKey)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Event != record.Event || parsed.CaseId != record.CaseId || parsed.NetworkId != record.NetworkId || parsed.Kind != record.Kind ||
		!slices.Equal(parsed.Scope.RepoIds, record.Scope.RepoIds) || !parsed.PlaceTime.Equal(record.PlaceTime) || !parsed.ExpireTime.Equal(record.ExpireTime) {
		t.Fatalf("read back %+v", parsed)
	}

	network := *record
	network.Scope = sidecar.HoldScopeNetwork()
	networkBody, _ := EncodeLegalHoldRecord(&network, testAbuseRecordMacKey)
	if !strings.Contains(string(networkBody), `"scope":"network"`) {
		t.Fatalf("a network scope encodes as %s", networkBody)
	}
	if parsed, err := ParseLegalHoldRecord(networkBody, testAbuseRecordMacKey); err != nil || !parsed.Scope.Network() {
		t.Fatalf("a network scope: %+v, %v", parsed, err)
	}

	if _, err := ParseLegalHoldRecord(body, []byte("another key of thirty-two bytes!")); !errors.Is(err, ErrAbuseRecordMac) {
		t.Errorf("another key: %v", err)
	}
	changed := strings.Replace(string(body), `"renewed"`, `"placed"`, 1)
	if _, err := ParseLegalHoldRecord([]byte(changed), testAbuseRecordMacKey); !errors.Is(err, ErrAbuseRecordMac) {
		t.Errorf("a changed event: %v", err)
	}
	macAt := strings.Index(string(body), `,"mac":`)
	if _, err := ParseLegalHoldRecord([]byte(string(body[:macAt])+"}"), testAbuseRecordMacKey); !errors.Is(err, ErrAbuseRecordMac) {
		t.Errorf("no mac: %v", err)
	}
	if _, err := ParseLegalHoldRecord([]byte(strings.Replace(string(body), `{"event"`, `{"extra":1,"event"`, 1)), testAbuseRecordMacKey); !errors.Is(err, ErrLegalHoldRecordInvalid) {
		t.Errorf("another key: %v", err)
	}
	if _, err := ParseLegalHoldRecord([]byte(strings.Replace(string(body), `"kind":"preserve_2703f",`, ``, 1)), testAbuseRecordMacKey); !errors.Is(err, ErrLegalHoldRecordInvalid) {
		t.Errorf("a missing key: %v", err)
	}
	if _, err := EncodeLegalHoldRecord(record, nil); !errors.Is(err, ErrAbuseRecordMacKey) {
		t.Errorf("no mac key: %v", err)
	}
}

// Every record it mailed. err, when set, fails each.
type testLegalHoldRecordMailer struct {
	stateLock sync.Mutex
	records   []*LegalHoldRecord
	err       error
}

func (self *testLegalHoldRecordMailer) MailLegalHoldRecord(ctx context.Context, record *LegalHoldRecord) error {
	if _, err := EncodeLegalHoldRecord(record, testAbuseRecordMacKey); err != nil {
		return err
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.err != nil {
		return self.err
	}
	self.records = append(self.records, record)
	return nil
}

func (self *testLegalHoldRecordMailer) mailed() []*LegalHoldRecord {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]*LegalHoldRecord{}, self.records...)
}

// What a hold test runs on: a first cluster and a task queue in memory on a fake clock, the hold/ writer installed,
// holds turned on, a recording legal-request mailer and the clock placement reads.
type testHoldWorld struct {
	clock  *sidecartest.FakeClock
	blobs  *sidecartest.FakeBlobs
	tasks  *sidecartest.FakeTasks
	store  *model.AbuseHoldStore
	mailer *testLegalHoldRecordMailer
}

func newTestHoldWorld(t testing.TB) *testHoldWorld {
	t.Helper()
	clock := sidecartest.NewFakeClock(testHoldDay0)
	blobs := sidecartest.NewFakeBlobs(clock, nil, nil, nil)
	first, err := blobs.FirstCluster()
	if err != nil {
		t.Fatal(err)
	}
	world := &testHoldWorld{
		clock:  clock,
		blobs:  blobs,
		tasks:  sidecartest.NewFakeTasks(clock, blobs.Faults(), nil),
		store:  model.NewAbuseHoldStore(first),
		mailer: &testLegalHoldRecordMailer{},
	}
	model.SetAbuseHoldStore(world.store)
	popConfig := server.Testing_PushAbuseConfig("holds_enabled: true\nprotected_names: {roots: [root.example], platform: [platform.example]}\n")
	restoreMailer := SetLegalHoldRecordMailer(world.mailer)
	restoreNow := Testing_SetLegalHoldNow(clock.Now)
	t.Cleanup(func() {
		model.SetAbuseHoldStore(nil)
		popConfig()
		restoreMailer()
		restoreNow()
	})
	return world
}

// The pass's deps on the world.
func (self *testHoldWorld) passDeps() *AbuseHoldPassDeps {
	return &AbuseHoldPassDeps{Store: self.store, Tasks: self.tasks, Now: self.clock.Now}
}

// The hold's object's bytes, nil when there is none.
func (self *testHoldWorld) object(t testing.TB, ctx context.Context, networkId server.Id, caseId server.Id) []byte {
	t.Helper()
	object, found, err := self.store.Read(ctx, networkId, caseId)
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		return nil
	}
	return object.Body
}

// The evs.sweep requests offered for a hold's end, and those still pending.
func (self *testHoldWorld) sweeps() (offered []*sidecar.TaskRequest, pending []*sidecar.TaskRequest) {
	for _, request := range self.tasks.AllRequests() {
		if request.Name == evsservice.EvsSweepTaskName {
			offered = append(offered, request)
		}
	}
	for _, request := range self.tasks.Pending() {
		if request.Name == evsservice.EvsSweepTaskName {
			pending = append(pending, request)
		}
	}
	return
}

// The owed record mails, as their tasks' args.
func testOwedLegalHoldRecords(t testing.TB, ctx context.Context) []*MailLegalHoldRecordArgs {
	t.Helper()
	owed := []*MailLegalHoldRecordArgs{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `
			SELECT args_json
			FROM pending_task
			WHERE function_name = 'github.com/urnetwork/server/controller.MailLegalHoldRecordTask'
			ORDER BY run_at, task_id
		`)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var argsJson string
				server.Raise(result.Scan(&argsJson))
				args := &MailLegalHoldRecordArgs{}
				server.Raise(json.Unmarshal([]byte(argsJson), args))
				owed = append(owed, args)
			}
		})
	})
	return owed
}

// Runs each owed record's task as a worker would, and forgets the rows.
func testRunOwedLegalHoldRecords(t testing.TB, ctx context.Context) (failed int) {
	t.Helper()
	for _, args := range testOwedLegalHoldRecords(t, ctx) {
		if _, err := MailLegalHoldRecordTask(args, session.NewLocalClientSession(ctx, "", nil)); err != nil {
			failed += 1
		}
	}
	if failed == 0 {
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM pending_task WHERE function_name = 'github.com/urnetwork/server/controller.MailLegalHoldRecordTask'`))
		})
	}
	return
}

// A network with an admin and a staff-filed case naming it.
func newTestHoldCase(t testing.TB, ctx context.Context) (networkId server.Id, caseId server.Id) {
	t.Helper()
	networkId = server.NewId()
	model.Testing_CreateNetwork(ctx, networkId, "hold-"+networkId.String()[24:], server.NewId())
	var err error
	server.Tx(ctx, func(tx server.PgTx) {
		caseId, _, err = model.OpenAbuseCaseInTx(ctx, tx, &model.AbuseCaseArgs{
			NetworkId:     &networkId,
			Category:      model.AbuseCategoryOther,
			Target:        model.AbuseTarget{Kind: model.AbuseTargetKindNetwork, Name: networkId.String()},
			ReporterClass: model.AbuseReporterClassLawEnforcement,
			JoinKind:      model.AbuseJoinKindNetwork,
			JoinKey:       networkId.String(),
		}, server.NowUtc(), model.DefaultAbuseSettings())
	})
	if err != nil {
		t.Fatal(err)
	}
	return
}

// What a network could learn of: its admin events, its owed account messages and its standing row.
func testHoldVisible(t testing.TB, ctx context.Context, networkId server.Id) (adminEvents int, messages int, standing int) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*) FROM network_admin_event WHERE network_id = $1`, networkId).Scan(&adminEvents))
		server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*) FROM account_message_outbox WHERE network_id = $1`, networkId).Scan(&messages))
		server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*) FROM network_abuse_state WHERE network_id = $1`, networkId).Scan(&standing))
	})
	return
}

// Placement owes its record in the row's transaction, so a crash between the row and the mail leaves it owed and
// its task mails it; the record carries the hold's terms, and a mailbox that is down leaves it owed for a retry.
func TestPlaceLegalHoldOwesItsRecordAcrossACrash(t *testing.T) {
	requireAbuseTables(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newTestHoldWorld(t)
		networkId, caseId := newTestHoldCase(t, ctx)
		placedCount := 0
		restorePlaced := Testing_SetLegalHoldPlacedHook(func(ctx context.Context, hold *model.LegalHold) {
			placedCount += 1
		})
		defer restorePlaced()

		model.Testing_LegalHoldStep = func(step string) {
			if step == model.LegalHoldStepRowAfter {
				runtime.Goexit()
			}
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			PlaceLegalHold(ctx, &model.LegalHoldArgs{NetworkId: networkId, CaseId: caseId, Kind: sidecar.HoldKindPreserve2703f})
		}()
		<-done
		model.Testing_LegalHoldStep = nil

		if placedCount != 0 || len(world.mailer.mailed()) != 0 {
			t.Fatalf("the crash ran the hook %d times and mailed %d", placedCount, len(world.mailer.mailed()))
		}
		hold := model.GetLegalHold(ctx, networkId, caseId)
		if hold == nil {
			t.Fatal("no row after the commit")
		}
		owed := testOwedLegalHoldRecords(t, ctx)
		if len(owed) != 1 || owed[0].Event != LegalHoldEventPlaced {
			t.Fatalf("owed %+v", owed)
		}

		world.mailer.err = ErrLegalHoldRecordMailerUnavailable
		if failed := testRunOwedLegalHoldRecords(t, ctx); failed != 1 || len(testOwedLegalHoldRecords(t, ctx)) != 1 {
			t.Fatal("a mailbox that is down took the record")
		}
		world.mailer.err = nil
		if failed := testRunOwedLegalHoldRecords(t, ctx); failed != 0 {
			t.Fatal("the retry failed")
		}
		mailed := world.mailer.mailed()
		if len(mailed) != 1 {
			t.Fatalf("mailed %d", len(mailed))
		}
		record := mailed[0]
		if record.Event != LegalHoldEventPlaced || record.NetworkId != networkId || record.CaseId != caseId || record.Kind != sidecar.HoldKindPreserve2703f ||
			!record.Scope.Network() || !record.PlaceTime.Equal(hold.PlaceTime) || !record.ExpireTime.Equal(hold.ExpireTime) {
			t.Fatalf("the record: %+v", record)
		}

		// the retry the staff member sends finishes what the crash cut off: the hook
		result, err := PlaceLegalHold(ctx, &model.LegalHoldArgs{NetworkId: networkId, CaseId: caseId, Kind: sidecar.HoldKindPreserve2703f})
		if err != nil || result.Inserted || placedCount != 1 || len(testOwedLegalHoldRecords(t, ctx)) != 0 {
			t.Fatalf("the retry: %+v, %v, hook %d", result, err, placedCount)
		}
	})
}

// Placing, renewing and ending a hold, sealed case or not, write no admin event, no account message and no
// standing row, and owe only the legal-request mailbox its records.
func TestLegalHoldsAreSilent(t *testing.T) {
	requireAbuseTables(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		for _, barred := range []bool{false, true} {
			world := newTestHoldWorld(t)
			networkId, caseId := newTestHoldCase(t, ctx)
			if barred {
				if err := model.BarAbuseCase(ctx, caseId); err != nil {
					t.Fatal(err)
				}
			}
			before := [3]int{}
			before[0], before[1], before[2] = testHoldVisible(t, ctx, networkId)
			if _, err := PlaceLegalHold(ctx, &model.LegalHoldArgs{NetworkId: networkId, CaseId: caseId, Kind: sidecar.HoldKindPreserve2703f}); err != nil {
				t.Fatal(err)
			}
			world.clock.AdvanceTo(testHoldDay(80))
			if _, err := RenewLegalHold(ctx, networkId, caseId); err != nil {
				t.Fatal(err)
			}
			world.clock.AdvanceTo(testHoldDay(180))
			if result := RunAbuseHoldPass(ctx, world.passDeps()); result.EndedCount != 1 {
				t.Fatalf("barred %t: the pass ended %+v", barred, result)
			}
			after := [3]int{}
			after[0], after[1], after[2] = testHoldVisible(t, ctx, networkId)
			if after != before {
				t.Fatalf("barred %t: a hold showed: %v, then %v", barred, before, after)
			}
			events := []LegalHoldEvent{}
			for _, args := range testOwedLegalHoldRecords(t, ctx) {
				if args.CaseId == caseId {
					events = append(events, args.Event)
				}
			}
			if !slices.Equal(events, []LegalHoldEvent{LegalHoldEventPlaced, LegalHoldEventRenewed, LegalHoldEventEnded}) {
				t.Fatalf("barred %t: owed %v", barred, events)
			}
		}
	})
}

// The hold files call nothing that tells a network anything: no hint, no admin event, no account message, no
// notice (owner decisions 93, 184). The scan's own fixture shows it catches one.
func TestLegalHoldFilesCallNothingThatNotifies(t *testing.T) {
	notifiers := []string{"SidecarHint", "AddAdminEventInTx", "AddAdminEvent", "addAccountMessagesInTx", "oweAbuseActionNoticeInTx", "SetNetworkAbuseSuspended"}
	scan := func(name string, file *ast.File) []string {
		found := []string{}
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			var called string
			switch fun := call.Fun.(type) {
			case *ast.SelectorExpr:
				called = fun.Sel.Name
			case *ast.Ident:
				called = fun.Name
			}
			if slices.Contains(notifiers, called) {
				found = append(found, name+": "+called)
			}
			return true
		})
		return found
	}
	found := []string{}
	for _, path := range []string{
		"abuse_hold_controller.go",
		filepath.Join("..", "model", "abuse_hold_model.go"),
		filepath.Join("..", "taskworker", "work", "abuse_hold_work.go"),
	} {
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		found = append(found, scan(filepath.Base(path), file)...)
	}
	if 0 < len(found) {
		t.Fatalf("a hold path notifies: %v", found)
	}
	fixture, err := parser.ParseFile(token.NewFileSet(), "fixture.go", `package controller
func f() { model.AddAdminEventInTx(nil, nil, nil); SidecarHint(nil, nil, nil) }
`, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	if caught := scan("fixture.go", fixture); len(caught) != 2 {
		t.Fatalf("the scan caught %v", caught)
	}
}

// FS-48 at T3's server half: a §2258A hold stands across days 180 to 364 while §2703(f) holds placed beside it end
// at 90 and, renewed at day 80, at 180; a second renewal and a §2258A renewal are refused; each hold's end queues
// its sweep pass before its object goes, and the network stays in HeldNetworkIds until its last hold ends, so the
// server sweeps resume only then.
func TestLegalHoldDeadlinesEndOnThePass(t *testing.T) {
	requireAbuseTables(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newTestHoldWorld(t)
		networkId, reportCase := newTestHoldCase(t, ctx)
		preserveCase := testHoldCaseOf(t, ctx, networkId)
		renewedCase := testHoldCaseOf(t, ctx, networkId)

		reportTime := testHoldDay0
		for _, args := range []*model.LegalHoldArgs{
			{NetworkId: networkId, CaseId: reportCase, Kind: sidecar.HoldKindReport2258a, ReportTime: &reportTime},
			{NetworkId: networkId, CaseId: preserveCase, Kind: sidecar.HoldKindPreserve2703f},
			{NetworkId: networkId, CaseId: renewedCase, Kind: sidecar.HoldKindPreserve2703f},
		} {
			if _, err := PlaceLegalHold(ctx, args); err != nil {
				t.Fatal(err)
			}
		}
		world.clock.AdvanceTo(testHoldDay(80))
		if _, err := RenewLegalHold(ctx, networkId, renewedCase); err != nil {
			t.Fatal(err)
		}
		if _, err := RenewLegalHold(ctx, networkId, renewedCase); !errors.Is(err, model.ErrLegalHoldNotRenewable) {
			t.Fatalf("a second renewal: %v", err)
		}
		if _, err := RenewLegalHold(ctx, networkId, reportCase); !errors.Is(err, model.ErrLegalHoldNotRenewable) {
			t.Fatalf("a §2258A renewal: %v", err)
		}

		endedAt := map[server.Id]int{}
		for day := 80; day <= 370; day += 1 {
			world.clock.AdvanceTo(testHoldDay(day))
			RunAbuseHoldPass(ctx, world.passDeps())
			for _, caseId := range []server.Id{reportCase, preserveCase, renewedCase} {
				if _, ended := endedAt[caseId]; !ended && model.GetLegalHold(ctx, networkId, caseId) == nil {
					endedAt[caseId] = day
					if world.object(t, ctx, networkId, caseId) != nil {
						t.Fatalf("day %d: an ended hold's object stayed", day)
					}
				}
			}
			if held := model.HeldNetworkIds(ctx, world.clock.Now())[networkId]; held != (day < 365) {
				t.Fatalf("day %d: held %t", day, held)
			}
		}
		if endedAt[preserveCase] != 90 || endedAt[renewedCase] != 180 || endedAt[reportCase] != 365 {
			t.Fatalf("ended at %v", endedAt)
		}
		offered, _ := world.sweeps()
		if len(offered) != 3 {
			t.Fatalf("%d sweeps offered", len(offered))
		}
		for _, request := range offered {
			if !strings.Contains(request.OnceKey, "hold_end") {
				t.Errorf("a sweep of another pass: %s", request.OnceKey)
			}
		}
	})
}

// Another staff-filed case naming the network.
func testHoldCaseOf(t testing.TB, ctx context.Context, networkId server.Id) (caseId server.Id) {
	t.Helper()
	var err error
	server.Tx(ctx, func(tx server.PgTx) {
		caseId, _, err = model.OpenAbuseCaseInTx(ctx, tx, &model.AbuseCaseArgs{
			NetworkId:     &networkId,
			Category:      model.AbuseCategoryOther,
			Target:        model.AbuseTarget{Kind: model.AbuseTargetKindNetwork, Name: networkId.String() + "-" + server.NewId().String()[24:]},
			ReporterClass: model.AbuseReporterClassLawEnforcement,
			JoinKind:      model.AbuseJoinKindNetwork,
			JoinKey:       networkId.String(),
		}, server.NowUtc(), model.DefaultAbuseSettings())
	})
	if err != nil {
		t.Fatal(err)
	}
	return
}

// A hold's end queues its sweep before its object goes; a kill between the two leaves object and row, and the
// next pass queues the sweep again, folding into the one still pending, and finishes.
func TestAbuseHoldPassQueuesTheSweepBeforeTheObjectGoes(t *testing.T) {
	requireAbuseTables(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newTestHoldWorld(t)
		networkId, caseId := newTestHoldCase(t, ctx)
		if _, err := PlaceLegalHold(ctx, &model.LegalHoldArgs{NetworkId: networkId, CaseId: caseId, Kind: sidecar.HoldKindPreserve2703f}); err != nil {
			t.Fatal(err)
		}
		world.clock.AdvanceTo(testHoldDay(90))

		deps := world.passDeps()
		deps.Step = func(step string, stepCaseId server.Id) {
			if step != AbuseHoldPassStepScheduled || stepCaseId != caseId {
				return
			}
			if _, pending := world.sweeps(); len(pending) != 1 || world.object(t, ctx, networkId, caseId) == nil {
				t.Errorf("at the barrier: %d pending sweeps, object %t", len(pending), world.object(t, ctx, networkId, caseId) != nil)
			}
			runtime.Goexit()
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			RunAbuseHoldPass(ctx, deps)
		}()
		<-done
		if world.object(t, ctx, networkId, caseId) == nil || model.GetLegalHold(ctx, networkId, caseId) == nil {
			t.Fatal("the kill lost the object or the row")
		}

		result := RunAbuseHoldPass(ctx, world.passDeps())
		if result.EndedCount != 1 || world.object(t, ctx, networkId, caseId) != nil || model.GetLegalHold(ctx, networkId, caseId) != nil {
			t.Fatalf("the rerun: %+v", result)
		}
		offered, pending := world.sweeps()
		if len(offered) != 2 || len(pending) != 1 || offered[0].OnceKey != offered[1].OnceKey {
			t.Fatalf("offered %d, pending %d", len(offered), len(pending))
		}
		if !pending[0].RunAt.Equal(testHoldDay(90)) {
			t.Fatalf("the sweep runs at %s", pending[0].RunAt)
		}
		if again := RunAbuseHoldPass(ctx, world.passDeps()); again.EndedCount != 0 || again.FailedCount != 0 {
			t.Fatalf("a third pass: %+v", again)
		}
	})
}

// A live row's lost object is put back, an object of other terms rewritten, and one keeping more left; an object
// with no row stays and alarms while it stands and ends past its own expiry, its sweep queued first; one that
// reads as no hold stays and alarms.
func TestAbuseHoldPassReconcilesObjectsAndRows(t *testing.T) {
	requireAbuseTables(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newTestHoldWorld(t)
		first, _ := world.blobs.FirstCluster()
		networkId, caseId := newTestHoldCase(t, ctx)
		placed, err := PlaceLegalHold(ctx, &model.LegalHoldArgs{NetworkId: networkId, CaseId: caseId, Kind: sidecar.HoldKindPreserve2703f})
		if err != nil {
			t.Fatal(err)
		}
		want, _ := sidecar.EncodeHoldRecord(placed.Hold.Record())

		// lost, as a site loss loses it: put back from the row
		if err := world.store.Delete(ctx, networkId, caseId); err != nil {
			t.Fatal(err)
		}
		world.clock.AdvanceTo(testHoldDay(1))
		result := RunAbuseHoldPass(ctx, world.passDeps())
		if result.RestoredCount != 1 || result.LiveCount != 1 || string(world.object(t, ctx, networkId, caseId)) != string(want) {
			t.Fatalf("a lost object: %+v", result)
		}

		// a shorter expiry than the row's: rewritten
		shorter := placed.Hold.Record()
		shorter.ExpireTime = testHoldDay(30)
		if err := sidecartest.PlaceHold(ctx, world.blobs, shorter); err != nil {
			t.Fatal(err)
		}
		if result := RunAbuseHoldPass(ctx, world.passDeps()); result.RewrittenCount != 1 || string(world.object(t, ctx, networkId, caseId)) != string(want) {
			t.Fatalf("an object keeping less: %+v", result)
		}
		// a longer one, as a renewal whose row did not commit: left
		longer := placed.Hold.Record()
		longer.ExpireTime = testHoldDay(180)
		if err := sidecartest.PlaceHold(ctx, world.blobs, longer); err != nil {
			t.Fatal(err)
		}
		if result := RunAbuseHoldPass(ctx, world.passDeps()); result.RewrittenCount != 0 {
			t.Fatalf("an object keeping more: %+v", result)
		}

		// an object with no row, as after a restore: alarms until its expiry, then ends with its sweep
		orphanNetwork := sidecar.NewId()
		orphanCase := sidecar.NewId()
		orphan := &sidecar.HoldRecord{
			NetworkId:  orphanNetwork,
			CaseId:     orphanCase,
			Kind:       sidecar.HoldKindPreserve2703f,
			Scope:      sidecar.HoldScopeNetwork(),
			PlaceTime:  testHoldDay0,
			ExpireTime: testHoldDay(90),
		}
		if err := sidecartest.PlaceHold(ctx, world.blobs, orphan); err != nil {
			t.Fatal(err)
		}
		unreadableKey := sidecar.HoldObjectKey(sidecar.NewId(), sidecar.NewId())
		if err := sidecartest.PlaceHoldBytes(ctx, world.blobs, unreadableKey, []byte(`{"version":2}`)); err != nil {
			t.Fatal(err)
		}
		result = RunAbuseHoldPass(ctx, world.passDeps())
		if result.UnrecordedCount != 1 || result.UnreadableCount != 1 || result.UnrecordedEndedCount != 0 {
			t.Fatalf("an object with no row: %+v", result)
		}
		if got := testutil.ToFloat64(model.Testing_AbuseHoldsGauge(string(sidecar.HoldKindPreserve2703f), model.AbuseHoldStateUnrecorded)); got != 1 {
			t.Fatalf("the gauge reads %v unrecorded", got)
		}
		if got := testutil.ToFloat64(model.Testing_AbuseHoldsGauge(string(sidecar.HoldKindPreserve2703f), model.AbuseHoldStateLive)); got != 1 {
			t.Fatalf("the gauge reads %v live", got)
		}

		world.clock.AdvanceTo(testHoldDay(90))
		result = RunAbuseHoldPass(ctx, world.passDeps())
		if result.UnrecordedEndedCount != 1 || result.EndedCount != 1 {
			t.Fatalf("past the expiries: %+v", result)
		}
		if _, err := first.Head(ctx, sidecar.HoldObjectKey(orphanNetwork, orphanCase)); !errors.Is(err, sidecar.ErrBlobNotFound) {
			t.Fatalf("the unrecorded object stayed: %v", err)
		}
		if _, err := first.Head(ctx, unreadableKey); err != nil {
			t.Fatalf("the unreadable object went: %v", err)
		}
		offered, _ := world.sweeps()
		if len(offered) != 2 {
			t.Fatalf("%d sweeps offered", len(offered))
		}
	})
}

// A pass with no hold/ writer or task queue ends nothing and puts back nothing: the object goes before the row.
func TestAbuseHoldPassWithNoWriterEndsNothing(t *testing.T) {
	requireAbuseTables(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newTestHoldWorld(t)
		networkId, caseId := newTestHoldCase(t, ctx)
		if _, err := PlaceLegalHold(ctx, &model.LegalHoldArgs{NetworkId: networkId, CaseId: caseId, Kind: sidecar.HoldKindPreserve2703f}); err != nil {
			t.Fatal(err)
		}
		world.clock.AdvanceTo(testHoldDay(90))
		for _, deps := range []*AbuseHoldPassDeps{
			{Tasks: world.tasks, Now: world.clock.Now},
			{Store: world.store, Now: world.clock.Now},
		} {
			if result := RunAbuseHoldPass(ctx, deps); result.EndedCount != 0 || result.FailedCount != 1 {
				t.Fatalf("%+v", result)
			}
		}
		if model.GetLegalHold(ctx, networkId, caseId) == nil || world.object(t, ctx, networkId, caseId) == nil {
			t.Fatal("a pass with no writer or queue ended the hold")
		}
	})
}

// The staff kinds: place_hold and renew_hold are legal's; report_csam bars the case, suspends the network through
// §21's one path and places report_2258a, and with holds off still bars and suspends, saying the hold waits.
func TestLegalHoldStaffKinds(t *testing.T) {
	requireAbuseTables(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newTestHoldWorld(t)
		effects, _ := newTestAbuseDeps(t)
		legal := &AbuseStaff{ReviewerId: server.NewId(), Roles: []AbuseStaffRole{AbuseStaffRoleLegal}}
		reviewer := &AbuseStaff{ReviewerId: server.NewId(), Roles: []AbuseStaffRole{AbuseStaffRoleReviewer}}
		networkId, caseId := newTestHoldCase(t, ctx)

		placeBody := `{"kind":"place_hold","case_id":"` + caseId.String() + `","network_id":"` + networkId.String() + `","hold_kind":"preserve_2703f"}`
		if _, err := AbuseStaffAction(ctx, reviewer, []byte(placeBody)); !errors.Is(err, ErrAbuseStaffRoleRefused) {
			t.Fatalf("a reviewer places: %v", err)
		}
		placed, err := AbuseStaffAction(ctx, legal, []byte(placeBody))
		if err != nil {
			t.Fatal(err)
		}
		hold := placed.Result.(*AbuseStaffHoldResult)
		if !hold.Inserted || hold.Hold.Kind != string(sidecar.HoldKindPreserve2703f) || string(hold.Hold.Scope) != `"network"` ||
			hold.Hold.ExpireTimeMs != testHoldDay(90).UnixMilli() {
			t.Fatalf("placed %+v", hold.Hold)
		}
		if _, err := AbuseStaffAction(ctx, legal, []byte(strings.Replace(placeBody, `"preserve_2703f"}`, `"preserve_2703f","report_time_ms":1}`, 1))); !errors.Is(err, ErrAbuseStaffRequestInvalid) {
			t.Fatalf("a §2703(f) report time: %v", err)
		}
		world.clock.AdvanceTo(testHoldDay(80))
		renewed, err := AbuseStaffAction(ctx, legal, []byte(`{"kind":"renew_hold","case_id":"`+caseId.String()+`","network_id":"`+networkId.String()+`"}`))
		if err != nil || renewed.Result.(*AbuseStaffHoldResult).Hold.ExpireTimeMs != testHoldDay(180).UnixMilli() {
			t.Fatalf("renewed %+v, %v", renewed, err)
		}

		csamNetwork, csamCase := newTestHoldCase(t, ctx)
		reportBody := `{"kind":"report_csam","case_id":"` + csamCase.String() + `","reason":"the report as filed"}`
		reported, err := AbuseStaffAction(ctx, reviewer, []byte(reportBody))
		if err != nil {
			t.Fatal(err)
		}
		report := reported.Result.(*AbuseStaffReportCsamResult)
		if !report.HoldPlaced || report.Hold.Kind != string(sidecar.HoldKindReport2258a) || !report.Case.Barred ||
			report.Suspension.Action.Kind != string(model.AbuseActionKindNetworkSuspend) {
			t.Fatalf("reported %+v", report)
		}
		if abuseCase := model.GetAbuseCase(ctx, csamCase); !abuseCase.Barred {
			t.Fatal("the case is not barred")
		}
		applied := effects.applied()
		if len(applied) != 1 || !applied[0].Barred || applied[0].Action.Kind != model.AbuseActionKindNetworkSuspend {
			t.Fatalf("effects %+v", applied)
		}
		if adminEvents, messages, _ := testHoldVisible(t, ctx, csamNetwork); adminEvents != 0 || messages != 0 {
			t.Fatalf("a barred suspension told the network: %d events, %d messages", adminEvents, messages)
		}

		pop := server.Testing_PushAbuseConfig("holds_enabled: false\nprotected_names: {roots: [root.example], platform: [platform.example]}\n")
		defer pop()
		_, offCase := newTestHoldCase(t, ctx)
		off, err := AbuseStaffAction(ctx, reviewer, []byte(`{"kind":"report_csam","case_id":"`+offCase.String()+`","reason":"the report as filed"}`))
		if err != nil {
			t.Fatal(err)
		}
		offReport := off.Result.(*AbuseStaffReportCsamResult)
		if offReport.HoldPlaced || offReport.HoldRefusal == "" || !offReport.Case.Barred || offReport.Suspension == nil {
			t.Fatalf("with holds off: %+v", offReport)
		}
	})
}
