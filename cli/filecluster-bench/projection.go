package main

// D-TEST-13's projection (EVSB-M10-08; §22.8's storage benchmark at M10, D-TEST-11's gate, D-TEST-17, R-EVS-24):
// format 2's drive-write cost, computed and never measured on new drives (owner decision 174), per XFS block size:
//
//	base       M2's nvme run's gate phase (report.go): format 1's measured bytes and write IOs per push on the
//	           busiest drive at 2x year one, the budget refilled at 0.8 DWPD
//	index      §22.8's index row's T3 counts (index.go): each leg's metadata writes per content push priced by M2's
//	           table for the block size and its write IOs counted by the ops run's, each class's increment its leg
//	           less the one before it, with the index on in every eligible repo, purges and staging on; a companion
//	           push is charged to the content pushes it indexes, since its leg's increment is spread over the content
//	           pushes of the leg it rides, and each class is also reported per event of its own
//	directory  the team directory's pushes per content push (D-TEST-17), and
//	drop       a drop's push, table append, job minutes and slot DELETEs per content push, each from its row's file
//	           (ProjectionRowFile), its writes counted where the row's run counted them
//
// Every priced increment is taken times the gate phase's calibration where that is above 1, since the budget
// charges priced bytes times k (§11.13's price row), so the projection never reads below what the drives measured.
// The gate is D-TEST-11's at 2x year one on the block size D-TEST-11 chose: base plus every increment at most 43
// KiB a push per drive, and the write IOs it makes at the gate phase's pushes a second at most 50% of rated IOPS.
//
// The projection reads open, computed for review and judged nothing, while M2's run chose no block size or a row's
// counts are missing, a smoke, unnamed, undrained, or §11.13's inferred figures (evsservice.PriceRows, and report.go's
// Format2IndexOperations for the index), which holds format 2's freeze (§22.10): a computed figure on inferred inputs
// is never D-TEST-13's evidence. Counts that are evidence and miss the gate fail it. After the freeze production
// confirms the projection (measured over priced bytes within 10%), which is not this file's.

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/urnetwork/sidecar/evsservice"
)

// The projection's verdicts.
const (
	ProjectionPass = "pass"
	ProjectionFail = "fail"
	ProjectionOpen = "open"
)

// The modes of a projection's row files.
const (
	ModeDirectory = "directory"
	ModeDrop      = "drop"
)

// Where a projection row's figures come from.
const (
	ProjectionSourceMeasured = "t3"
	ProjectionSourceInferred = "inferred"
)

// D-TEST-13's numbers beyond D-TEST-11's, which ReportSettings holds. DefaultProjectionSettings is their only source.
type ProjectionSettings struct {
	// The team directory's pushes per content push when no directory row is given: §19.6's ~36 pushes a member-day,
	// ~1.5% of its members' own, most on its cards ref.
	DirectoryEventsPerPush map[evsservice.PriceEvent]float64
	// A drop's per content push when no drop row is given: drops are rare, a job a repo's history now and then.
	DropEventsPerPush map[evsservice.PriceEvent]float64
	// The format-2 rows' figures the inferred rows price.
	Rows *evsservice.PriceRowSettings
}

func DefaultProjectionSettings() *ProjectionSettings {
	return &ProjectionSettings{
		DirectoryEventsPerPush: map[evsservice.PriceEvent]float64{
			evsservice.PriceEventDirectoryCardChange:    0.005,
			evsservice.PriceEventDirectoryCardRenewal:   0.008,
			evsservice.PriceEventDirectoryNoticeCreate:  0.0007,
			evsservice.PriceEventDirectoryNoticeRewrite: 0.0007,
			evsservice.PriceEventDirectoryNoticeRemove:  0.0006,
		},
		DropEventsPerPush: map[evsservice.PriceEvent]float64{
			evsservice.PriceEventDropPush:        0.000001,
			evsservice.PriceEventDropTableAppend: 0.000001,
			evsservice.PriceEventDropJobMinute:   0.00001,
			evsservice.PriceEventDropSlotDelete:  0.0001,
		},
		Rows: evsservice.DefaultPriceRowSettings(),
	}
}

