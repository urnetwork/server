// finished_task's partition family: its declaration, the drop rule, the
// runway around the cutover, and the single-writer premise of task identity.
// No database is read.
package task

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// The registered family is daily by run end time, keeps its days ahead,
// retires the legacy partition like a day, keeps no DEFAULT partition, and
// carries its fence.
func TestFinishedTaskPartitionFamilyDeclaration(t *testing.T) {
	family := server.GetPartitionFamily("finished_task")
	if family == nil || family != finishedTaskPartitionFamily {
		t.Fatal("finished_task is not registered as its own family")
	}
	settings := server.DefaultFinishedTaskPartitionSettings()
	if !slices.Equal(family.Tables, []string{"finished_task"}) || family.KeyColumn != "run_end_time" ||
		family.KeyKind != server.PartitionKeyTimestamp || family.Granularity != server.PartitionGranularityDaily {
		t.Fatalf("family key %+v", family)
	}
	if finishedTaskPostErrorRetention != 7*24*time.Hour {
		t.Fatalf("the failed Post retention is %s", finishedTaskPostErrorRetention)
	}
	if family.AheadRanges != settings.AheadDays || family.Retention != finishedTaskPostErrorRetention {
		t.Fatalf("family runway %d and retention %s", family.AheadRanges, family.Retention)
	}
	if family.DefaultPartition || family.KeepLegacyPartition || family.AnalyzeByOwnTask || family.RetireByOwnTask || !family.ReindexExempt {
		t.Fatalf("family policy %+v", family)
	}
	if family.LeafAnalyzeDelay != settings.LeafAnalyzeDelay || family.Settings == nil ||
		family.Settings.DropLockTimeout != settings.ExclusiveLockTimeout ||
		family.Settings.DropStatementTimeout != settings.CatalogStatementTimeout ||
		family.Settings.CreateLockTimeout != settings.CreateLockTimeout {
		t.Fatalf("family settings %+v", family.Settings)
	}
	if family.DropFence == nil || family.DropPrecondition == nil || family.Archive == nil || family.DropGuardSetting != "" {
		t.Fatal("family lacks its fence")
	}
	for _, table := range []string{"finished_task", "finished_task_p20261024", "finished_task_legacy"} {
		if server.PartitionFamilyOfTable(table) != family {
			t.Errorf("%s is not the family's", table)
		}
	}
	// the cutover's own tables are not leaves
	for _, table := range []string{"finished_task_staged", "finished_task_attach_probe", "finished_task_default", "pending_task"} {
		if server.PartitionFamilyOfTable(table) != nil {
			t.Errorf("%s counted as a leaf", table)
		}
	}
	if !server.PartitionRegistryReindexExempt("finished_task_p20261024", false) || server.PartitionRegistryReindexExempt("finished_task", false) {
		t.Error("days stay out of the rotation, and the plain table stays in it")
	}
}

// A day drops at the instant its upper bound is 7 days past and not before,
// so every row of a dropped day ended at least 7 days earlier, the failed
// Post retention, and at most 8; the legacy partition retires the same way at
// the cutover plus 7 days. A 25-hour-old and a 6-day-old failed Post are
// inside days that have not expired, at any time of day.
func TestFinishedTaskPartitionDropRule(t *testing.T) {
	ctx := context.Background()
	family := newFinishedTaskPartitionFamily(server.DefaultFinishedTaskPartitionSettings())
	day := family.RangeAt(time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC))
	cut := day.Upper.Add(7 * 24 * time.Hour)
	if family.IsExpired(ctx, cut.Add(-time.Nanosecond), day) || !family.IsExpired(ctx, cut, day) {
		t.Fatal("a day did not expire exactly 7 days after its upper bound")
	}
	for at := day.Lower; at.Before(day.Upper); at = at.Add(7 * time.Minute) {
		if age := cut.Sub(at); age < finishedTaskPostErrorRetention || 8*24*time.Hour < age {
			t.Fatalf("a row ending at %s is %s old when its day drops", at, age)
		}
	}
	legacy := server.PartitionRange{Upper: time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC), Legacy: true}
	legacyCut := legacy.Upper.Add(7 * 24 * time.Hour)
	if family.IsExpired(ctx, legacyCut.Add(-time.Nanosecond), legacy) || !family.IsExpired(ctx, legacyCut, legacy) {
		t.Fatal("the legacy partition did not retire at the cutover plus 7 days")
	}
	for now := time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC); now.Before(time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)); now = now.Add(37 * time.Minute) {
		for _, age := range []time.Duration{25 * time.Hour, 6 * 24 * time.Hour, 7*24*time.Hour - time.Second} {
			if family.IsExpired(ctx, now, family.RangeAt(now.Add(-age))) {
				t.Fatalf("at %s the day of a failed Post %s old expired", now, age)
			}
		}
	}
	if family.IsExpired(ctx, cut.AddDate(1, 0, 0), server.PartitionRange{Default: true}) {
		t.Fatal("a DEFAULT partition expired")
	}
}

