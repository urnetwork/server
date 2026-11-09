package model

// QUOTA-M4-01: DESIGN.md 20.9's one draw order (D-QUOTA-9), its comparator, the source scan that
// keeps every site on it, and the escrow_draw_order gate's state, all without a database.

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/server"
)

func TestEscrowDrawLessTable(t *testing.T) {
	now := time.Date(2026, time.November, 9, 0, 0, 0, 0, time.UTC)
	low := server.Id{0x01}
	high := server.Id{0x02}
	// ids compare byte by byte, as PostgreSQL orders uuids: a high first byte wins over a
	// later one
	lowLater := server.Id{0x01, 0xff}
	balance := func(isCredit bool, endTime time.Time, startTime time.Time, balanceId server.Id) *escrowTransferBalance {
		return &escrowTransferBalance{isCredit: isCredit, endTime: endTime, startTime: startTime, balanceId: balanceId}
	}
	year := now.AddDate(1, 0, 0)
	month := now.AddDate(0, 0, 30)
	for _, c := range []struct {
		name string
		a    *escrowTransferBalance
		b    *escrowTransferBalance
		less bool
	}{
		// a NULL grant_kind reads false, as a data pack's does: both before the line
		{name: "pack before a line ending first", a: balance(false, year, now, high), b: balance(true, month, now, low), less: true},
		{name: "line after a pack ending later", a: balance(true, month, now, low), b: balance(false, year, now, high), less: false},
		{name: "earlier end first", a: balance(false, month, now, high), b: balance(false, year, now, low), less: true},
		{name: "end tie, earlier start first", a: balance(false, year, now.Add(-time.Hour), high), b: balance(false, year, now, low), less: true},
		{name: "end and start tie, lower id first", a: balance(false, year, now, low), b: balance(false, year, now, high), less: true},
		{name: "ids by first byte", a: balance(false, year, now, lowLater), b: balance(false, year, now, high), less: true},
		{name: "two lines, earlier end first", a: balance(true, month, now, high), b: balance(true, year, now, low), less: true},
		{name: "equal keys", a: balance(true, month, now, low), b: balance(true, month, now, low), less: false},
	} {
		if got := escrowDrawLess(c.a, c.b); got != c.less {
			t.Errorf("%s: escrowDrawLess = %t, want %t", c.name, got, c.less)
		}
		want := 0
		if c.less {
			want = -1
		} else if escrowDrawLess(c.b, c.a) {
			want = 1
		}
		if got := escrowDrawCompare(c.a, c.b); got != want {
			t.Errorf("%s: escrowDrawCompare = %d, want %d", c.name, got, want)
		}
	}
}

// Every key over a small domain with ties at each position: the order is a strict total order.
func TestEscrowDrawLessIsAStrictTotalOrder(t *testing.T) {
	now := time.Date(2026, time.November, 9, 0, 0, 0, 0, time.UTC)
	times := []time.Time{now, now.Add(time.Hour), now.Add(48 * time.Hour)}
	ids := []server.Id{{0x01}, {0x01, 0x02}, {0x02}}
	balances := []*escrowTransferBalance{}
	for _, isCredit := range []bool{false, true} {
		for _, endTime := range times {
			for _, startTime := range times {
				for _, balanceId := range ids {
					balances = append(balances, &escrowTransferBalance{
						isCredit:  isCredit,
						endTime:   endTime,
						startTime: startTime,
						balanceId: balanceId,
					})
				}
			}
		}
	}
	equal := func(a *escrowTransferBalance, b *escrowTransferBalance) bool {
		return a.drawKey() == b.drawKey()
	}
	for _, a := range balances {
		if escrowDrawLess(a, a) {
			t.Fatalf("irreflexivity: %+v draws before itself", a.drawKey())
		}
		for _, b := range balances {
			ab, ba := escrowDrawLess(a, b), escrowDrawLess(b, a)
			if ab && ba {
				t.Fatalf("antisymmetry: %+v and %+v each draw first", a.drawKey(), b.drawKey())
			}
			if !equal(a, b) && !ab && !ba {
				t.Fatalf("totality: %+v and %+v are unordered", a.drawKey(), b.drawKey())
			}
			if !ab {
				continue
			}
			for _, c := range balances {
				if escrowDrawLess(b, c) && !escrowDrawLess(a, c) {
					t.Fatalf("transitivity: %+v < %+v < %+v", a.drawKey(), b.drawKey(), c.drawKey())
				}
			}
		}
	}
	// a line never draws before a balance that is not one, whatever its times
	for _, a := range balances {
		for _, b := range balances {
			if a.isCredit && !b.isCredit && escrowDrawLess(a, b) {
				t.Fatalf("a line drew first: %+v before %+v", a.drawKey(), b.drawKey())
			}
		}
	}
	// DrawOrderBalance shares the keys
	a := &DrawOrderBalance{Credit: true, EndTime: now, BalanceId: ids[0]}
	b := &DrawOrderBalance{EndTime: now.Add(time.Hour), BalanceId: ids[2]}
	if compareDrawOrderBalances(a, b) != 1 || compareDrawOrderBalances(b, a) != -1 {
		t.Fatal("DrawOrderBalance does not follow escrowDrawLess")
	}
}

