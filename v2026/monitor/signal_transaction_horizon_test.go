// Synthetic frames exercise the transaction horizon signal (SIGNALS.md §2.4l)
// through its public adapter: each holder kind at both thresholds, the
// timestamp and next-id history bounds, the ranking, withholding and malformed
// frames. No database is read; signal_transaction_horizon_native_test.go runs
// the read on the test PostgreSQL.
package monitor

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"
)

// One holder row, with ages relative to the frame's clock (zero is absent).
type transactionHorizonTestHolder struct {
	kind        string
	pid         int64
	backendType string
	state       string
	roleKind    string
	shape       string
	xidAge      int64
	xminAge     int64
	xactAge     time.Duration
	startedAge  time.Duration
	boundAge    time.Duration
	slotName    string
	slotType    string
}

func transactionHorizonTestSession(pid int64, xminAge int64, statementAge time.Duration) transactionHorizonTestHolder {
	return transactionHorizonTestHolder{
		kind: "session", pid: pid, backendType: "client backend", state: "active", roleKind: "ordinary", shape: "application",
		xidAge: -1, xminAge: xminAge, xactAge: statementAge, startedAge: statementAge, boundAge: statementAge,
	}
}

// A frame the source answers with; census fields default from the holders.
type transactionHorizonTestFrame struct {
	host        string
	observed    time.Time
	recovery    string
	nextXid     uint64
	maxPrepared string
	holders     []transactionHorizonTestHolder
	extraSlots  int
	// a rows hook for malformed frames
	mutate func([]Row) []Row
}

func newTransactionHorizonTestFrame(now time.Time, holders ...transactionHorizonTestHolder) *transactionHorizonTestFrame {
	return &transactionHorizonTestFrame{host: "pg-1", observed: now, recovery: "0", nextXid: 900_000_000, maxPrepared: "0", holders: holders}
}

func (self *transactionHorizonTestFrame) micros(age time.Duration) string {
	if age == 0 {
		return "0"
	}
	return strconv.FormatInt(self.observed.Add(-age).UnixMicro(), 10)
}

func (self *transactionHorizonTestFrame) rows(limit int) []Row {
	rows := []Row{{"meta", self.host, strconv.FormatInt(self.observed.UnixMicro(), 10), self.recovery,
		strconv.FormatUint(self.nextXid, 10), self.maxPrepared, "", "", "", "", "", "", "", "", ""}}
	sessions, slotHolders, prepared := 0, 0, 0
	earliest, earliestRank := int64(0), 0
	for index, holder := range self.holders {
		switch holder.kind {
		case "session":
			sessions++
		case "slot":
			slotHolders++
		case "prepared":
			prepared++
		}
		if holder.boundAge != 0 {
			bound, _ := strconv.ParseInt(self.micros(holder.boundAge), 10, 64)
			if earliest == 0 || bound < earliest {
				earliest, earliestRank = bound, index+1
			}
		}
		if index < limit {
			rows = append(rows, Row{"holder", strconv.Itoa(index + 1), holder.kind, strconv.FormatInt(holder.pid, 10),
				holder.backendType, holder.state, holder.roleKind, holder.shape,
				strconv.FormatInt(holder.xidAge, 10), strconv.FormatInt(holder.xminAge, 10),
				self.micros(holder.xactAge), self.micros(holder.startedAge), self.micros(holder.boundAge),
				holder.slotName, holder.slotType})
		}
	}
	rows = append(rows, Row{"end", strconv.Itoa(len(self.holders)), strconv.Itoa(min(len(self.holders), limit)),
		strconv.Itoa(sessions), strconv.Itoa(slotHolders + self.extraSlots), strconv.Itoa(slotHolders), strconv.Itoa(prepared),
		strconv.FormatInt(earliest, 10), strconv.Itoa(earliestRank), "", "", "", "", "", ""})
	if self.mutate != nil {
		rows = self.mutate(rows)
	}
	return rows
}

// A signal and a clock the tests advance between runs; the same signal
// instance keeps its next-id history across them.
type transactionHorizonTestRun struct {
	signal Signal
	now    time.Time
	frame  *transactionHorizonTestFrame
	query  string
}

