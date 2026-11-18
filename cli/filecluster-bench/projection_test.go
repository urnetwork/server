package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/sidecar/evsservice"
)

// The content pushes each fixture leg commits, by kind.
func fixtureContentPushes() map[string]int64 {
	return map[string]int64{IndexContentInRef: 750, IndexContentGitMode: 150, IndexContentStream: 100}
}

// What each class's events number in a leg that has it on.
var fixtureClassEventCounts = map[string]map[string]int64{
	IndexClassIndexed:   {IndexEventIndexedPush: 750},
	IndexClassCompanion: {IndexEventCompanionPush: 125},
	IndexClassRollup:    {IndexEventRollup: 2},
	IndexClassCut:       {IndexEventCut: 1, IndexEventIndexEpochRebuild: 3},
	IndexClassRewrite:   {IndexEventRewrite: 1},
	IndexClassMigration: {IndexEventMigratedRepo: 1},
}

// An index row's file of a full run on the fixtures' build: every leg commits fixtureContentPushes, writes the
// base's head overwrite, record and pidx/ creates and batch DELETE a content push, and each class's leg adds
// addedOverwrites[class] 1 KiB overwrites a content push over the leg before it.
func fixtureIndex(addedOverwrites map[string]float64) *IndexMeasurement {
	measurement := &IndexMeasurement{RowHeader: RowHeader{
		Mode:      ModeIndex,
		Label:     "fixture",
		Host:      "bench.example",
		Started:   time.Date(2026, time.November, 10, 0, 0, 0, 0, time.UTC),
		Ended:     time.Date(2026, time.November, 10, 7, 0, 0, 0, time.UTC),
		CommitSet: fixtureCommitSet(),
		Settings:  map[string]string{"leg": "1h0m0s"},
		ClusterId: "",
	}}
	pushes := int64(1000)
	overwrites := 0.0
	events := map[string]int64{}
	for _, request := range DefaultIndexSettings().Legs {
		if 0 < len(request.Classes) {
			class := request.Classes[len(request.Classes)-1]
			overwrites += addedOverwrites[class]
			for event, count := range fixtureClassEventCounts[class] {
				events[event] = count
			}
		}
		leg := &IndexLeg{
			Name:          request.Name,
			Classes:       append([]string{}, request.Classes...),
			Seconds:       3600,
			ContentPushes: fixtureContentPushes(),
			Events:        map[string]int64{},
			Operations: []*CountedOperation{
				{Kind: CountedCreate, ObjectByteCount: 1024, Count: pushes},
				{Kind: CountedCreate, ObjectByteCount: 4 * 1024, Count: pushes},
				{Kind: CountedDelete, ObjectByteCount: 0, Count: pushes},
				{Kind: CountedOverwrite, ObjectByteCount: 1024, Count: pushes + int64(math.Round(overwrites*float64(pushes)))},
			},
			HddPutCount: pushes / 5,
			Drained:     true,
		}
		for event, count := range events {
			leg.Events[event] = count
		}
		measurement.Legs = append(measurement.Legs, leg)
	}
	return measurement
}

// The index's increments of a format 2 that fits on 1 KiB blocks: ~4.6 KiB a push in all.
func fixtureFittingIncrements() map[string]float64 {
	return map[string]float64{
		IndexClassIndexed:   0.3,
		IndexClassCompanion: 0.1,
		IndexClassRollup:    0.01,
		IndexClassCut:       0.01,
		IndexClassRewrite:   0.005,
		IndexClassMigration: 0.02,
	}
}

// A directory or drop row file of a full run, each event measured: count events of create, overwrite and delete
// one each, at rate a content push.
func fixtureRowFile(mode string, rates map[evsservice.PriceEvent]float64) *ProjectionRowFile {
	file := &ProjectionRowFile{RowHeader: RowHeader{Mode: mode, Label: "fixture", Host: "bench.example", CommitSet: fixtureCommitSet(), Settings: map[string]string{}}}
	for event, rate := range rates {
		file.Events = append(file.Events, &ProjectionRowEvent{
			Event:          string(event),
			PerContentPush: rate,
			RateMeasured:   true,
			Count:          100,
			Operations: []*CountedOperation{
				{Kind: CountedCreate, ObjectByteCount: 2048, Count: 100},
				{Kind: CountedOverwrite, ObjectByteCount: 2048, Count: 100},
				{Kind: CountedDelete, Count: 100},
			},
		})
	}
	return file
}

// Every input of a projection that is evidence: M2's fixture runs, an index row whose format 2 fits on 1 KiB
// blocks, and measured directory and drop rows.
func fixtureProjectionInputs() *ProjectionInputs {
	settings := DefaultProjectionSettings()
	return &ProjectionInputs{
		M2:        fixtureRuns(),
		Index:     fixtureIndex(fixtureFittingIncrements()),
		Directory: fixtureRowFile(ModeDirectory, settings.DirectoryEventsPerPush),
		Drop:      fixtureRowFile(ModeDrop, settings.DropEventsPerPush),
	}
}

