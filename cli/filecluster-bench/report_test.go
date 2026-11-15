package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/sidecar/evsservice"
	"github.com/urnetwork/sidecar/sidecartest/load"
)

// The build every fixture is a run of: synthetic commits.
func fixtureCommitSet() map[string]string {
	return map[string]string{"sidecar": "5c0ffee5c0ffee5c0ffee5c0ffee5c0ffee5c0ff", "server": "be5be5be5be5be5be5be5be5be5be5be5be5be5b"}
}

func fixtureFs116() *Fs116Evidence {
	return &Fs116Evidence{
		CommitSet: fixtureCommitSet(),
		Listed:    []string{"TestFs116SteadyLoadAdmitsTenPushesAnInterval", "TestFs116AllowanceRampsBackAfterAStorm"},
		Passed:    []string{"TestFs116SteadyLoadAdmitsTenPushesAnInterval", "TestFs116AllowanceRampsBackAfterAStorm"},
		Time:      time.Date(2026, time.November, 1, 0, 0, 0, 0, time.UTC),
		Command:   "go test -count=1 -json -run ^TestFs116 ./sidecartest/fault/",
	}
}

const fixtureDriveCount = 8

// Each drive's bytes over a window: the busiest's exactly, the others nine tenths of it.
func fixtureDrives(busiestByteCount uint64, busiestWrites uint64, busiestReads uint64) []*DriveDelta {
	drives := []*DriveDelta{}
	for i := range fixtureDriveCount {
		drive := &DriveDelta{DriveId: fmt.Sprintf("0-0-%d", i), WrittenByteCount: busiestByteCount, WritesCompleted: busiestWrites, ReadsCompleted: busiestReads}
		if i != 0 {
			drive.WrittenByteCount = busiestByteCount * 9 / 10
			drive.WritesCompleted = busiestWrites * 9 / 10
		}
		drives = append(drives, drive)
	}
	return drives
}

// The per-drive costs an ops fixture measures: §11.13's rows on 4 KiB blocks, and on 1 KiB blocks the scaled ones
// evsservice's defaults hold.
type fixtureCosts struct {
	create    int64
	overwrite int64
	delete    int64
}

func fixtureCostsOf(blockByteCount int64) fixtureCosts {
	if blockByteCount == 1024 {
		return fixtureCosts{create: 7168, overwrite: 10752, delete: 2816}
	}
	return fixtureCosts{create: 9216, overwrite: 14336, delete: 3584}
}

// What evsservice.PriceTable's model adds for an object past 14 KiB on four shards, rounded to the block.
func fixtureLargeByteCount(objectByteCount int64, blockByteCount int64) int64 {
	if objectByteCount <= 14*1024 {
		return 0
	}
	shard := (objectByteCount - 14*1024 + 3) / 4
	return (shard + blockByteCount - 1) / blockByteCount * blockByteCount
}

const (
	fixtureOpsSeconds        = 600
	fixtureOpsCount          = 300000
	fixtureIdleSeconds       = 120
	fixtureIdleBytesASecond  = 1000
	fixtureGateSeconds       = 3600
	fixtureGatePushes        = 1440000
	fixturePricedByteCount   = 45 * 1000 * 1000 * 1000
	fixtureP0RevocationBytes = 300 * 1000 * 1000
	fixtureP0RotationBytes   = 600 * 1000 * 1000
	fixtureP0LogBytes        = 100 * 1000 * 1000
)

