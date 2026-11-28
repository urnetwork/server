package usagearchive

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testReviewTable() ReviewTable {
	return ReviewTable{
		Table: "contract_close",
		Columns: []ReviewColumn{
			{Name: "contract_id", Type: "uuid"},
			{Name: "close_time", Type: "timestamp without time zone"},
			{Name: "party", Type: "character varying(16)"},
			{Name: "checkpoint", Type: "boolean"},
			{Name: "note", Type: "jsonb"},
		},
		Key: []string{"contract_id", "party"},
	}
}

const (
	testReviewIdA = "019fe50a-6b53-ec66-080f-b6ba59649f66"
	testReviewIdB = "019fe50a-7811-e9d5-c88a-91145cc5a8dc"
	testReviewIdC = "019fe51d-c309-5a9f-794a-bdf5039ada1f"
)

// Keys compare as their raw values: a party that is a tab (escaped \t) sorts
// before one that is a backslash (escaped \\), though the escaped texts sort
// the other way. A NULL prints as \N.
const testReviewLines = testReviewIdA + "\t2026-08-09 05:41:33.139\ta\\tb\tf\t\\N\n" +
	testReviewIdA + "\t2026-08-09 05:41:33.139\tdestination\tt\t{\"a\": \"b\\\\\\\\c\"}\n" +
	testReviewIdA + "\t2026-08-09 05:41:33.139\tsource\tt\t{}\n" +
	testReviewIdB + "\t\\N\t\\t\tf\t\\N\n" +
	testReviewIdB + "\t\\N\t\\\\\tf\t\\N\n"

func testReviewFigures(t *testing.T, stream string, chunk int) *reviewFigures {
	t.Helper()
	figures, err := newReviewFigures(testReviewTable(), zeroContractId, testReviewIdB)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte(stream)
	for 0 < len(data) {
		n := min(chunk, len(data))
		figures.Write(data[:n])
		data = data[n:]
	}
	return figures
}

// The figures do not depend on how the stream is cut into writes, and the
// digest is the week export's line digest summed.
func TestReviewFiguresAcrossWrites(t *testing.T) {
	whole := testReviewFigures(t, testReviewLines, len(testReviewLines))
	if err := whole.Close(); err != nil {
		t.Fatal(err)
	}
	var want digestSum
	for _, line := range strings.Split(strings.TrimSuffix(testReviewLines, "\n"), "\n") {
		want.Add(LineDigest([]byte(line)))
	}
	if whole.Rows != 5 || whole.Digest() != want.String() {
		t.Fatalf("figures %d rows, digest %s; want 5, %s", whole.Rows, whole.Digest(), want.String())
	}
	for _, chunk := range []int{1, 2, 7, 36, 37, 64} {
		cut := testReviewFigures(t, testReviewLines, chunk)
		if err := cut.Close(); err != nil {
			t.Fatal(err)
		}
		if cut.Rows != whole.Rows || cut.Digest() != whole.Digest() {
			t.Fatalf("chunks of %d give %d rows, digest %s", chunk, cut.Rows, cut.Digest())
		}
	}
}

// A repeated or decreasing key, a contract outside the batch or not one of its
// contracts, a line with too few fields, and a stream cut inside a line all
// fail.
func TestReviewFiguresRefuseMalformedStreams(t *testing.T) {
	lines := strings.Split(strings.TrimSuffix(testReviewLines, "\n"), "\n")
	cases := map[string]string{
		"repeated":    lines[0] + "\n" + lines[0] + "\n",
		"decreasing":  lines[2] + "\n" + lines[1] + "\n",
		"outside":     strings.Replace(lines[3], testReviewIdB, testReviewIdC, 1) + "\n",
		"fields":      testReviewIdA + "\tx\n",
		"cut":         lines[0],
		"no contract": "x\t\\N\tsource\tf\t\\N\n",
	}
	for name, stream := range cases {
		figures := testReviewFigures(t, stream, 5)
		if err := figures.Close(); err == nil {
			t.Fatalf("%s: a malformed stream passed", name)
		}
	}
	figures := testReviewFigures(t, "", 1)
	figures.ids = map[string]bool{testReviewIdA: true}
	figures.Write([]byte(lines[3] + "\n"))
	if err := figures.Close(); err == nil {
		t.Fatal("a contract that is not one of the batch's passed")
	}
}