func projectionClause(block *BlockProjection, name string) *GateClause {
	for _, clause := range block.Clauses {
		if clause.Name == name {
			return clause
		}
	}
	return nil
}

func fixtureNow() time.Time {
	return time.Date(2026, time.November, 18, 0, 0, 0, 0, time.UTC)
}

// D-TEST-13 on evidence that fits: M2 chose 1 KiB blocks, and the base plus T3's priced increments, each class's
// leg less the one before it times k, stays under 43 KiB a push and 50% of rated IOPS, so the gate passes there.
func TestProjectionOnT3CountsThatFitPasses(t *testing.T) {
	report, err := BuildProjection(fixtureProjectionInputs(), nil, nil, fixtureNow())
	if err != nil {
		t.Fatal(err)
	}
	if report.ChosenBlockByteCount != 1024 || report.Verdict != ProjectionPass {
		t.Fatalf("chose %d, verdict %s: %v", report.ChosenBlockByteCount, report.Verdict, report.Reasons)
	}
	block := report.Block(1024)
	for _, clause := range block.Clauses {
		if !clause.Pass {
			t.Errorf("%s: %s", clause.Name, clause.Detail)
		}
	}
	nvme, err := BuildNvmeReport(fixtureRuns(), nil, fixtureNow())
	if err != nil {
		t.Fatal(err)
	}
	m2 := nvme.Block(1024)
	k := max(1, m2.Calibration)
	// each class's increment is its added overwrites a push at the measured table's overwrite, times k
	var index float64
	for _, row := range block.Rows {
		added, ok := fixtureFittingIncrements()[row.Name]
		if !ok {
			continue
		}
		if row.Source != ProjectionSourceMeasured {
			t.Errorf("%s: source %s", row.Name, row.Source)
		}
		want := math.Round(added*1000) / 1000 * float64(m2.PriceTable.OverwriteByteCount) * k
		if math.Abs(row.ByteCountPerPush-want) > 1 {
			t.Errorf("%s: %.1f B a push, want %.1f", row.Name, row.ByteCountPerPush, want)
		}
		index += row.ByteCountPerPush
	}
	if math.Abs(block.IndexByteCountPerPush-index) > 1e-6 || block.BasePushByteCount != m2.PushByteCountPerDrive {
		t.Errorf("index %.1f against %.1f; base %.1f against M2's %.1f", block.IndexByteCountPerPush, index, block.BasePushByteCount, m2.PushByteCountPerDrive)
	}
	if block.PushByteCountPerDrive <= block.BasePushByteCount+block.IndexByteCountPerPush {
		t.Errorf("the directory and drop rows added nothing: %.1f", block.PushByteCountPerDrive)
	}
	// the companion push's increment, spread over the leg's content pushes, is per companion push 8 times that
	for _, row := range block.Rows {
		if row.Name == IndexClassCompanion && math.Abs(row.ByteCountPerEvent-row.ByteCountPerPush/k*1000/125) > 1 {
			t.Errorf("a companion push: %.1f B each, %.1f a content push", row.ByteCountPerEvent, row.ByteCountPerPush)
		}
	}
	text := &bytes.Buffer{}
	if err := WriteProjection(text, report); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text.String(), "computed, never measured") || !strings.Contains(text.String(), "verdict pass") {
		t.Errorf("the text:\n%s", text.String())
	}
}

// A miss on evidence fails the gate: an index whose in-ref updates add 1.2 KiB-overwrites a push puts 1 KiB blocks
// past 43 KiB, and a delete that costs ten times its IOs puts the write IOPS past 50%.
func TestProjectionMissFailsTheGate(t *testing.T) {
	inputs := fixtureProjectionInputs()
	increments := fixtureFittingIncrements()
	increments[IndexClassIndexed] = 1.2
	inputs.Index = fixtureIndex(increments)
	report, err := BuildProjection(inputs, nil, nil, fixtureNow())
	if err != nil {
		t.Fatal(err)
	}
	if report.Verdict != ProjectionFail {
		t.Fatalf("verdict %s: %v", report.Verdict, report.Reasons)
	}
	if clause := projectionClause(report.Block(1024), "push-bytes"); clause == nil || clause.Pass {
		t.Fatalf("the push-bytes clause: %+v", clause)
	}
	if clause := projectionClause(report.Block(1024), "write-iops"); clause == nil || !clause.Pass {
		t.Fatalf("the write-iops clause: %+v", clause)
	}

	// the IOs: a delete of ten times the fixture's IOs, and the migration's leg adding 1.3 deletes a push
	inputs = fixtureProjectionInputs()
	for _, measurement := range inputs.M2 {
		for _, result := range measurement.Ops {
			if result.Class == OpsClassDelete {
				for _, drive := range result.Drives {
					drive.WritesCompleted *= 10
				}
			}
		}
	}
	for _, leg := range inputs.Index.Legs {
		if leg.Name == IndexClassMigration {
			leg.Operations = append(leg.Operations, &CountedOperation{Kind: CountedDelete, Count: 1300})
		}
	}
	report, err = BuildProjection(inputs, nil, nil, fixtureNow())
	if err != nil {
		t.Fatal(err)
	}
	block := report.Block(1024)
	if report.Verdict != ProjectionFail || projectionClause(block, "write-iops").Pass || !projectionClause(block, "push-bytes").Pass {
		t.Fatalf("verdict %s, %.1f%% of IOPS, %.1f KiB: %v", report.Verdict, 100*block.WriteIopsShare, block.PushByteCountPerDrive/1024, report.Reasons)
	}
}