// The one order holds at every site only if no second sort of a payer's balances, and no
// equality spelling of the flag, exists in the package (DESIGN.md 20.9, plan-QUOTA-draw-order-sites).
func TestDrawOrderSitesShareOneOrder(t *testing.T) {
	balanceTypes := map[string]bool{
		"escrowTransferBalance": true,
		// createTransferEscrowInTx's local alias
		"escrow":           true,
		"DrawOrderBalance": true,
	}
	// the comparator itself, and the functions allowed to stand for it
	comparators := map[string]bool{
		"compareEscrowDrawKeys":    true,
		"escrowDrawLess":           true,
		"escrowDrawCompare":        true,
		"compareDrawOrderBalances": true,
	}
	sortFunctions := map[string]bool{
		"slices.SortFunc":         true,
		"slices.SortStableFunc":   true,
		"slices.IsSortedFunc":     true,
		"slices.BinarySearchFunc": true,
		"slices.MinFunc":          true,
		"slices.MaxFunc":          true,
		"sort.Slice":              true,
		"sort.SliceStable":        true,
	}
	equality := regexp.MustCompile(`grant_kind\s*(=|<>|!=)`)

	balanceParams := func(funcType *ast.FuncType) int {
		count := 0
		for _, field := range funcType.Params.List {
			if star, ok := field.Type.(*ast.StarExpr); ok {
				if ident, ok := star.X.(*ast.Ident); ok && balanceTypes[ident.Name] {
					count += max(1, len(field.Names))
				}
			}
		}
		return count
	}
	callsComparator := func(body ast.Node) bool {
		found := false
		ast.Inspect(body, func(node ast.Node) bool {
			if call, ok := node.(*ast.CallExpr); ok {
				if ident, ok := call.Fun.(*ast.Ident); ok && comparators[ident.Name] {
					found = true
				}
			}
			return true
		})
		return found
	}
	readsTimes := func(body ast.Node) bool {
		found := false
		ast.Inspect(body, func(node ast.Node) bool {
			if selector, ok := node.(*ast.SelectorExpr); ok {
				switch selector.Sel.Name {
				case "endTime", "EndTime", "startTime", "StartTime":
					found = true
				}
			}
			return true
		})
		return found
	}

	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fileSet := token.NewFileSet()
	scanned := 0
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if location := equality.FindIndex(data); location != nil {
			t.Errorf("%s: %q spells the credit flag with equality, which sorts NULL grant_kind after the line; use IS NOT DISTINCT FROM", path, data[location[0]:location[1]])
		}
		file, err := parser.ParseFile(fileSet, path, data, 0)
		if err != nil {
			t.Fatal(err)
		}
		scanned++
		ast.Inspect(file, func(node ast.Node) bool {
			switch node := node.(type) {
			case *ast.FuncDecl:
				// a second comparator of two balances by their times
				if node.Body != nil && !comparators[node.Name.Name] && balanceParams(node.Type) == 2 && readsTimes(node.Body) && !callsComparator(node.Body) {
					t.Errorf("%s: %s orders balances by their times outside escrowDrawLess", path, node.Name.Name)
				}
			case *ast.CallExpr:
				selector, ok := node.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				pkg, ok := selector.X.(*ast.Ident)
				if !ok || !sortFunctions[pkg.Name+"."+selector.Sel.Name] || len(node.Args) < 2 {
					return true
				}
				switch comparator := node.Args[len(node.Args)-1].(type) {
				case *ast.FuncLit:
					if 0 < balanceParams(comparator.Type) && !callsComparator(comparator.Body) {
						t.Errorf("%s:%d: a sort of balances that is not escrowDrawLess", path, fileSet.Position(node.Pos()).Line)
					}
				case *ast.Ident:
					if strings.Contains(strings.ToLower(comparator.Name), "draw") && !comparators[comparator.Name] {
						t.Errorf("%s:%d: a sort of balances by %s, not escrowDrawLess", path, fileSet.Position(node.Pos()).Line, comparator.Name)
					}
				}
			}
			return true
		})
	}
	if scanned < 100 {
		t.Fatalf("scanned %d files; the scan did not find the package", scanned)
	}

	// the scan is not vacuous: it finds today's sort and the equality spelling
	sample := `package model
func f(balances []*escrow) {
	slices.SortFunc(balances, func(a *escrow, b *escrow) int {
		return a.endTime.Compare(b.endTime)
	})
}
`
	file, err := parser.ParseFile(fileSet, "sample.go", sample, 0)
	if err != nil {
		t.Fatal(err)
	}
	flagged := false
	ast.Inspect(file, func(node ast.Node) bool {
		if call, ok := node.(*ast.CallExpr); ok {
			if comparator, ok := call.Args[len(call.Args)-1].(*ast.FuncLit); ok && 0 < balanceParams(comparator.Type) && !callsComparator(comparator.Body) {
				flagged = true
			}
		}
		return true
	})
	if !flagged || !equality.MatchString("ORDER BY grant_kind = 'team_credit'") || equality.MatchString(escrowDrawCreditSql) {
		t.Fatal("the scan misses what it is for")
	}
}