func newTransactionHorizonTestRun() *transactionHorizonTestRun {
	return &transactionHorizonTestRun{signal: NewTransactionHorizonSignal(), now: time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)}
}

func (self *transactionHorizonTestRun) run(t *testing.T) Alerts {
	t.Helper()
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		self.query = query
		return self.frame.rows(DefaultTransactionHorizonSettings().MaxHolders), nil
	}}
	settings := syntheticSettings(source)
	now := self.now
	settings.Now = func() time.Time { return now }
	alerts, err := self.signal.Run(context.Background(), settings)
	if err != nil {
		t.Fatalf("horizon run failed: %v", err)
	}
	return alerts
}

// Runs one frame on a fresh signal.
func runTransactionHorizon(t *testing.T, holders ...transactionHorizonTestHolder) Alerts {
	t.Helper()
	run := newTransactionHorizonTestRun()
	run.frame = newTransactionHorizonTestFrame(run.now, holders...)
	return run.run(t)
}

func TestTransactionHorizonRegistered(t *testing.T) {
	found := 0
	for _, signal := range NewSignals() {
		if signal.Key() != "transaction-horizon" {
			continue
		}
		found++
		if signal.Number() != "2.4l" || signal.Cadence() != 5*time.Minute || signal.ID() != "pg/transaction-horizon" {
			t.Fatal("the transaction horizon lost its catalog identity or cadence")
		}
	}
	if found != 1 {
		t.Fatal("the §2.4l transaction horizon is not registered")
	}
}

// No holder, or holders under the warn age, is healthy.
func TestTransactionHorizonHealthy(t *testing.T) {
	if alerts := runTransactionHorizon(t); len(alerts) != 0 {
		t.Fatalf("no holder alerted: %s", alerts[0].Observed)
	}
	if alerts := runTransactionHorizon(t, transactionHorizonTestSession(41, 9000, 14*time.Minute+59*time.Second)); len(alerts) != 0 {
		t.Fatalf("a holder under 15 minutes alerted: %s", alerts[0].Observed)
	}
}

// A session warns from 15 minutes and pages from one hour, keeping one
// identity across the escalation.
func TestTransactionHorizonSessionThresholds(t *testing.T) {
	identity := ""
	for _, test := range []struct {
		age      time.Duration
		severity Severity
	}{
		{age: 15 * time.Minute, severity: SeverityWarn},
		{age: 59*time.Minute + 59*time.Second, severity: SeverityWarn},
		{age: time.Hour, severity: SeverityPage},
		{age: 3 * time.Hour, severity: SeverityPage},
	} {
		alert := requireAlertClass(t, runTransactionHorizon(t, transactionHorizonTestSession(41, 9000, test.age)), "transaction-horizon")
		if alert.Severity != test.severity || alert.Sustain != 1 || alert.Target != "pg-1" || alert.SignalNumber != "2.4l" ||
			!strings.Contains(alert.Observed, fmt.Sprintf("horizon_age_s>=%d ", int64(test.age.Seconds()))) ||
			!strings.Contains(alert.Observed, "age_source=timestamp age_source_holder=1") ||
			!strings.Contains(alert.Evidence, "holder[1] kind=session pid=41 backend=client backend state=active role=ordinary shape=application") ||
			!strings.Contains(alert.Evidence, "application=withheld query=withheld") ||
			!strings.Contains(alert.Mechanism, "started before its role picked up the bound") {
			t.Fatalf("age %s: wrong horizon alert: %s / %s / %s", test.age, alert.Severity, alert.Observed, alert.Evidence)
		}
		if identity != "" && alert.Identity() != identity {
			t.Fatal("the escalation to a page changed the alert identity")
		}
		identity = alert.Identity()
	}
}