// A full ops run on blockByteCount blocks whose every cost is fixtureCostsOf's, the idle window's background on top.
func fixtureOps(blockByteCount int64) *Measurement {
	costs := fixtureCostsOf(blockByteCount)
	idleDrives := fixtureDrives(fixtureIdleBytesASecond*fixtureIdleSeconds, 0, 0)
	for _, drive := range idleDrives {
		drive.WrittenByteCount = fixtureIdleBytesASecond * fixtureIdleSeconds
	}
	measurement := &Measurement{
		Mode:           ModeOps,
		Label:          "fixture",
		Host:           "bench.example",
		Started:        time.Date(2026, time.November, 2, 0, 0, 0, 0, time.UTC),
		Ended:          time.Date(2026, time.November, 2, 3, 0, 0, 0, time.UTC),
		CommitSet:      fixtureCommitSet(),
		Settings:       map[string]string{"duration": "10m0s"},
		Fs116:          fixtureFs116(),
		ClusterId:      "meta-1",
		BlockByteCount: blockByteCount,
		DriveCount:     fixtureDriveCount,
		DataShardCount: 4,
		Idle:           &IdleWindow{Seconds: fixtureIdleSeconds, Drives: idleDrives},
	}
	background := uint64(fixtureIdleBytesASecond * fixtureOpsSeconds)
	window := func(class string, objectByteCount int64, perOp int64) *OpsResult {
		return &OpsResult{
			Class:           class,
			ObjectByteCount: objectByteCount,
			Seconds:         fixtureOpsSeconds,
			Count:           fixtureOpsCount,
			LatencyMillis:   load.Percentiles{Count: fixtureOpsCount, P50: 2, P99: 9},
			Drives:          fixtureDrives(uint64(perOp)*fixtureOpsCount+background, 3*fixtureOpsCount, fixtureOpsCount),
			RawUsageDelta:   2 * objectByteCount * fixtureOpsCount,
			InodeDelta:      2 * fixtureOpsCount,
		}
	}
	sizes := DefaultOpsSettings().ObjectByteCounts
	for _, size := range sizes {
		measurement.Ops = append(measurement.Ops, window(OpsClassCreate, size, costs.create+fixtureLargeByteCount(size, blockByteCount)))
		measurement.Ops = append(measurement.Ops, window(OpsClassOverwrite, size, costs.overwrite+fixtureLargeByteCount(size, blockByteCount)))
	}
	measurement.Ops = append(measurement.Ops, window(OpsClassGet, 4*1024, 0))
	measurement.Ops = append(measurement.Ops, window(OpsClassList, 4*1024, 0))
	for _, size := range sizes {
		measurement.Ops = append(measurement.Ops, window(OpsClassDelete, size, costs.delete))
	}
	return measurement
}

// A full mix run whose gate phase writes pushByteCount a push on its busiest drive and passes every other clause.
func fixtureMix(blockByteCount int64, pushByteCount uint64) *Measurement {
	measurement := &Measurement{
		Mode:           ModeMix,
		Label:          "fixture",
		Host:           "bench.example",
		Started:        time.Date(2026, time.November, 3, 0, 0, 0, 0, time.UTC),
		Ended:          time.Date(2026, time.November, 6, 9, 0, 0, 0, time.UTC),
		CommitSet:      fixtureCommitSet(),
		Settings:       map[string]string{"steady": "1h0m0s"},
		Fs116:          fixtureFs116(),
		ClusterId:      "meta-1",
		BlockByteCount: blockByteCount,
		DriveCount:     fixtureDriveCount,
		DataShardCount: 4,
	}
	phase := func(name string, factor float64, pushes int64, perPush uint64) *MixPhase {
		return &MixPhase{
			Name:                   name,
			Factor:                 factor,
			RefillDwpd:             0.8,
			Seconds:                fixtureGateSeconds,
			OfferedPushesPerSecond: 200 * factor,
			PushesCommitted:        pushes,
			PushFailureKinds:       map[string]int64{},
			PushLatencyMillis:      load.Percentiles{Count: pushes, P50: 6, P99: 40},
			OperationLatencyMillis: map[string]load.Percentiles{
				StoreOperationGet:    {Count: 1000, P50: 1, P99: 8},
				StoreOperationPut:    {Count: 1000, P50: 3, P99: 19},
				StoreOperationList:   {Count: 100, P50: 4, P99: 22},
				StoreOperationDelete: {Count: 100, P50: 2, P99: 12},
			},
			Drives:          fixtureDrives(perPush*uint64(pushes), 30000*fixtureGateSeconds, 3000*fixtureGateSeconds),
			PricedByteCount: fixturePricedByteCount,
			P0PricedByteCounts: map[string]int64{
				"revocation": fixtureP0RevocationBytes,
				"rotation":   fixtureP0RotationBytes,
				"log":        fixtureP0LogBytes,
			},
			P0RefillRatioMax:      0.2,
			OldestBatchSecondsMax: 25.5 * 3600,
			SeriesRead:            true,
			RecordsPerBatch:       map[string]float64{"meta-1": 1.3},
			BatchesSampled:        map[string]int{"meta-1": 512},
			RawUsageDelta:         40 * 1000 * 1000 * 1000,
			InodeDelta:            10 * 1000 * 1000,
			RawCapacityByteCount:  15360 * 1000 * 1000 * 1000,
			RawUsageByteCount:     1000 * 1000 * 1000 * 1000,
			UsedInodeCount:        200 * 1000 * 1000,
			FreeInodeCount:        7300 * 1000 * 1000,
			RestartIncluded:       true,
			EventsDriven:          append([]string{}, MixEvents...),
			EventsMissing:         []string{},
		}
	}
	measurement.Phases = []*MixPhase{
		phase(MixPhaseYearOne, 1, fixtureGatePushes/2, pushByteCount+1024),
		phase(MixPhaseTwiceYear, 2, fixtureGatePushes, pushByteCount),
		phase(MixPhaseAtTheLimit, 2.2, fixtureGatePushes*11/10, pushByteCount),
	}
	return measurement
}