// Gate off, every escrow read is today's text byte for byte; on, the flag leads.
func TestEscrowDrawOrderReadsGateOffAreTodays(t *testing.T) {
	off := escrowBalanceRead{drawOrder: false}
	if off.sql() != escrowTransferBalanceSql {
		t.Fatal("gate-off escrow read changed")
	}
	if settlementEscrowReadSqlFor(false) != settlementEscrowReadSql {
		t.Fatal("gate-off settlement read changed")
	}
	if deadlineEscrowRowsSqlFor(false) != deadlineEscrowRowsSql {
		t.Fatal("gate-off deadline read changed")
	}
	on := escrowBalanceRead{drawOrder: true}
	if !strings.Contains(on.sql(), escrowDrawCreditSql) {
		t.Fatal("gate-on escrow read selects no flag")
	}
	if !strings.Contains(settlementEscrowReadSqlFor(true), "ORDER BY selected_balance.is_credit, selected_balance.end_time, selected_balance.start_time, selected_escrow.balance_id") {
		t.Fatal("gate-on settlement read is not in draw order")
	}
	if !strings.Contains(deadlineEscrowRowsSqlFor(true), "ORDER BY (b.grant_kind IS NOT DISTINCT FROM 'team_credit'),b.end_time NULLS LAST,b.start_time NULLS LAST,e.balance_id") {
		t.Fatal("gate-on deadline read is not in draw order")
	}
}

// A gate on fakes: a clock, the file and the look, each look run when the test asks.
type testingEscrowDrawOrderGate struct {
	gate      *escrowDrawOrderGate
	now       time.Time
	linked    bool
	fileOn    bool
	fileNamed bool
	line      bool
	lookCount int
	pending   []func()
}

func newTestingEscrowDrawOrderGate() *testingEscrowDrawOrderGate {
	fakes := &testingEscrowDrawOrderGate{
		now:    time.Date(2026, time.November, 9, 0, 0, 0, 0, time.UTC),
		linked: true,
	}
	settings := DefaultEscrowDrawOrderSettings()
	fakes.gate = &escrowDrawOrderGate{
		settings: settings,
		now:      func() time.Time { return fakes.now },
		linked:   func() bool { return fakes.linked },
		file:     func() (bool, bool) { return fakes.fileOn, fakes.fileNamed },
		lookForLine: func(ctx context.Context, now time.Time) bool {
			fakes.lookCount++
			return fakes.line
		},
		startLook: func(look func()) {
			fakes.pending = append(fakes.pending, look)
		},
	}
	return fakes
}

func (self *testingEscrowDrawOrderGate) runLooks() {
	pending := self.pending
	self.pending = nil
	for _, look := range pending {
		look()
	}
}

func TestEscrowDrawOrderGateNeedsMg6(t *testing.T) {
	fakes := newTestingEscrowDrawOrderGate()
	fakes.linked = false
	fakes.fileOn, fakes.fileNamed = true, true
	if fakes.gate.enabled() || fakes.gate.creditOpen() {
		t.Fatal("the gate opened without MG6")
	}
}

// A file that never named the gate leaves a process that never read it on in today's order,
// and costs no look.
func TestEscrowDrawOrderGateOffWithoutAName(t *testing.T) {
	fakes := newTestingEscrowDrawOrderGate()
	for range 3 {
		if fakes.gate.enabled() {
			t.Fatal("the gate opened with the file silent")
		}
		fakes.now = fakes.now.Add(time.Hour)
	}
	if len(fakes.pending) != 0 || fakes.lookCount != 0 {
		t.Fatal("a silent file started a look")
	}
}