// The first-ranked holder is named, and a lower-ranked holder's older
// timestamp still dates the horizon: ids are ordered, so it bounds the oldest.
func TestTransactionHorizonRankedHolderAndBound(t *testing.T) {
	writer := transactionHorizonTestSession(7, 0, 2*time.Minute)
	writer.xidAge, writer.xminAge, writer.boundAge = 50_000, -1, 0
	reader := transactionHorizonTestSession(8, 40_000, 20*time.Minute)
	alert := requireAlertClass(t, runTransactionHorizon(t, writer, reader), "transaction-horizon")
	if !strings.Contains(alert.Symptom, "pid 7") || !strings.Contains(alert.Observed, "horizon_age_xids=50000 ") ||
		!strings.Contains(alert.Observed, "age_source=timestamp age_source_holder=2") ||
		!strings.Contains(alert.Evidence, "holder[2] kind=session pid=8") {
		t.Fatalf("ranking or bound lost: %s / %s / %s", alert.Symptom, alert.Observed, alert.Evidence)
	}
}

// The query keeps §2.4's ranking: the greatest xid age, then the oldest
// transaction start, then stable keys.
func TestTransactionHorizonQueryRanksAsVacuumHealth(t *testing.T) {
	query := strings.Join(strings.Fields(transactionHorizonQuery(DefaultTransactionHorizonSettings())), " ")
	for _, clause := range []string{
		"ORDER BY greatest(holder.xid_age, holder.xmin_age) DESC, CASE WHEN holder.started_us = 0 THEN 1 ELSE 0 END, holder.started_us, holder.kind, holder.pid, holder.slot_name",
		"coalesce(a.xact_start, a.query_start)",
		"a.backend_type <> 'autovacuum worker'",
		"a.pid NOT IN (SELECT pid FROM vacuuming)",
		"FROM pg_replication_slots slot WHERE slot.xmin IS NOT NULL OR slot.catalog_xmin IS NOT NULL",
		"FROM pg_prepared_xacts p",
		"(SELECT count(*) FROM pg_replication_slots)::text",
		"(SELECT count(*) FROM pg_prepared_xacts)::text",
		"current_setting('max_prepared_transactions')",
		"pg_snapshot_xmax(pg_current_snapshot())",
		"WHERE r.rank <= 5",
	} {
		if !strings.Contains(query, clause) {
			t.Errorf("horizon query lost %q", clause)
		}
	}
}

// Statement text is classified inside the database and never selected out;
// names that could carry private text are never read.
func TestTransactionHorizonQueryWithholdsPrivateText(t *testing.T) {
	query := transactionHorizonQuery(DefaultTransactionHorizonSettings())
	for _, forbidden := range []string{"application_name", "client_addr", "client_hostname", "usename", "p.gid", "p.database", "a.query AS", "a.query,"} {
		if strings.Contains(query, forbidden) {
			t.Errorf("horizon query reads %q", forbidden)
		}
	}
	if uses, shapeTests := strings.Count(query, "a.query"), strings.Count(query, "a.query)"); shapeTests != 8 || uses != shapeTests+2 {
		t.Errorf("horizon query uses a.query %d times, %d of them shape tests; want only the eight shape tests and the two query_start reads", uses, shapeTests)
	}
}

// A prepared transaction is dated by its prepare, withholds its identifier,
// and is resolved, never restarted away.
func TestTransactionHorizonPreparedTransaction(t *testing.T) {
	prepared := transactionHorizonTestHolder{
		kind: "prepared", roleKind: "ordinary", shape: "prepared-transaction",
		xidAge: 70_000, xminAge: -1, xactAge: 2 * time.Hour, startedAge: 2 * time.Hour, boundAge: 2 * time.Hour,
	}
	alert := requireAlertClass(t, runTransactionHorizon(t, prepared), "transaction-horizon")
	if alert.Severity != SeverityPage || !strings.Contains(alert.Evidence, "holder[1] kind=prepared xid_age_xids=70000 prepared_age_s=7200 role=ordinary gid=withheld") ||
		!strings.Contains(alert.Observed, "prepared_transactions=1 max_prepared_transactions=0") ||
		!strings.Contains(alert.Action, "ROLLBACK PREPARED") || !strings.Contains(alert.Action, "survives the restart") {
		t.Fatalf("wrong prepared-transaction alert: %s / %s / %s", alert.Observed, alert.Evidence, alert.Action)
	}
}