// Both block sizes' runs: 1 KiB blocks at 31 KiB a push, 4 KiB at 40 KiB, every clause passing on each.
func fixtureRuns() []*Measurement {
	return []*Measurement{
		fixtureOps(1024), fixtureMix(1024, 31*1024),
		fixtureOps(4 * 1024), fixtureMix(4*1024, 40*1024),
	}
}

func fixtureTime() time.Time {
	return time.Date(2026, time.November, 7, 0, 0, 0, 0, time.UTC)
}

func failedClauses(block *BlockSizeReport) []string {
	names := []string{}
	for _, clause := range block.Clauses {
		if !clause.Pass {
			names = append(names, clause.Name)
		}
	}
	return names
}

func TestNvmeReportPassesOnAFullRunOfBothBlockSizes(t *testing.T) {
	report, err := BuildNvmeReport(fixtureRuns(), nil, fixtureTime())
	if err != nil {
		t.Fatal(err)
	}
	for _, blockByteCount := range []int64{1024, 4 * 1024} {
		block := report.Block(blockByteCount)
		if block == nil {
			t.Fatalf("no %d B block", blockByteCount)
		}
		if failed := failedClauses(block); 0 < len(failed) || !block.GatePass || 0 < len(block.Errors) {
			t.Fatalf("%d B blocks failed %v, errors %v", blockByteCount, failed, block.Errors)
		}
	}
	if !report.Pass || report.ChosenBlockByteCount != 1024 {
		t.Fatalf("chose %d (%s), not 1 KiB", report.ChosenBlockByteCount, report.Choice)
	}
	oneKiB := report.Block(1024)
	if oneKiB.PushByteCountPerDrive != 31*1024 {
		t.Fatalf("%v bytes a push, not 31 KiB", oneKiB.PushByteCountPerDrive)
	}
	if want := 30000.0 / 85000; math.Abs(oneKiB.WriteIopsShare-want) > 1e-9 {
		t.Fatalf("write share %v, not %v", oneKiB.WriteIopsShare, want)
	}
	if got := oneKiB.RecordsPerBatch["meta-1"]; got != 1.3 {
		t.Fatalf("records per batch %v", got)
	}
	var text bytes.Buffer
	if err := WriteNvmeReport(&text, report); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"verdict pass", "1 KiB blocks", "records per batch on meta-1: 1.30", "warn line"} {
		if !strings.Contains(text.String(), want) {
			t.Fatalf("the report lacks %q:\n%s", want, text.String())
		}
	}
}