// The file on opens the gate; the file off turns it off only once a look shows no line in
// window, and a line keeps it on, alarmed, whatever the file says (E-a).
func TestEscrowDrawOrderGateRefusesOffUnderALiveLine(t *testing.T) {
	fakes := newTestingEscrowDrawOrderGate()
	fakes.fileOn, fakes.fileNamed = true, true
	if !fakes.gate.enabled() {
		t.Fatal("the file on did not open the gate")
	}

	// off with a line in window: on until a look, then on and alarmed
	fakes.fileOn = false
	fakes.line = true
	refused := testutil.ToFloat64(escrowDrawOrderOffRefused)
	if !fakes.gate.enabled() {
		t.Fatal("the gate turned off before any look")
	}
	if len(fakes.pending) != 1 {
		t.Fatalf("%d looks started, want 1", len(fakes.pending))
	}
	// a second read while the look is out starts no other
	fakes.gate.enabled()
	if len(fakes.pending) != 1 {
		t.Fatal("a second look started while one was out")
	}
	fakes.runLooks()
	if !fakes.gate.enabled() {
		t.Fatal("the gate turned off under a live line")
	}
	if testutil.ToFloat64(escrowDrawOrderOffRefused) != refused+1 {
		t.Fatal("the refused transition was not alarmed")
	}

	// within the interval no new look; past it, the line has ended and the gate turns off
	fakes.now = fakes.now.Add(30 * time.Second)
	fakes.gate.enabled()
	if len(fakes.pending) != 0 {
		t.Fatal("a look started inside the interval")
	}
	fakes.line = false
	fakes.now = fakes.now.Add(31 * time.Second)
	if !fakes.gate.enabled() {
		t.Fatal("the gate turned off on a stale look")
	}
	fakes.runLooks()
	if fakes.gate.enabled() {
		t.Fatal("the gate stayed on with no line in window")
	}
	if testutil.ToFloat64(escrowDrawOrderOffRefused) != refused+1 {
		t.Fatal("a transition with no line was alarmed")
	}
}

// A failed look keeps the last answer and is retried past the interval.
func TestEscrowDrawOrderGateKeepsAnswerOnFailedLook(t *testing.T) {
	fakes := newTestingEscrowDrawOrderGate()
	fakes.fileOn, fakes.fileNamed = false, true
	fails := true
	fakes.gate.lookForLine = func(ctx context.Context, now time.Time) bool {
		if fails {
			panic("no connection")
		}
		return false
	}
	if !fakes.gate.enabled() {
		t.Fatal("an explicit off with no look answered off")
	}
	fakes.runLooks()
	if !fakes.gate.enabled() {
		t.Fatal("a failed look turned the gate off")
	}
	fakes.runLooks()
	fails = false
	fakes.gate.enabled()
	fakes.runLooks()
	if fakes.gate.enabled() {
		t.Fatal("a look that succeeded left the gate on")
	}
}

// A line may be written only once the gate has read on for CreditOpenDelay, and never while the
// file says off.
func TestEscrowDrawOrderCreditOpensAfterTheReread(t *testing.T) {
	fakes := newTestingEscrowDrawOrderGate()
	fakes.fileOn, fakes.fileNamed = true, true
	if fakes.gate.creditOpen() {
		t.Fatal("credit opened at once")
	}
	fakes.now = fakes.now.Add(fakes.gate.settings.CreditOpenDelay - time.Second)
	if fakes.gate.creditOpen() {
		t.Fatal("credit opened before the delay")
	}
	fakes.now = fakes.now.Add(time.Second)
	if !fakes.gate.creditOpen() {
		t.Fatal("credit did not open after the delay")
	}
	fakes.fileOn = false
	fakes.line = true
	if fakes.gate.creditOpen() {
		t.Fatal("credit open while the file says off")
	}
}

// An answer of no line from before the file read on is stale once it reads off again, since a line
// may have been written meanwhile: the gate stays on until a look that started after.
func TestEscrowDrawOrderGateDropsAStaleAnswer(t *testing.T) {
	fakes := newTestingEscrowDrawOrderGate()
	fakes.fileOn, fakes.fileNamed = false, true
	fakes.gate.enabled()
	fakes.runLooks()
	if fakes.gate.enabled() {
		t.Fatal("no line, and the gate stayed on")
	}

	// on, a line written, then off again
	fakes.fileOn = true
	fakes.gate.enabled()
	fakes.line = true
	fakes.fileOn = false
	if !fakes.gate.enabled() {
		t.Fatal("the gate turned off on an answer from before it read on")
	}
	fakes.runLooks()
	if !fakes.gate.enabled() {
		t.Fatal("the gate turned off under the line written while it was on")
	}

	// a look that started before the file read on answers nothing
	fakes.line = false
	fakes.now = fakes.now.Add(time.Hour)
	fakes.gate.enabled()
	fakes.fileOn = true
	fakes.gate.enabled()
	fakes.runLooks()
	fakes.fileOn = false
	if !fakes.gate.enabled() {
		t.Fatal("a look begun before the file read on decided the gate")
	}
}