// An inactive slot is dated from when it went inactive.
func TestTransactionHorizonInactiveSlot(t *testing.T) {
	slot := transactionHorizonTestHolder{
		kind: "slot", state: "inactive", shape: "replication-slot", xidAge: 80_000, xminAge: -1,
		startedAge: 20 * time.Minute, boundAge: 20 * time.Minute, slotName: "standby_a", slotType: "physical",
	}
	alert := requireAlertClass(t, runTransactionHorizon(t, slot), "transaction-horizon")
	if alert.Severity != SeverityWarn || !strings.Contains(alert.Symptom, "physical replication slot standby_a (inactive)") ||
		!strings.Contains(alert.Evidence, "holder[1] kind=slot slot=standby_a type=physical state=inactive xmin_age_xids=80000") ||
		!strings.Contains(alert.Observed, "slots=1 slot_holders=1") || !strings.Contains(alert.Action, "pg_drop_replication_slot") {
		t.Fatalf("wrong inactive-slot alert: %s / %s / %s", alert.Symptom, alert.Observed, alert.Evidence)
	}
}

// An active slot has no timestamp: the next-id history dates it. Its id was
// assigned before the first earlier read whose next id passed it.
func TestTransactionHorizonActiveSlotDatedByHistory(t *testing.T) {
	run := newTransactionHorizonTestRun()
	slotXid := uint64(899_000_000)
	slot := func(nextXid uint64) transactionHorizonTestHolder {
		return transactionHorizonTestHolder{
			kind: "slot", state: "active", shape: "replication-slot", xidAge: int64(nextXid - 1 - slotXid), xminAge: -1,
			slotName: "standby_b", slotType: "physical",
		}
	}
	next := uint64(900_000_000)
	read := func(advance time.Duration) Alerts {
		run.now = run.now.Add(advance)
		next += 300_000
		run.frame = newTransactionHorizonTestFrame(run.now, slot(next))
		run.frame.nextXid = next
		return run.run(t)
	}
	// first read: the slot's id was assigned before it, but nothing earlier
	// dates it yet
	if alerts := read(0); len(alerts) != 0 {
		t.Fatalf("an undated slot alerted on the first read: %s", alerts[0].Observed)
	}
	for minute := 5; minute < 15; minute += 5 {
		if alerts := read(5 * time.Minute); len(alerts) != 0 {
			t.Fatalf("minute %d: the slot alerted before the history reached 15 minutes: %s", minute, alerts[0].Observed)
		}
	}
	alert := requireAlertClass(t, read(5*time.Minute), "transaction-horizon")
	if alert.Severity != SeverityWarn || !strings.Contains(alert.Observed, "horizon_age_s>=900 ") ||
		!strings.Contains(alert.Observed, "age_source=history") || !strings.Contains(alert.Observed, "history_s=900") {
		t.Fatalf("history did not date the slot at 15 minutes: %s", alert.Observed)
	}
	for minute := 20; minute < 60; minute += 5 {
		read(5 * time.Minute)
	}
	alert = requireAlertClass(t, read(5*time.Minute), "transaction-horizon")
	if alert.Severity != SeverityPage || !strings.Contains(alert.Observed, "horizon_age_s>=3600 ") {
		t.Fatalf("history did not page the slot at an hour: %s / %s", alert.Severity, alert.Observed)
	}
}

// A holder whose id was assigned after the last earlier read is fresh.
func TestTransactionHorizonHistoryKeepsFreshHolderHealthy(t *testing.T) {
	run := newTransactionHorizonTestRun()
	for index := range 20 {
		run.now = run.now.Add(5 * time.Minute)
		// about 3,300 ids a second; the feedback is 1,000 ids behind
		next := uint64(900_000_000) + uint64(index)*1_000_000
		walsender := transactionHorizonTestHolder{
			kind: "session", pid: 99, backendType: "walsender", state: "active", roleKind: "ordinary", shape: "replication",
			xidAge: -1, xminAge: 1_000, startedAge: 48 * time.Hour,
		}
		run.frame = newTransactionHorizonTestFrame(run.now, walsender)
		run.frame.nextXid = next
		if alerts := run.run(t); len(alerts) != 0 {
			t.Fatalf("a walsender's fresh feedback horizon alerted: %s", alerts[0].Observed)
		}
	}
}