// Without T3's counts the projection is computed from §11.13's inferred figures and reads open, never pass, even
// where those figures fit: format 2's freeze holds.
func TestProjectionReadsOpenOnInferredRows(t *testing.T) {
	for _, change := range []func(inputs *ProjectionInputs){
		func(inputs *ProjectionInputs) { inputs.Index = nil },
		func(inputs *ProjectionInputs) { inputs.Directory = nil },
		func(inputs *ProjectionInputs) { inputs.Drop = nil },
		func(inputs *ProjectionInputs) { inputs.Directory.Events[0].RateMeasured = false },
		func(inputs *ProjectionInputs) { inputs.Drop.Events[0].Count = 0 },
	} {
		inputs := fixtureProjectionInputs()
		change(inputs)
		report, err := BuildProjection(inputs, nil, nil, fixtureNow())
		if err != nil {
			t.Fatal(err)
		}
		if report.Verdict != ProjectionOpen {
			t.Errorf("verdict %s on inferred inputs: %v", report.Verdict, report.Reasons)
		}
		found := false
		for _, reason := range report.Reasons {
			found = found || strings.Contains(reason, "inferred") || strings.Contains(reason, "stated") || strings.Contains(reason, "counted no")
		}
		if !found {
			t.Errorf("the reasons do not say why: %v", report.Reasons)
		}
	}
	// the inferred index row is report.go's figures, priced and shown
	inputs := fixtureProjectionInputs()
	inputs.Index = nil
	report, err := BuildProjection(inputs, nil, nil, fixtureNow())
	if err != nil {
		t.Fatal(err)
	}
	if rows := report.Block(1024).Rows; len(rows) == 0 || rows[0].Source != ProjectionSourceInferred || rows[0].ByteCountPerPush <= 0 {
		t.Errorf("the inferred index row: %+v", rows)
	}
}

// An index row that is no evidence reads open: a smoke, a run naming no build, a leg undrained, failed or missing,
// a leg turning on the wrong classes, a class leg that counted none of its events.
func TestProjectionReadsOpenOnAnIndexRowThatIsNoEvidence(t *testing.T) {
	for name, change := range map[string]func(index *IndexMeasurement){
		"smoke":     func(index *IndexMeasurement) { index.Smoke = true },
		"no build":  func(index *IndexMeasurement) { index.CommitSet = map[string]string{} },
		"dirty":     func(index *IndexMeasurement) { index.CommitSet["server"] += "+dirty" },
		"undrained": func(index *IndexMeasurement) { index.Leg(IndexClassRollup).Drained = false },
		"failed":    func(index *IndexMeasurement) { index.Leg(IndexClassCut).Failure = "the load stopped" },
		"missing":   func(index *IndexMeasurement) { index.Legs = index.Legs[:len(index.Legs)-1] },
		"no off":    func(index *IndexMeasurement) { index.Legs = index.Legs[1:] },
		"classes":   func(index *IndexMeasurement) { index.Leg(IndexClassCompanion).Classes = []string{IndexClassCompanion} },
		"no events": func(index *IndexMeasurement) { index.Leg(IndexClassRewrite).Events = map[string]int64{} },
	} {
		inputs := fixtureProjectionInputs()
		change(inputs.Index)
		report, err := BuildProjection(inputs, nil, nil, fixtureNow())
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if report.Verdict != ProjectionOpen {
			t.Errorf("%s: verdict %s", name, report.Verdict)
		}
		if clause := projectionClause(report.Block(1024), "index-row"); clause == nil || clause.Pass {
			t.Errorf("%s: the index-row clause %+v", name, clause)
		}
	}
}