// One event of a directory or drop row file.
type ProjectionRowEvent struct {
	// An evsservice.PriceEvent.
	Event string `json:"event"`
	// How many a content push of the mix brings, and whether a run measured that rate rather than the design
	// stating it.
	PerContentPush float64 `json:"per_content_push"`
	RateMeasured   bool    `json:"rate_measured"`
	// The writes of Count such events as the row's run counted them. Count zero: the event is priced by its
	// inferred row.
	Count          int64               `json:"count"`
	Operations     []*CountedOperation `json:"operations"`
	HddPutCount    int64               `json:"hdd_put_count"`
	HddDeleteCount int64               `json:"hdd_delete_count"`
}

// The directory's or a drop's events, as the projection reads them beside the index file.
type ProjectionRowFile struct {
	RowHeader
	Events []*ProjectionRowEvent `json:"events"`
}

// A directory or drop row file, strict.
func ParseProjectionRowFile(data []byte, mode string) (*ProjectionRowFile, error) {
	file := &ProjectionRowFile{}
	if err := parseRowFile(data, file, true); err != nil {
		return nil, err
	}
	if file.Mode != mode {
		return nil, fmt.Errorf("mode %q is no %s row file", file.Mode, mode)
	}
	return file, nil
}

// One increment the projection adds to the base.
type ProjectionRow struct {
	// The index class, or the directory's or drop's event.
	Name   string `json:"name"`
	Source string `json:"source"`
	// Its events per content push, and each one's priced bytes and write IOs on the busiest drive.
	EventsPerPush         float64 `json:"events_per_push"`
	ByteCountPerEvent     float64 `json:"byte_count_per_event"`
	WriteIoCountPerEvent  float64 `json:"write_io_count_per_event"`
	HddOperationsPerEvent float64 `json:"hdd_operations_per_event"`
	// What it adds to a content push, calibration included.
	ByteCountPerPush    float64 `json:"byte_count_per_push"`
	WriteIoCountPerPush float64 `json:"write_io_count_per_push"`
}

// One block size's projection.
type BlockProjection struct {
	BlockByteCount int64 `json:"block_byte_count"`
	// M2's measured base and its calibration.
	BasePushByteCount     float64          `json:"base_push_byte_count"`
	BaseWriteIoCount      float64          `json:"base_write_io_count"`
	PushesPerSecond       float64          `json:"pushes_per_second"`
	Calibration           float64          `json:"calibration"`
	Rows                  []*ProjectionRow `json:"rows"`
	PushByteCountPerDrive float64          `json:"push_byte_count_per_drive"`
	WriteIopsShare        float64          `json:"write_iops_share"`
	// Its index increment alone, per content push, by content kind where the index file says.
	IndexByteCountPerPush float64       `json:"index_byte_count_per_push"`
	Clauses               []*GateClause `json:"clauses"`
	// Every clause whose input is evidence holds, and every other one too: a block size's own verdict.
	Verdict string   `json:"verdict"`
	Errors  []string `json:"errors"`
}

// D-TEST-13's report.
type ProjectionReport struct {
	Generated time.Time         `json:"generated"`
	CommitSet map[string]string `json:"commit_set"`
	// D-TEST-11's choice, whose projection is the gate, and the reason.
	ChosenBlockByteCount int64              `json:"chosen_block_byte_count"`
	Choice               string             `json:"choice"`
	Blocks               []*BlockProjection `json:"blocks"`
	Verdict              string             `json:"verdict"`
	Reasons              []string           `json:"reasons"`
}

// The block size's projection, nil when there is none.
func (self *ProjectionReport) Block(blockByteCount int64) *BlockProjection {
	for _, block := range self.Blocks {
		if block.BlockByteCount == blockByteCount {
			return block
		}
	}
	return nil
}