// A next id that goes backwards (a restored or different cluster) starts the
// history over instead of dating holders against the old one.
func TestTransactionHorizonHistoryResetsOnRegression(t *testing.T) {
	run := newTransactionHorizonTestRun()
	slot := transactionHorizonTestHolder{
		kind: "slot", state: "active", shape: "replication-slot", xidAge: 10_000_000, xminAge: -1,
		slotName: "standby_c", slotType: "physical",
	}
	for index := range 5 {
		run.now = run.now.Add(5 * time.Minute)
		run.frame = newTransactionHorizonTestFrame(run.now, slot)
		run.frame.nextXid = 900_000_000 + uint64(index)*100_000
		run.run(t)
	}
	run.now = run.now.Add(5 * time.Minute)
	run.frame = newTransactionHorizonTestFrame(run.now, slot)
	run.frame.nextXid = 500_000_000
	if alerts := run.run(t); len(alerts) != 0 {
		t.Fatalf("a regressed next id kept the old history: %s", alerts[0].Observed)
	}
}

// Each first-ranked holder's fixed shape picks its reading.
func TestTransactionHorizonAttribution(t *testing.T) {
	for _, test := range []struct {
		shape, roleKind, backendType, want string
	}{
		{shape: "reindex", roleKind: "ordinary", backendType: "client backend", want: "pg_stat_progress_create_index"},
		{shape: "vacuum", roleKind: "ordinary", backendType: "client backend", want: "pg_stat_progress_cluster"},
		{shape: "validate", roleKind: "ordinary", backendType: "client backend", want: "the rotation resumes the table"},
		{shape: "dump", roleKind: "ordinary", backendType: "client backend", want: "reschedule a dump"},
		{shape: "application", roleKind: "superuser", backendType: "client backend", want: "Ask the operator who owns the pid"},
		{shape: "replication", roleKind: "ordinary", backendType: "walsender", want: "standby's long query"},
		{shape: "application", roleKind: "ordinary", backendType: "client backend", want: "recycle that pool"},
	} {
		holder := transactionHorizonTestSession(51, 9000, 30*time.Minute)
		holder.shape, holder.roleKind, holder.backendType = test.shape, test.roleKind, test.backendType
		alert := requireAlertClass(t, runTransactionHorizon(t, holder), "transaction-horizon")
		if !strings.Contains(alert.Action, test.want) || !strings.Contains(alert.Evidence, "shape="+test.shape) {
			t.Errorf("%s/%s: action %q lacks %q", test.shape, test.roleKind, alert.Action, test.want)
		}
	}
}

// The census reports every slot and prepared transaction, zero included.
func TestTransactionHorizonCensusCountsZero(t *testing.T) {
	run := newTransactionHorizonTestRun()
	run.frame = newTransactionHorizonTestFrame(run.now, transactionHorizonTestSession(61, 9000, 20*time.Minute))
	run.frame.extraSlots = 2
	alert := requireAlertClass(t, run.run(t), "transaction-horizon")
	if !strings.Contains(alert.Observed, "holders=1 session_holders=1 slots=2 slot_holders=0 prepared_transactions=0 max_prepared_transactions=0") {
		t.Fatalf("the census lost a zero or the slot total: %s", alert.Observed)
	}
	if !strings.Contains(run.query, `\set monitor_primary_hostname`) {
		t.Fatal("the horizon read lost its primary hostname guard")
	}
}

// The frame names at most five holders; the census binds the rest.
func TestTransactionHorizonBoundsNamedHolders(t *testing.T) {
	holders := []transactionHorizonTestHolder{}
	for index := range 8 {
		holders = append(holders, transactionHorizonTestSession(int64(100+index), int64(9000-index), 20*time.Minute))
	}
	alert := requireAlertClass(t, runTransactionHorizon(t, holders...), "transaction-horizon")
	if !strings.Contains(alert.Evidence, "holder[5]") || strings.Contains(alert.Evidence, "holder[6]") ||
		!strings.Contains(alert.Observed, "holders=8 session_holders=8") {
		t.Fatalf("named holders or census wrong: %s / %s", alert.Observed, alert.Evidence)
	}
}