// Each mutation breaks one clause of 1 KiB blocks' gate, and that clause, at least, must fail.
func TestNvmeGateEachClauseFailsAFixtureThatBreaksIt(t *testing.T) {
	cases := []struct {
		clause string
		mutate func(ops *Measurement, mix *Measurement)
	}{
		{"evidence-mix", func(ops *Measurement, mix *Measurement) { mix.Smoke = true }},
		{"evidence-ops", func(ops *Measurement, mix *Measurement) { ops.Smoke = true }},
		{"evidence-mix", func(ops *Measurement, mix *Measurement) { mix.Fs116 = nil }},
		{"evidence-mix", func(ops *Measurement, mix *Measurement) { mix.Fs116.Passed = mix.Fs116.Passed[:1] }},
		{"evidence-mix", func(ops *Measurement, mix *Measurement) {
			mix.Fs116.CommitSet = map[string]string{"sidecar": "0ther", "server": "0ther"}
		}},
		{"evidence-mix", func(ops *Measurement, mix *Measurement) { mix.Fs116.Listed = nil }},
		{"price-table", func(ops *Measurement, mix *Measurement) { ops.Ops = ops.Ops[:0] }},
		{"push-bytes", func(ops *Measurement, mix *Measurement) {
			phase := mix.Phase(MixPhaseTwiceYear)
			phase.Drives[0].WrittenByteCount = uint64(43*1024+1) * uint64(phase.PushesCommitted)
		}},
		{"write-iops", func(ops *Measurement, mix *Measurement) {
			mix.Phase(MixPhaseTwiceYear).Drives[3].WritesCompleted = 42501 * fixtureGateSeconds
		}},
		{"no-budget-refusal", func(ops *Measurement, mix *Measurement) { mix.Phase(MixPhaseTwiceYear).PushesRefusedByBudget = 1 }},
		{"no-budget-refusal", func(ops *Measurement, mix *Measurement) { mix.Phase(MixPhaseTwiceYear).BudgetShedCount = 1 }},
		{"no-fold-behind", func(ops *Measurement, mix *Measurement) { mix.Phase(MixPhaseTwiceYear).PushesFoldBehind = 1 }},
		{"no-fold-behind", func(ops *Measurement, mix *Measurement) { mix.Phase(MixPhaseTwiceYear).FoldBehindCount = 1 }},
		{"batch-age", func(ops *Measurement, mix *Measurement) {
			mix.Phase(MixPhaseTwiceYear).OldestBatchSecondsMax = 26*3600 + 1
		}},
		{"p0-page-silent", func(ops *Measurement, mix *Measurement) { mix.Phase(MixPhaseTwiceYear).P0RefillRatioMax = 1 }},
		{"operation-p99", func(ops *Measurement, mix *Measurement) {
			mix.Phase(MixPhaseTwiceYear).OperationLatencyMillis[StoreOperationPut] = load.Percentiles{Count: 1000, P99: 25.5}
		}},
		{"operation-p99", func(ops *Measurement, mix *Measurement) { mix.Phase(MixPhaseTwiceYear).OperationLatencyMillis = nil }},
		{"refill", func(ops *Measurement, mix *Measurement) { mix.Phase(MixPhaseTwiceYear).RefillDwpd = 0.768 }},
		{"whole-mix", func(ops *Measurement, mix *Measurement) {
			phase := mix.Phase(MixPhaseTwiceYear)
			phase.EventsDriven = []string{MixEventPush}
			phase.EventsMissing = []string{MixEventGkmCommit}
		}},
		{"restart", func(ops *Measurement, mix *Measurement) { mix.Phase(MixPhaseTwiceYear).RestartIncluded = false }},
		{"drives-read", func(ops *Measurement, mix *Measurement) { mix.Phase(MixPhaseTwiceYear).Drives[5].Missing = true }},
		{"series-read", func(ops *Measurement, mix *Measurement) { mix.Phase(MixPhaseTwiceYear).SeriesRead = false }},
		{"gate-phase", func(ops *Measurement, mix *Measurement) { mix.Phases = mix.Phases[:1] }},
	}
	for i, c := range cases {
		runs := fixtureRuns()
		c.mutate(runs[0], runs[1])
		report, err := BuildNvmeReport(runs, nil, fixtureTime())
		if err != nil {
			t.Fatalf("case %d (%s): %v", i, c.clause, err)
		}
		block := report.Block(1024)
		failed := failedClauses(block)
		found := false
		for _, name := range failed {
			if name == c.clause {
				found = true
			}
		}
		if !found || block.GatePass {
			t.Errorf("case %d: %s did not fail (failed %v, gate %t)", i, c.clause, failed, block.GatePass)
		}
		// 4 KiB blocks still pass and format 2 fits there at 40 KiB plus its index, so they are chosen
		if report.ChosenBlockByteCount != 4*1024 {
			t.Errorf("case %d: chose %d after 1 KiB blocks failed: %s", i, report.ChosenBlockByteCount, report.Choice)
		}
	}
}