// With no block size M2's gate passed there is no price table to project with: open, and a projection of nothing
// is refused outright.
func TestProjectionReadsOpenWithoutM2sChoice(t *testing.T) {
	inputs := fixtureProjectionInputs()
	inputs.M2 = []*Measurement{fixtureOps(1024), fixtureMix(1024, 50*1024), fixtureOps(4 * 1024), fixtureMix(4*1024, 50*1024)}
	report, err := BuildProjection(inputs, nil, nil, fixtureNow())
	if err != nil {
		t.Fatal(err)
	}
	if report.ChosenBlockByteCount != 0 || report.Verdict != ProjectionOpen {
		t.Fatalf("chose %d, verdict %s", report.ChosenBlockByteCount, report.Verdict)
	}
	if _, err := BuildProjection(&ProjectionInputs{}, nil, nil, fixtureNow()); err == nil {
		t.Error("a projection with no M2 run was built")
	}
}

// D-TEST-11's choice is the gate: had M2 chosen 4 KiB blocks on its inferred figures, T3's counts that fit 1 KiB
// blocks but not 4 KiB fail it there.
func TestProjectionJudgesTheBlockSizeM2Chose(t *testing.T) {
	inputs := fixtureProjectionInputs()
	// 1 KiB blocks' gate misses at 44 KiB, 4 KiB's passes at 40 with room for the inferred format 2
	inputs.M2 = []*Measurement{fixtureOps(1024), fixtureMix(1024, 44*1024), fixtureOps(4 * 1024), fixtureMix(4*1024, 36*1024)}
	increments := fixtureFittingIncrements()
	increments[IndexClassIndexed] = 0.6
	inputs.Index = fixtureIndex(increments)
	report, err := BuildProjection(inputs, nil, nil, fixtureNow())
	if err != nil {
		t.Fatal(err)
	}
	if report.ChosenBlockByteCount != 4*1024 {
		t.Fatalf("chose %d: %s", report.ChosenBlockByteCount, report.Choice)
	}
	if report.Verdict != ProjectionFail || report.Block(4*1024).Verdict != ProjectionFail {
		t.Fatalf("verdict %s, 4 KiB's %s at %.1f KiB", report.Verdict, report.Block(4*1024).Verdict, report.Block(4*1024).PushByteCountPerDrive/1024)
	}
}

// The mode writes its text and JSON and exits by the verdict: 0 on a pass, 1 on a miss, 2 while open.
func TestProjectionModeExitsByItsVerdict(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, value any) string {
		path := filepath.Join(dir, name)
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	inputs := fixtureProjectionInputs()
	args := []string{}
	for i, measurement := range inputs.M2 {
		args = append(args, "-m2", write(string(rune('a'+i))+".json", measurement))
	}
	directory := write("directory.json", inputs.Directory)
	drop := write("drop.json", inputs.Drop)
	fitting := write("index.json", inputs.Index)
	increments := fixtureFittingIncrements()
	increments[IndexClassIndexed] = 1.2
	missing := write("index-miss.json", fixtureIndex(increments))
	for _, c := range []struct {
		extra []string
		code  int
	}{
		{extra: []string{"-index", fitting, "-directory", directory, "-drop", drop}, code: 0},
		{extra: []string{"-index", missing, "-directory", directory, "-drop", drop}, code: 1},
		{extra: []string{"-directory", directory, "-drop", drop}, code: 2},
	} {
		out := filepath.Join(dir, "projection.txt")
		jsonOut := filepath.Join(dir, "projection.json")
		stderr := &bytes.Buffer{}
		code := run(context.Background(), append(append(append([]string{"projection"}, args...), c.extra...), "-out", out, "-json", jsonOut), &bytes.Buffer{}, stderr)
		if code != c.code {
			t.Errorf("%v: exit %d, want %d: %s", c.extra, code, c.code, stderr.String())
		}
		text, err := os.ReadFile(out)
		if err != nil || !strings.Contains(string(text), "D-TEST-13's projection") {
			t.Errorf("%v: the text %q, %v", c.extra, text, err)
		}
		report := &ProjectionReport{}
		data, err := os.ReadFile(jsonOut)
		if err != nil || json.Unmarshal(data, report) != nil || len(report.Blocks) != 2 {
			t.Errorf("%v: the JSON %v", c.extra, err)
		}
	}
	// a row file of the wrong mode is refused before anything is computed
	code := run(context.Background(), append(append([]string{"projection"}, args...), "-directory", drop), &bytes.Buffer{}, &bytes.Buffer{})
	if code != 2 {
		t.Errorf("a drop file as the directory's: exit %d", code)
	}
	var start *startError
	if _, err := runProjectionMode(context.Background(), []string{}, &bytes.Buffer{}); !errors.As(err, &start) {
		t.Errorf("no M2 measurement: %v", err)
	}
}