// The busiest drive's write IOs per operation of one ops window, the idle window's background taken off it.
func opsWritesPerOp(result *OpsResult, idle *IdleWindow) (float64, error) {
	if result.Count <= 0 {
		return 0, fmt.Errorf("%s at %d B: no operation succeeded", result.Class, result.ObjectByteCount)
	}
	if anyMissing(result.Drives) {
		return 0, fmt.Errorf("%s at %d B: a drive's counters were not read", result.Class, result.ObjectByteCount)
	}
	idleRates := map[string]float64{}
	if idle != nil && 0 < idle.Seconds {
		for _, drive := range idle.Drives {
			if !drive.Missing {
				idleRates[drive.DriveId] = float64(drive.WritesCompleted) / idle.Seconds
			}
		}
	}
	busiest := math.Inf(-1)
	for _, drive := range result.Drives {
		if drive.Healing {
			continue
		}
		busiest = max(busiest, float64(drive.WritesCompleted)-idleRates[drive.DriveId]*result.Seconds)
	}
	if math.IsInf(busiest, -1) {
		return 0, fmt.Errorf("%s at %d B: every drive was under heal", result.Class, result.ObjectByteCount)
	}
	return max(0, busiest) / float64(result.Count), nil
}

// Write IOs per create, overwrite and delete on the busiest drive, the most any small size's window showed.
func opsWriteIoCounts(ops *Measurement, settings *ReportSettings) (map[evsservice.WriteOperationKind]float64, error) {
	kinds := map[string]evsservice.WriteOperationKind{
		OpsClassCreate:    evsservice.WriteOperationCreate,
		OpsClassOverwrite: evsservice.WriteOperationOverwrite,
		OpsClassDelete:    evsservice.WriteOperationDelete,
	}
	counts := map[evsservice.WriteOperationKind]float64{}
	for _, result := range ops.Ops {
		kind, ok := kinds[result.Class]
		if !ok || (result.Class != OpsClassDelete && settings.SmallObjectByteCount < result.ObjectByteCount) {
			continue
		}
		perOp, err := opsWritesPerOp(result, ops.Idle)
		if err != nil {
			return nil, err
		}
		counts[kind] = max(counts[kind], perOp)
	}
	for class, kind := range kinds {
		if _, ok := counts[kind]; !ok {
			return nil, fmt.Errorf("no %s measured at a small size", class)
		}
	}
	return counts, nil
}

// The counted writes as operations the table prices.
func countedWriteOperations(operations []*CountedOperation) ([]evsservice.WriteOperation, error) {
	kinds := map[string]evsservice.WriteOperationKind{
		CountedCreate:    evsservice.WriteOperationCreate,
		CountedOverwrite: evsservice.WriteOperationOverwrite,
		CountedDelete:    evsservice.WriteOperationDelete,
	}
	writes := []evsservice.WriteOperation{}
	for _, operation := range operations {
		kind, ok := kinds[operation.Kind]
		if !ok || operation.Count < 0 || operation.ObjectByteCount < 0 {
			return nil, fmt.Errorf("a counted write %+v", operation)
		}
		writes = append(writes, evsservice.WriteOperation{Kind: kind, ObjectByteCount: operation.ObjectByteCount, Count: float64(operation.Count)})
	}
	return writes, nil
}

// What writes cost one drive on table, in priced bytes and in write IOs, staged bytes riding another write's IOs.
func priceWrites(table *evsservice.PriceTable, ioCounts map[evsservice.WriteOperationKind]float64, writes []evsservice.WriteOperation) (float64, float64, error) {
	byteCount, err := table.PriceOperations(writes)
	if err != nil {
		return 0, 0, err
	}
	ios := 0.0
	for _, write := range writes {
		ios += ioCounts[write.Kind] * write.Count
	}
	return float64(byteCount), ios, nil
}

// Why a row file's header is not evidence, empty when it is.
func rowHeaderNotEvidence(header *RowHeader) []string {
	reasons := []string{}
	if header.Smoke {
		reasons = append(reasons, "it is a smoke run")
	}
	if len(header.CommitSet) == 0 {
		reasons = append(reasons, "it names no build")
	}
	for repo, commit := range header.CommitSet {
		if strings.HasSuffix(commit, "+dirty") {
			reasons = append(reasons, fmt.Sprintf("%s's tree held changes", repo))
		}
	}
	sort.Strings(reasons)
	return reasons
}