func TestNvmeBlockSizeChoiceFollowsDTest11(t *testing.T) {
	// 1 KiB blocks fail: 4 KiB blocks pass and format 2 fits there
	runs := fixtureRuns()
	runs[1].Phase(MixPhaseTwiceYear).PushesRefusedByBudget = 3
	report, err := BuildNvmeReport(runs, nil, fixtureTime())
	if err != nil {
		t.Fatal(err)
	}
	if !report.Pass || report.ChosenBlockByteCount != 4*1024 || !report.Block(4*1024).Format2Fits {
		t.Fatalf("chose %d: %s", report.ChosenBlockByteCount, report.Choice)
	}

	// 1 KiB blocks fail, and 4 KiB blocks pass at 42 KiB a push, where format 2's index takes it past 43
	runs = []*Measurement{fixtureOps(1024), fixtureMix(1024, 31*1024), fixtureOps(4 * 1024), fixtureMix(4*1024, 42*1024)}
	runs[1].Phase(MixPhaseTwiceYear).FoldBehindCount = 1
	report, err = BuildNvmeReport(runs, nil, fixtureTime())
	if err != nil {
		t.Fatal(err)
	}
	fourKiB := report.Block(4 * 1024)
	if !fourKiB.GatePass || fourKiB.Format2Fits || report.Pass || report.ChosenBlockByteCount != 0 {
		t.Fatalf("4 KiB gate %t, format 2 fits %t at %v, chose %d: %s", fourKiB.GatePass, fourKiB.Format2Fits, fourKiB.Format2PushByteCountPerDrive, report.ChosenBlockByteCount, report.Choice)
	}
	// 42 KiB and §11.13's ~2 KiB of index on 4 KiB blocks: 43,008 + ceil(0.15 x 14,336)
	if want := float64(42*1024) + math.Ceil(0.15*14336); fourKiB.Format2PushByteCountPerDrive != want {
		t.Fatalf("format 2 projected %v, not %v", fourKiB.Format2PushByteCountPerDrive, want)
	}

	// both fail
	runs = fixtureRuns()
	runs[1].Phase(MixPhaseTwiceYear).P0RefillRatioMax = 2
	runs[3].Phase(MixPhaseTwiceYear).OldestBatchSecondsMax = 30 * 3600
	report, err = BuildNvmeReport(runs, nil, fixtureTime())
	if err != nil {
		t.Fatal(err)
	}
	if report.Pass || report.ChosenBlockByteCount != 0 {
		t.Fatalf("chose %d with both gates failing", report.ChosenBlockByteCount)
	}

	// one block size alone, passing, is still chosen by the rule
	report, err = BuildNvmeReport([]*Measurement{fixtureOps(1024), fixtureMix(1024, 31*1024)}, nil, fixtureTime())
	if err != nil {
		t.Fatal(err)
	}
	if !report.Pass || report.ChosenBlockByteCount != 1024 {
		t.Fatalf("1 KiB alone: chose %d", report.ChosenBlockByteCount)
	}
}

func TestNvmePriceTableIsTheBusiestDrivesBytesPerOperation(t *testing.T) {
	for _, blockByteCount := range []int64{1024, 4 * 1024} {
		table, fit, footprints, err := DerivePriceTable(fixtureOps(blockByteCount), DefaultReportSettings())
		if err != nil {
			t.Fatal(err)
		}
		costs := fixtureCostsOf(blockByteCount)
		want := &evsservice.PriceTable{
			BlockByteCount:       blockByteCount,
			SmallObjectByteCount: 14 * 1024,
			CreateByteCount:      costs.create,
			OverwriteByteCount:   costs.overwrite,
			DeleteByteCount:      costs.delete,
			ShardCount:           4,
		}
		if *table != *want {
			t.Fatalf("%d B blocks: %+v, not %+v", blockByteCount, table, want)
		}
		// the fixture's large objects cost what the model says, so every fit is exact
		for _, row := range fit {
			if row.Under || math.Abs(row.Error) > 1e-9 {
				t.Fatalf("%d B blocks: %s at %d off by %v", blockByteCount, row.Class, row.ObjectByteCount, row.Error)
			}
		}
		if len(footprints) != 6 || footprints[0].InodesPerOp != 2 || footprints[0].RawBytesPerOp != 2048 {
			t.Fatalf("footprints %+v", footprints[0])
		}
		// the defaults' table is what the inferred rows hold, and the measured one equals them here
		defaults, err := evsservice.DefaultPriceTable(blockByteCount)
		if err != nil {
			t.Fatal(err)
		}
		if *defaults != *table {
			t.Fatalf("the fixture's costs differ from the inferred table: %+v and %+v", defaults, table)
		}
	}
}