// COPY's escapes come off for key comparisons, so keys compare as the raw
// values PostgreSQL ordered.
func TestUnescapeCopyField(t *testing.T) {
	for escaped, raw := range map[string]string{
		`plain`:      "plain",
		`a\tb`:       "a\tb",
		`back\\s`:    `back\s`,
		`\b\f\n\r\v`: "\b\f\n\r\v",
		`trail\`:     `trail\`,
	} {
		if got := string(unescapeCopyField([]byte(escaped))); got != raw {
			t.Fatalf("%q unescaped to %q, want %q", escaped, got, raw)
		}
	}
}

// Only the types whose text can hold a backslash or a control character are
// escaped, and every value prints with its type's output function.
func TestReviewCopyLineSql(t *testing.T) {
	sql := ReviewCopyLineSql(testReviewTable(), "t")
	if strings.Count(sql, "format('%s', ") != 5 {
		t.Fatalf("not every column prints with format: %s", sql)
	}
	// each escape ends with the vertical tab's
	if strings.Count(sql, "chr(11)") != 2 || strings.Count(sql, "format('%s', t.\"party\"), E'\\\\', E'\\\\\\\\')") != 1 {
		t.Fatalf("not exactly the varchar and jsonb columns are escaped: %s", sql)
	}
	if strings.Contains(sql, `t."checkpoint"::text`) {
		t.Fatalf("a boolean cast to text prints true, not t: %s", sql)
	}
	order := reviewOrderSql(testReviewTable())
	if order != `t."contract_id", t."party" COLLATE "C"` {
		t.Fatalf("order %s", order)
	}
}

func TestParseReviewPaymentIds(t *testing.T) {
	paymentIds, err := ParseReviewPaymentIds(" " + strings.ToUpper(testReviewIdB) + "," + testReviewIdA)
	if err != nil || len(paymentIds) != 2 || paymentIds[0] != testReviewIdA || paymentIds[1] != testReviewIdB {
		t.Fatalf("parsed %v, %v", paymentIds, err)
	}
	for _, value := range []string{"", testReviewIdA + "," + testReviewIdA, testReviewIdA + ",", "not-a-uuid"} {
		if _, err := ParseReviewPaymentIds(value); err == nil {
			t.Fatalf("%q parsed", value)
		}
	}
}

// The floor of a time is its Unix milliseconds in the first six bytes, and
// the time of an id is read back from them.
func TestContractIdFloorAndTime(t *testing.T) {
	at := time.Date(2026, 8, 9, 5, 41, 33, 139000000, time.UTC)
	floor := ContractIdFloor(at)
	if floor != "019fe50a-6b53-0000-0000-000000000000" {
		t.Fatalf("floor %s", floor)
	}
	back, err := ContractIdTime(testReviewIdA)
	if err != nil || !back.Equal(at) {
		t.Fatalf("time %s, %v", back, err)
	}
	if week := reviewWeekOf(at); week.Format(time.DateOnly) != "2026-08-03" || week.Weekday() != time.Monday {
		t.Fatalf("week %s", week)
	}
}

func testReviewBatchDir(t *testing.T) (string, *ReviewRun) {
	t.Helper()
	dir := t.TempDir()
	run := &ReviewRun{Format: ReviewRunFormat, RunId: "019fe50a-0000-7000-8000-000000000001", Payments: []ReviewPayment{{PaymentId: testReviewIdC, Row: "{}"}}}
	for _, table := range ReviewTables {
		run.Tables = append(run.Tables, ReviewTable{Table: table, Columns: []ReviewColumn{{Name: "contract_id", Type: "uuid"}}, Key: []string{"contract_id"}})
	}
	if err := writeReviewJson(filepath.Join(dir, reviewRunFile), run); err != nil {
		t.Fatal(err)
	}
	return dir, run
}

func testWriteReviewBatch(t *testing.T, dir string, run *ReviewRun, index int64, after string, upper string) {
	t.Helper()
	batch := &ReviewBatch{
		Format: ReviewBatchFormat, RunId: run.RunId, Batch: index,
		AfterContractId: after, UpperContractId: upper, Contracts: 1, VerifiedTime: "x",
		PaymentContracts: map[string]int64{},
	}
	for _, table := range ReviewTables {
		batch.Tables = append(batch.Tables, ReviewTableCopy{Table: table, File: reviewCopyName(index, table)})
		if err := os.WriteFile(filepath.Join(dir, reviewCopyName(index, table)), nil, 0o640); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeReviewJson(filepath.Join(dir, reviewManifestName(index)), batch); err != nil {
		t.Fatal(err)
	}
}

// Batches tile the id space from the zero id; the files of the batch after
// the last are an unfinished batch; a gap, a batch that does not start where
// the one before ended, and a file of no batch are refused.
func TestReviewBatchStateTilesTheIdSpace(t *testing.T) {
	dir, run := testReviewBatchDir(t)
	testWriteReviewBatch(t, dir, run, 1, zeroContractId, testReviewIdA)
	testWriteReviewBatch(t, dir, run, 2, testReviewIdA, testReviewIdB)
	if err := os.WriteFile(filepath.Join(dir, reviewCopyName(3, "transfer_contract")), nil, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, reviewCopyName(3, "transfer_escrow")+partialSuffix), nil, 0o640); err != nil {
		t.Fatal(err)
	}
	batches, unfinished, err := reviewBatchState(dir, run)
	if err != nil || len(batches) != 2 || len(unfinished) != 2 {
		t.Fatalf("%d batches, unfinished %v, %v", len(batches), unfinished, err)
	}
	if _, err := clearUnfinishedReviewBatch(dir, run); err != nil {
		t.Fatal(err)
	}
	if _, unfinished, err := reviewBatchState(dir, run); err != nil || len(unfinished) != 0 {
		t.Fatalf("unfinished %v after clearing, %v", unfinished, err)
	}

	stray, _ := testReviewBatchDir(t)
	testWriteReviewBatch(t, stray, run, 1, zeroContractId, testReviewIdA)
	if err := os.WriteFile(filepath.Join(stray, reviewCopyName(5, "transfer_contract")), nil, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, _, err := reviewBatchState(stray, run); err == nil {
		t.Fatal("a file of no batch passed")
	}

	gap, _ := testReviewBatchDir(t)
	testWriteReviewBatch(t, gap, run, 1, zeroContractId, testReviewIdA)
	testWriteReviewBatch(t, gap, run, 3, testReviewIdA, testReviewIdB)
	if _, _, err := reviewBatchState(gap, run); err == nil {
		t.Fatal("a missing batch passed")
	}

	overlap, _ := testReviewBatchDir(t)
	testWriteReviewBatch(t, overlap, run, 1, zeroContractId, testReviewIdB)
	testWriteReviewBatch(t, overlap, run, 2, testReviewIdA, testReviewIdC)
	if _, _, err := reviewBatchState(overlap, run); err == nil {
		t.Fatal("overlapping batches passed")
	}
}

// The plan's digest changes with every input a commit depends on.
func TestReviewClearPlanDigest(t *testing.T) {
	plan := func() *ReviewClearPlan {
		return &ReviewClearPlan{
			Change: reviewClearChange, Run: &ReviewRun{RunId: "r"}, Batches: 2, Contracts: 9,
			PaymentContracts: map[string]int64{testReviewIdA: 9}, ManifestsSha256: "m",
			VerificationFile: "/x/verify-1.json", VerificationSha256: "v",
			Payments: []ReviewPayment{{PaymentId: testReviewIdA, Row: `{"a": 1}`}},
		}
	}
	base := plan().digest()
	if base != plan().digest() {
		t.Fatal("the digest is not deterministic")
	}
	changes := []func(*ReviewClearPlan){
		func(p *ReviewClearPlan) { p.Change = reviewRestoreChange },
		func(p *ReviewClearPlan) { p.Run.RunId = "s" },
		func(p *ReviewClearPlan) { p.Batches = 3 },
		func(p *ReviewClearPlan) { p.Contracts = 8 },
		func(p *ReviewClearPlan) { p.PaymentContracts[testReviewIdA] = 8 },
		func(p *ReviewClearPlan) { p.ManifestsSha256 = "n" },
		func(p *ReviewClearPlan) { p.VerificationSha256 = "w" },
		func(p *ReviewClearPlan) { p.Payments[0].Row = `{"a": 2}` },
	}
	for i, change := range changes {
		changed := plan()
		change(changed)
		if changed.digest() == base {
			t.Fatalf("change %d left the digest as it was", i)
		}
	}
}

func TestReviewRefusal(t *testing.T) {
	err := reviewRefusal("payment %s changed", testReviewIdA)
	if !IsReviewRefusal(err) || !IsReviewRefusal(errors.Join(errors.New("x"), err)) || IsReviewRefusal(errors.New("x")) {
		t.Fatal("refusals are not told apart from failures")
	}
}