// The index file's rows on one block size: each class's increment over the leg before it, and whether they are
// evidence. With no file, one inferred row from report.go's Format2IndexOperations.
func projectIndex(index *IndexMeasurement, table *evsservice.PriceTable, ioCounts map[evsservice.WriteOperationKind]float64, calibration float64, blockByteCount int64, reportSettings *ReportSettings) ([]*ProjectionRow, []string, error) {
	if index == nil {
		byteCount, ios, err := priceWrites(table, ioCounts, reportSettings.Format2IndexOperations[blockByteCount])
		if err != nil {
			return nil, nil, err
		}
		return []*ProjectionRow{{
			Name: "index", Source: ProjectionSourceInferred, EventsPerPush: 1, ByteCountPerEvent: byteCount, WriteIoCountPerEvent: ios,
			ByteCountPerPush: byteCount * calibration, WriteIoCountPerPush: ios,
		}}, []string{"no index row: §11.13's inferred index operations, which are no evidence"}, nil
	}
	reasons := rowHeaderNotEvidence(&index.RowHeader)
	for i := range reasons {
		reasons[i] = "the index row: " + reasons[i]
	}
	off := index.Leg("off")
	if off == nil {
		return nil, append(reasons, "the index row has no off leg, so no increment can be attributed"), nil
	}
	perPush := func(leg *IndexLeg) (float64, float64, float64, error) {
		pushes := leg.contentPushCount()
		if pushes <= 0 {
			return 0, 0, 0, fmt.Errorf("leg %s committed no content push", leg.Name)
		}
		writes, err := countedWriteOperations(leg.Operations)
		if err != nil {
			return 0, 0, 0, err
		}
		byteCount, ios, err := priceWrites(table, ioCounts, writes)
		if err != nil {
			return 0, 0, 0, err
		}
		return byteCount / float64(pushes), ios / float64(pushes), float64(leg.HddPutCount+leg.HddDeleteCount) / float64(pushes), nil
	}
	for _, leg := range index.Legs {
		if leg.Failure != "" {
			reasons = append(reasons, fmt.Sprintf("the index row's %s leg failed: %s", leg.Name, leg.Failure))
		} else if !leg.Drained {
			reasons = append(reasons, fmt.Sprintf("the index row's %s leg did not drain", leg.Name))
		}
	}
	rows := []*ProjectionRow{}
	previous := off
	previousBytes, previousIos, previousHdd, err := perPush(off)
	if err != nil {
		return nil, append(reasons, "the index row: "+err.Error()), nil
	}
	for _, class := range IndexClasses {
		leg := index.Leg(class)
		if leg == nil || leg.Failure != "" {
			reasons = append(reasons, fmt.Sprintf("the index row has no %s leg run, so its increment is unknown", class))
			continue
		}
		if !slicesEqualSet(leg.Classes, append(append([]string{}, previous.Classes...), class)) {
			reasons = append(reasons, fmt.Sprintf("the index row's %s leg turned on %v, not %v and %s", class, leg.Classes, previous.Classes, class))
		}
		byteCount, ios, hdd, err := perPush(leg)
		if err != nil {
			reasons = append(reasons, "the index row: "+err.Error())
			continue
		}
		row := &ProjectionRow{
			Name:                class,
			Source:              ProjectionSourceMeasured,
			ByteCountPerPush:    (byteCount - previousBytes) * calibration,
			WriteIoCountPerPush: ios - previousIos,
		}
		events := leg.Events[IndexClassEvents[class]]
		if 0 < events {
			row.EventsPerPush = float64(events) / float64(leg.contentPushCount())
			row.ByteCountPerEvent = (byteCount - previousBytes) / row.EventsPerPush
			row.WriteIoCountPerEvent = (ios - previousIos) / row.EventsPerPush
			row.HddOperationsPerEvent = (hdd - previousHdd) / row.EventsPerPush
		} else {
			reasons = append(reasons, fmt.Sprintf("the index row's %s leg counted no %s", class, IndexClassEvents[class]))
		}
		rows = append(rows, row)
		previous, previousBytes, previousIos, previousHdd = leg, byteCount, ios, hdd
	}
	return rows, reasons, nil
}