func TestNvmePriceTableNamesASizeTheModelUnderprices(t *testing.T) {
	ops := fixtureOps(4 * 1024)
	// 160 KiB creates write a fifth more than the model's shard
	result := ops.OpsResult(OpsClassCreate, 160*1024)
	result.Drives[0].WrittenByteCount = uint64(float64(result.Drives[0].WrittenByteCount) * 1.2)
	_, fit, _, err := DerivePriceTable(ops, DefaultReportSettings())
	if err != nil {
		t.Fatal(err)
	}
	under := []string{}
	for _, row := range fit {
		if row.Under {
			under = append(under, fmt.Sprintf("%s-%d", row.Class, row.ObjectByteCount))
		}
	}
	if len(under) != 1 || under[0] != "create-163840" {
		t.Fatalf("underpriced %v", under)
	}
}

func TestNvmePriceTableRefusesARunMissingADrive(t *testing.T) {
	ops := fixtureOps(1024)
	ops.OpsResult(OpsClassOverwrite, 4*1024).Drives[2].Missing = true
	if _, _, _, err := DerivePriceTable(ops, DefaultReportSettings()); err == nil {
		t.Fatal("priced a window with a drive unread")
	}
}

func TestNvmePriceTableFileLoadsAsEvsservicePriceTable(t *testing.T) {
	report, err := BuildNvmeReport(fixtureRuns(), nil, fixtureTime())
	if err != nil {
		t.Fatal(err)
	}
	var file bytes.Buffer
	if err := WritePriceTableFile(&file, report); err != nil {
		t.Fatal(err)
	}
	// evsservice.PriceTable loads it as it stands, the provenance beside it unread
	loaded := &evsservice.PriceTable{}
	if err := json.Unmarshal(file.Bytes(), loaded); err != nil {
		t.Fatal(err)
	}
	if *loaded != *report.PriceTable() {
		t.Fatalf("loaded %+v, not %+v", loaded, report.PriceTable())
	}
	parsed, err := ParsePriceTableFile(file.Bytes())
	if err != nil || *parsed != *loaded {
		t.Fatalf("parsed %+v, %v", parsed, err)
	}
	if !strings.Contains(file.String(), "\"source\"") || !strings.Contains(file.String(), fixtureCommitSet()["sidecar"]) {
		t.Fatalf("the file names no source:\n%s", file.String())
	}
	if _, err := ParsePriceTableFile([]byte(`{"BlockByteCount": 2048}`)); err == nil {
		t.Fatal("parsed a table for 2 KiB blocks")
	}
	if _, err := ParsePriceTableFile([]byte(`{"BlockByteCount": 4096, "SmallObjectByteCount": 14336, "CreateByteCount": 0, "OverwriteByteCount": 1, "DeleteByteCount": 1, "ShardCount": 4}`)); err == nil {
		t.Fatal("parsed a table with a zero row")
	}

	// no table leaves a report whose gate failed
	runs := fixtureRuns()
	runs[1].Phase(MixPhaseTwiceYear).PushesFoldBehind = 1
	runs[3].Phase(MixPhaseTwiceYear).PushesFoldBehind = 1
	failed, err := BuildNvmeReport(runs, nil, fixtureTime())
	if err != nil {
		t.Fatal(err)
	}
	if err := WritePriceTableFile(&bytes.Buffer{}, failed); err == nil {
		t.Fatal("wrote a price table from a failed gate")
	}
}

func TestNvmeP0WarnLineIsFourTimesItsShareAtMostHalfTheRefill(t *testing.T) {
	report, err := BuildNvmeReport(fixtureRuns(), nil, fixtureTime())
	if err != nil {
		t.Fatal(err)
	}
	block := report.Block(1024)
	busiest := float64(31*1024) * fixtureGatePushes
	k := busiest / fixturePricedByteCount
	refill := 0.8 * 1920e9 / 24
	wantShare := float64(fixtureP0RevocationBytes+fixtureP0RotationBytes+fixtureP0LogBytes) * k / refill
	if math.Abs(block.Calibration-k) > 1e-9 || math.Abs(block.P0Share-wantShare) > 1e-9 {
		t.Fatalf("k %v share %v, not %v and %v", block.Calibration, block.P0Share, k, wantShare)
	}
	if math.Abs(block.P0WarnLine-4*wantShare) > 1e-9 {
		t.Fatalf("warn line %v, not %v", block.P0WarnLine, 4*wantShare)
	}
	if math.Abs(block.P0Shares["rotation"]-float64(fixtureP0RotationBytes)*k/refill) > 1e-9 {
		t.Fatalf("rotation's share %v", block.P0Shares["rotation"])
	}

	// a P0 share past an eighth of the refill holds the line at half
	runs := fixtureRuns()
	runs[1].Phase(MixPhaseTwiceYear).P0PricedByteCounts["rotation"] = 20 * 1000 * 1000 * 1000
	report, err = BuildNvmeReport(runs, nil, fixtureTime())
	if err != nil {
		t.Fatal(err)
	}
	if line := report.Block(1024).P0WarnLine; line != 0.5 {
		t.Fatalf("warn line %v, not half the refill", line)
	}
}