// After the swap the legacy partition covers every day before the cutover
// day, so the runway creates nothing for them; from the cutover day on, the
// runway keeps the days ahead, and after downtime catches up today and the
// days ahead in one pass. A day that passed during the downtime is not
// created: a completion writes its own end time, today's.
func TestFinishedTaskPartitionRunwayAroundTheCutover(t *testing.T) {
	family := newFinishedTaskPartitionFamily(server.DefaultFinishedTaskPartitionSettings())
	cutoverDay := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
	leaf := func(r server.PartitionRange) server.PartitionLeaf {
		return server.PartitionLeaf{Table: "finished_task", Name: family.LeafName("finished_task", r), Range: r, Canonical: true}
	}
	leaves := []server.PartitionLeaf{leaf(server.PartitionRange{Upper: cutoverDay, Legacy: true})}
	for offset := 0; offset <= family.AheadRanges; offset += 1 {
		leaves = append(leaves, leaf(family.RangeAt(cutoverDay.AddDate(0, 0, offset))))
	}
	// the swap 14 hours before the cutover day
	swap := cutoverDay.Add(-14 * time.Hour)
	if missing := family.MissingRanges(leaves, swap, swap); len(missing) != 0 {
		t.Fatalf("the staged days left gaps: %v", missing)
	}
	if covered, future := family.Runway(leaves, swap); !covered || future < family.AheadRanges {
		t.Fatalf("runway at the swap %t %d", covered, future)
	}
	// five days on, with the runway task down since the swap
	later := cutoverDay.AddDate(0, 0, 5).Add(3 * time.Hour)
	missing := family.MissingRanges(leaves, later, later)
	want := []time.Time{}
	for offset := 5; offset <= 5+family.AheadRanges; offset += 1 {
		want = append(want, cutoverDay.AddDate(0, 0, offset))
	}
	lowers := []time.Time{}
	for _, r := range missing {
		lowers = append(lowers, r.Lower)
	}
	if !slices.EqualFunc(lowers, want, time.Time.Equal) {
		t.Fatalf("catch-up creates %v, want %v", lowers, want)
	}
	if covered, _ := family.Runway(leaves, later); covered {
		t.Fatal("a day past the staged ones counted covered")
	}
}

// An insert into finished_task, quoted or schema-qualified, but not into one
// of its partitions.
var finishedTaskInsertPattern = regexp.MustCompile(`(?i)\binsert\s+into\s+(?:public\.)?"?finished_task\b`)

// The identity of a finished task rests on its single writer: the fenced
// move out of pending_task, in the ordinary and the batched completion, each
// refusing an id that already finished. Any other statement that inserts
// into finished_task outside the tests would need its own identity proof, as
// the cutover's rollback has (it copies days back into the legacy table,
// refusing an id the legacy table holds).
func TestFinishedTaskOnlyTheFencedMoveInserts(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	writers := map[string]int{}
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			// the sibling repositories and build output are not this module's
			if name := entry.Name(); path != root && (strings.HasPrefix(name, ".") || name == "vendor" || name == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		source, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if count := len(finishedTaskInsertPattern.FindAllIndex(source, -1)); 0 < count {
			relative, _ := filepath.Rel(root, path)
			writers[relative] += count
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{
		filepath.Join("task", "task_run_once.go"):         1,
		filepath.Join("task", "task_completion_batch.go"): 1,
	}
	if len(writers) != len(want) {
		t.Fatalf("finished_task writers %v, want %v", writers, want)
	}
	for path, count := range want {
		if writers[path] != count {
			t.Fatalf("finished_task writers %v, want %v", writers, want)
		}
	}
	guard := "WHERE NOT EXISTS (SELECT 1 FROM finished_task WHERE finished_task.task_id=removed.task_id)"
	for _, statement := range []string{taskCompletionOwnerSql, taskCompletionBatchSql} {
		if strings.Count(statement, guard) != 1 || !strings.Contains(statement, "DELETE FROM pending_task WHERE task_id=") {
			t.Fatalf("a move lost its identity guard or its fenced delete:\n%s", statement)
		}
	}
}