// Whether a and b hold the same names.
func slicesEqualSet(a []string, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for _, name := range a {
		found := false
		for _, other := range b {
			if name == other {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// A directory or drop row file's rows on one block size, or, with no file, the inferred rows at the inferred rates.
func projectRowFile(file *ProjectionRowFile, mode string, inferredRates map[evsservice.PriceEvent]float64, table *evsservice.PriceTable, ioCounts map[evsservice.WriteOperationKind]float64, calibration float64, settings *ProjectionSettings) ([]*ProjectionRow, []string, error) {
	priceRows, err := evsservice.PriceRows(settings.Rows)
	if err != nil {
		return nil, nil, err
	}
	inferred := func(event evsservice.PriceEvent, rate float64) (*ProjectionRow, error) {
		priceRow, err := evsservice.PriceRowOf(priceRows, event)
		if err != nil {
			return nil, err
		}
		writes := append(append([]evsservice.WriteOperation{}, priceRow.Operations.Immediate...), priceRow.Operations.Deferred...)
		byteCount, ios, err := priceWrites(table, ioCounts, writes)
		if err != nil {
			return nil, err
		}
		return &ProjectionRow{
			Name: string(event), Source: ProjectionSourceInferred, EventsPerPush: rate, ByteCountPerEvent: byteCount, WriteIoCountPerEvent: ios,
			HddOperationsPerEvent: priceRow.HddPutCount + priceRow.HddDeleteCount,
			ByteCountPerPush:      rate * byteCount * calibration, WriteIoCountPerPush: rate * ios,
		}, nil
	}
	rows := []*ProjectionRow{}
	if file == nil {
		events := make([]string, 0, len(inferredRates))
		for event := range inferredRates {
			events = append(events, string(event))
		}
		sort.Strings(events)
		for _, event := range events {
			row, err := inferred(evsservice.PriceEvent(event), inferredRates[evsservice.PriceEvent(event)])
			if err != nil {
				return nil, nil, err
			}
			rows = append(rows, row)
		}
		return rows, []string{fmt.Sprintf("no %s row: its inferred rows at inferred rates, which are no evidence", mode)}, nil
	}
	reasons := rowHeaderNotEvidence(&file.RowHeader)
	for i := range reasons {
		reasons[i] = fmt.Sprintf("the %s row: %s", mode, reasons[i])
	}
	for _, event := range file.Events {
		if event.PerContentPush < 0 || math.IsNaN(event.PerContentPush) || math.IsInf(event.PerContentPush, 0) {
			return nil, nil, fmt.Errorf("the %s row's %s: %v a content push", mode, event.Event, event.PerContentPush)
		}
		if !event.RateMeasured {
			reasons = append(reasons, fmt.Sprintf("the %s row's %s rate is stated, not measured", mode, event.Event))
		}
		if event.Count <= 0 {
			reasons = append(reasons, fmt.Sprintf("the %s row counted no %s write", mode, event.Event))
			row, err := inferred(evsservice.PriceEvent(event.Event), event.PerContentPush)
			if err != nil {
				return nil, nil, err
			}
			rows = append(rows, row)
			continue
		}
		writes, err := countedWriteOperations(event.Operations)
		if err != nil {
			return nil, nil, fmt.Errorf("the %s row's %s: %w", mode, event.Event, err)
		}
		byteCount, ios, err := priceWrites(table, ioCounts, writes)
		if err != nil {
			return nil, nil, err
		}
		perEvent := byteCount / float64(event.Count)
		iosPerEvent := ios / float64(event.Count)
		rows = append(rows, &ProjectionRow{
			Name: event.Event, Source: ProjectionSourceMeasured, EventsPerPush: event.PerContentPush,
			ByteCountPerEvent: perEvent, WriteIoCountPerEvent: iosPerEvent,
			HddOperationsPerEvent: float64(event.HddPutCount+event.HddDeleteCount) / float64(event.Count),
			ByteCountPerPush:      event.PerContentPush * perEvent * calibration, WriteIoCountPerPush: event.PerContentPush * iosPerEvent,
		})
	}
	return rows, reasons, nil
}

// The projection's inputs.
type ProjectionInputs struct {
	// M2's nvme run: each block size's ops and mix measurements, as the report mode reads them.
	M2 []*Measurement
	// The index row's file; nil reads the inferred index operations.
	Index *IndexMeasurement
	// The directory's and a drop's row files; nil reads their inferred rows.
	Directory *ProjectionRowFile
	Drop      *ProjectionRowFile
}

// Builds D-TEST-13's projection: see the file header. It refuses inputs no projection can be computed from at all,
// none of M2's measurements; anything else it cannot judge reads open.
func BuildProjection(inputs *ProjectionInputs, reportSettings *ReportSettings, settings *ProjectionSettings, now time.Time) (*ProjectionReport, error) {
	if reportSettings == nil {
		reportSettings = DefaultReportSettings()
	}
	if settings == nil {
		settings = DefaultProjectionSettings()
	}
	if len(inputs.M2) == 0 {
		return nil, errors.New("no M2 measurement: no price table, so nothing can be projected")
	}
	nvme, err := BuildNvmeReport(inputs.M2, reportSettings, now)
	if err != nil {
		return nil, err
	}
	report := &ProjectionReport{Generated: now, CommitSet: nvme.CommitSet, ChosenBlockByteCount: nvme.ChosenBlockByteCount, Choice: nvme.Choice}
	opsByBlock := map[int64]*Measurement{}
	mixByBlock := map[int64]*Measurement{}
	for _, measurement := range inputs.M2 {
		if measurement.Mode == ModeOps {
			opsByBlock[measurement.BlockByteCount] = measurement
		} else {
			mixByBlock[measurement.BlockByteCount] = measurement
		}
	}
	for _, block := range nvme.Blocks {
		projection := &BlockProjection{BlockByteCount: block.BlockByteCount, Calibration: max(1, block.Calibration)}
		report.Blocks = append(report.Blocks, projection)
		addClause := func(name string, pass bool, detail string) {
			projection.Clauses = append(projection.Clauses, &GateClause{Name: name, Pass: pass, Detail: detail})
		}
		open := []string{}
		addClause("m2-gate", block.GatePass, map[bool]string{
			true:  "M2's nvme gate passed on these blocks",
			false: "M2's nvme gate did not pass on these blocks, so its base and price table are no evidence",
		}[block.GatePass])
		if !block.GatePass {
			open = append(open, "M2's gate did not pass on these blocks")
		}
		ops, mix := opsByBlock[block.BlockByteCount], mixByBlock[block.BlockByteCount]
		if block.PriceTable == nil || ops == nil || mix == nil || mix.Phase(reportSettings.GatePhase) == nil || block.PushByteCountPerDrive <= 0 {
			projection.Errors = append(projection.Errors, "M2's run gives no price table or no gate phase on these blocks")
			projection.Verdict = ProjectionOpen
			continue
		}
		ioCounts, err := opsWriteIoCounts(ops, reportSettings)
		if err != nil {
			projection.Errors = append(projection.Errors, fmt.Sprintf("write IOs per operation: %v", err))
			projection.Verdict = ProjectionOpen
			continue
		}
		phase := mix.Phase(reportSettings.GatePhase)
		busiest, _ := busiestDrive(phase.Drives)
		projection.BasePushByteCount = block.PushByteCountPerDrive
		if 0 < phase.Seconds {
			projection.PushesPerSecond = float64(phase.PushesCommitted) / phase.Seconds
		}
		if busiest != nil && 0 < phase.PushesCommitted {
			busiestWrites := 0.0
			for _, drive := range phase.Drives {
				if !drive.Missing && !drive.Healing {
					busiestWrites = max(busiestWrites, float64(drive.WritesCompleted))
				}
			}
			projection.BaseWriteIoCount = busiestWrites / float64(phase.PushesCommitted)
		}

		indexRows, indexReasons, err := projectIndex(inputs.Index, block.PriceTable, ioCounts, projection.Calibration, block.BlockByteCount, reportSettings)
		if err != nil {
			projection.Errors = append(projection.Errors, fmt.Sprintf("the index row: %v", err))
		}
		directoryRows, directoryReasons, err := projectRowFile(inputs.Directory, ModeDirectory, settings.DirectoryEventsPerPush, block.PriceTable, ioCounts, projection.Calibration, settings)
		if err != nil {
			projection.Errors = append(projection.Errors, fmt.Sprintf("the directory row: %v", err))
		}
		dropRows, dropReasons, err := projectRowFile(inputs.Drop, ModeDrop, settings.DropEventsPerPush, block.PriceTable, ioCounts, projection.Calibration, settings)
		if err != nil {
			projection.Errors = append(projection.Errors, fmt.Sprintf("the drop row: %v", err))
		}
		for _, row := range indexRows {
			projection.IndexByteCountPerPush += row.ByteCountPerPush
		}
		projection.Rows = append(append(append(projection.Rows, indexRows...), directoryRows...), dropRows...)
		projection.PushByteCountPerDrive = projection.BasePushByteCount
		ios := projection.BaseWriteIoCount
		for _, row := range projection.Rows {
			projection.PushByteCountPerDrive += row.ByteCountPerPush
			ios += row.WriteIoCountPerPush
		}
		projection.WriteIopsShare = ios * projection.PushesPerSecond / reportSettings.RatedWriteIops

		for _, group := range []struct {
			name    string
			reasons []string
		}{{name: "index-row", reasons: indexReasons}, {name: "directory-row", reasons: directoryReasons}, {name: "drop-row", reasons: dropReasons}} {
			addClause(group.name, len(group.reasons) == 0, func() string {
				if len(group.reasons) == 0 {
					return "T3's counts, a full run of a named build"
				}
				return strings.Join(group.reasons, "; ")
			}())
			open = append(open, group.reasons...)
		}
		pushPass := projection.PushByteCountPerDrive <= reportSettings.MaxPushByteCountPerDrive
		addClause("push-bytes", pushPass, fmt.Sprintf("%.2f KiB a push per drive projected (M2's %.2f, the index's %.2f), against %.0f KiB",
			projection.PushByteCountPerDrive/1024, projection.BasePushByteCount/1024, projection.IndexByteCountPerPush/1024, reportSettings.MaxPushByteCountPerDrive/1024))
		iopsPass := projection.WriteIopsShare <= reportSettings.MaxWriteIopsShare
		addClause("write-iops", iopsPass, fmt.Sprintf("%.1f%% of rated write IOPS projected at %.0f pushes a second, against %.0f%%",
			100*projection.WriteIopsShare, projection.PushesPerSecond, 100*reportSettings.MaxWriteIopsShare))
		switch {
		case 0 < len(projection.Errors) || 0 < len(open):
			projection.Verdict = ProjectionOpen
		case pushPass && iopsPass:
			projection.Verdict = ProjectionPass
		default:
			projection.Verdict = ProjectionFail
		}
	}

	// the gate is the projection on the block size D-TEST-11 chose
	chosen := report.Block(report.ChosenBlockByteCount)
	switch {
	case report.ChosenBlockByteCount == 0 || chosen == nil:
		report.Verdict = ProjectionOpen
		report.Reasons = append(report.Reasons, "M2's nvme run chose no block size, so there is no price table to project with: format 2's freeze holds")
	default:
		report.Verdict = chosen.Verdict
		for _, clause := range chosen.Clauses {
			if !clause.Pass {
				report.Reasons = append(report.Reasons, clause.Name+": "+clause.Detail)
			}
		}
		report.Reasons = append(report.Reasons, chosen.Errors...)
		if report.Verdict == ProjectionOpen {
			report.Reasons = append(report.Reasons, "computed, not judged: format 2's freeze holds until the projection is computed from evidence (D-TEST-13)")
		}
	}
	return report, nil
}

// The report as text, for the review: it says computed, never measured.
func WriteProjection(out io.Writer, report *ProjectionReport) error {
	b := &strings.Builder{}
	fmt.Fprintf(b, "D-TEST-13's projection of format 2's drive writes, computed, never measured on new drives, %s\n", report.Generated.UTC().Format(time.RFC3339))
	fmt.Fprintf(b, "build %s; M2's block size: %s\n", formatCommitSet(report.CommitSet), report.Choice)
	fmt.Fprintf(b, "verdict %s\n", report.Verdict)
	for _, reason := range report.Reasons {
		fmt.Fprintf(b, "  %s\n", reason)
	}
	for _, block := range report.Blocks {
		fmt.Fprintf(b, "\n%d B blocks: %s\n", block.BlockByteCount, block.Verdict)
		for _, clause := range block.Clauses {
			fmt.Fprintf(b, "  %-14s %-4s %s\n", clause.Name, map[bool]string{true: "pass", false: "fail"}[clause.Pass], clause.Detail)
		}
		for _, err := range block.Errors {
			fmt.Fprintf(b, "  error %s\n", err)
		}
		fmt.Fprintf(b, "  base %.2f KiB and %.1f write IOs a push at %.0f pushes a second, k %.3f\n", block.BasePushByteCount/1024, block.BaseWriteIoCount, block.PushesPerSecond, block.Calibration)
		for _, row := range block.Rows {
			fmt.Fprintf(b, "  %-26s %-8s %.6f a push, %9.0f B and %.2f write IOs each, %.3f HDD operations each: +%.3f KiB a push\n",
				row.Name, row.Source, row.EventsPerPush, row.ByteCountPerEvent, row.WriteIoCountPerEvent, row.HddOperationsPerEvent, row.ByteCountPerPush/1024)
		}
		fmt.Fprintf(b, "  projected %.2f KiB a push per drive, %.1f%% of rated write IOPS\n", block.PushByteCountPerDrive/1024, 100*block.WriteIopsShare)
	}
	_, err := io.WriteString(out, b.String())
	return err
}

func runProjectionMode(ctx context.Context, args []string, stdout io.Writer) (returnPass bool, returnErr error) {
	flags := flag.NewFlagSet("filecluster-bench projection", flag.ContinueOnError)
	m2Paths := stringsFlag{}
	flags.Var(&m2Paths, "m2", "an nvme run's ops or mix measurement, repeatable: each block size's of M2's run")
	indexPath := flags.String("index", "", "the index row's file; none reads the inferred index operations, which no gate takes")
	directoryPath := flags.String("directory", "", "the directory row's file; none reads its inferred rows")
	dropPath := flags.String("drop", "", "the drop row's file; none reads its inferred rows")
	outPath := flags.String("out", "", "where the text goes; stdout when empty")
	jsonPath := flags.String("json", "", "where the projection as JSON goes, if anywhere")
	if err := flags.Parse(args); err != nil {
		return false, err
	}
	inputs := &ProjectionInputs{}
	for _, path := range m2Paths {
		measurement, err := ReadMeasurement(path)
		if err != nil {
			return false, refuseStart("%v", err)
		}
		inputs.M2 = append(inputs.M2, measurement)
	}
	read := func(path string, parse func(data []byte) error) error {
		if path == "" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return refuseStart("%v", err)
		}
		if err := parse(data); err != nil {
			return refuseStart("%s: %v", path, err)
		}
		return nil
	}
	if err := read(*indexPath, func(data []byte) (err error) { inputs.Index, err = ParseIndexMeasurement(data); return err }); err != nil {
		return false, err
	}
	if err := read(*directoryPath, func(data []byte) (err error) {
		inputs.Directory, err = ParseProjectionRowFile(data, ModeDirectory)
		return err
	}); err != nil {
		return false, err
	}
	if err := read(*dropPath, func(data []byte) (err error) { inputs.Drop, err = ParseProjectionRowFile(data, ModeDrop); return err }); err != nil {
		return false, err
	}
	report, err := BuildProjection(inputs, nil, nil, time.Now())
	if err != nil {
		return false, refuseStart("%v", err)
	}
	out := stdout
	if *outPath != "" {
		file, err := os.Create(*outPath)
		if err != nil {
			return false, err
		}
		defer func() {
			if err := file.Close(); err != nil && returnErr == nil {
				returnErr = err
			}
		}()
		out = file
	}
	if err := WriteProjection(out, report); err != nil {
		return false, err
	}
	if *jsonPath != "" {
		data, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			return false, err
		}
		if err := os.WriteFile(*jsonPath, append(data, '\n'), 0o644); err != nil {
			return false, err
		}
	}
	if report.Verdict == ProjectionOpen {
		// computed, not judged: exit 2, as a run that cannot judge
		return false, &startError{err: errors.New("the projection reads open: " + strings.Join(report.Reasons, "; "))}
	}
	return report.Verdict == ProjectionPass, nil
}