func TestNvmeReportRefusesWhatItCannotJudge(t *testing.T) {
	runs := fixtureRuns()
	runs[2].CommitSet = map[string]string{"sidecar": "0ther", "server": "0ther"}
	if _, err := BuildNvmeReport(runs, nil, fixtureTime()); err == nil {
		t.Fatal("judged runs of two builds together")
	}
	runs = fixtureRuns()
	runs[2].BlockByteCount = 2048
	if _, err := BuildNvmeReport(runs, nil, fixtureTime()); err == nil {
		t.Fatal("judged a run on 2 KiB blocks")
	}
	runs = fixtureRuns()
	runs = append(runs, fixtureMix(1024, 31*1024))
	if _, err := BuildNvmeReport(runs, nil, fixtureTime()); err == nil {
		t.Fatal("judged two mix runs of one block size")
	}
	if _, err := BuildNvmeReport(nil, nil, fixtureTime()); err == nil {
		t.Fatal("judged nothing")
	}
}

func TestNvmeDirtyTreesAreNoEvidence(t *testing.T) {
	runs := fixtureRuns()
	for _, run := range runs {
		run.CommitSet = map[string]string{"sidecar": "5c0ffee+dirty", "server": "be5be5"}
		run.Fs116.CommitSet = run.CommitSet
	}
	report, err := BuildNvmeReport(runs, nil, fixtureTime())
	if err != nil {
		t.Fatal(err)
	}
	if report.Pass {
		t.Fatal("a run on a tree with changes passed")
	}
	for _, block := range report.Blocks {
		if failed := failedClauses(block); len(failed) < 2 || failed[0] != "evidence-ops" || failed[1] != "evidence-mix" {
			t.Fatalf("%d B blocks failed %v", block.BlockByteCount, failed)
		}
	}
}

func TestNvmeMeasurementFileIsReadWholeOrNotAtAll(t *testing.T) {
	data, err := fixtureMix(1024, 31*1024).Json()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseMeasurement(data)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Phase(MixPhaseTwiceYear).PushesCommitted != fixtureGatePushes {
		t.Fatalf("read back %d pushes", parsed.Phase(MixPhaseTwiceYear).PushesCommitted)
	}
	withExtra := bytes.Replace(data, []byte(`"mode": "mix",`), []byte(`"mode": "mix", "extra": 1,`), 1)
	if _, err := ParseMeasurement(withExtra); err == nil {
		t.Fatal("read a file with a field the shape lacks")
	}
	if _, err := ParseMeasurement([]byte(`{"mode": "nvme"}`)); err == nil {
		t.Fatal("read a file of an unknown mode")
	}
}

func TestFs116EvidenceMustNameThisBuildAndEveryTest(t *testing.T) {
	if err := fixtureFs116().Check(fixtureCommitSet()); err != nil {
		t.Fatal(err)
	}
	var none *Fs116Evidence
	if err := none.Check(fixtureCommitSet()); err == nil {
		t.Fatal("no evidence passed")
	}
	other := fixtureFs116()
	other.CommitSet = map[string]string{"sidecar": "0ther", "server": fixtureCommitSet()["server"]}
	if err := other.Check(fixtureCommitSet()); err == nil {
		t.Fatal("evidence of another sidecar commit passed")
	}
	missing := fixtureFs116()
	missing.Passed = missing.Passed[1:]
	if err := missing.Check(fixtureCommitSet()); err == nil {
		t.Fatal("evidence missing a pass passed")
	}
	dirty := fixtureFs116()
	dirty.CommitSet = map[string]string{"sidecar": "5c0ffee+dirty", "server": "be5be5"}
	if err := dirty.Check(dirty.CommitSet); err == nil {
		t.Fatal("evidence from a tree with changes passed")
	}
}