// Every frame defect is unknown with a fixed reason, never healthy, and never
// echoes a private cell.
func TestTransactionHorizonMalformedFramesAreUnknown(t *testing.T) {
	const canary = "private-horizon-canary"
	for _, test := range []struct {
		name   string
		mutate func(*transactionHorizonTestFrame)
		reason string
	}{
		{name: "replica", mutate: func(f *transactionHorizonTestFrame) { f.recovery = "1" }, reason: "source-identity-or-role-unavailable"},
		{name: "other host", mutate: func(f *transactionHorizonTestFrame) { f.host = "pg-2" }, reason: "source-identity-or-role-unavailable"},
		{name: "clock", mutate: func(f *transactionHorizonTestFrame) { f.observed = f.observed.Add(time.Hour) }, reason: "source-clock-unavailable"},
		{name: "next id", mutate: func(f *transactionHorizonTestFrame) { f.nextXid = 0 }, reason: "invalid-next-transaction-id"},
		{name: "backend text", mutate: func(f *transactionHorizonTestFrame) { f.holders[0].backendType = canary + "; X" }, reason: "invalid-session-holder"},
		{name: "slot name", mutate: func(f *transactionHorizonTestFrame) {
			f.holders[0] = transactionHorizonTestHolder{kind: "slot", state: "active", shape: "replication-slot", xidAge: 5, xminAge: -1, slotName: canary, slotType: "physical"}
		}, reason: "invalid-slot-holder"},
		{name: "census", mutate: func(f *transactionHorizonTestFrame) {
			f.mutate = func(rows []Row) []Row { rows[len(rows)-1][1] = "7"; return rows }
		}, reason: "invalid-census"},
		{name: "missing holder row", mutate: func(f *transactionHorizonTestFrame) {
			f.mutate = func(rows []Row) []Row { return append(rows[:1], rows[2:]...) }
		}, reason: "invalid-census"},
		{name: "width", mutate: func(f *transactionHorizonTestFrame) {
			f.mutate = func(rows []Row) []Row { rows[1] = append(rows[1], canary); return rows }
		}, reason: "invalid-frame-width"},
		{name: "order", mutate: func(f *transactionHorizonTestFrame) {
			f.holders = append(f.holders, transactionHorizonTestSession(72, 90_000, time.Minute))
		}, reason: "invalid-holder-order"},
	} {
		run := newTransactionHorizonTestRun()
		run.frame = newTransactionHorizonTestFrame(run.now, transactionHorizonTestSession(71, 9000, 20*time.Minute))
		test.mutate(run.frame)
		alerts := run.run(t)
		if len(alerts) != 1 || alerts[0].Class != "transaction-horizon-unobservable" || alerts[0].Sustain != 2 ||
			!strings.Contains(alerts[0].Observed, "reason="+test.reason) {
			t.Errorf("%s: frame defect not unknown with reason %s: %+v", test.name, test.reason, alerts)
			continue
		}
		requireAlertOmits(t, alerts[0], canary)
	}
}

// A source failure is an unknown finding with its fixed error class; a
// canceled run stays an error.
func TestTransactionHorizonSourceFailures(t *testing.T) {
	private := errors.New("private-horizon-error-canary")
	source := &syntheticSource{postgresFn: func(string) ([]Row, error) { return nil, private }}
	alerts, err := NewTransactionHorizonSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil || len(alerts) != 1 || alerts[0].Class != "transaction-horizon-unobservable" ||
		!strings.Contains(alerts[0].Observed, "reason=horizon-source-unavailable") {
		t.Fatalf("a source failure was not unknown: %v %+v", err, alerts)
	}
	requireAlertOmits(t, alerts[0], "private-horizon-error-canary")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := NewTransactionHorizonSignal().Run(ctx, syntheticSettings(source)); !errors.Is(err, context.Canceled) {
		t.Fatalf("a canceled run returned %v", err)
	}
}
