package main

// The measurement files the ops and mix modes write, and D-TEST-11's report the report mode makes of them (§22.8's
// nvme row; R-EVS-19, R-OPS-12, FI-14).
//
// A measurement file is one mode's run on one XFS block size: the ops mode's each operation class alone, from which
// the price table comes, or the mix mode's phases, from which the gate comes. The report takes the files of both
// block sizes and, per block size:
//
//	price table  each cost row the busiest drive's device bytes per operation at the small sizes (1, 4 and 10 KiB),
//	             the largest of them, the idle window's background taken off; a larger object's added shard is the
//	             model's (evsservice.PriceTable), checked against the 16, 64 and 160 KiB runs and each fit reported
//	gate         at 2x year one, the budget refilled at 0.8 DWPD for the run: at most 43 KiB per push per drive (the
//	             busiest drive's bytes over the pushes committed, so the P2 work a push causes is in it), writes
//	             within 50% of rated IOPS, no push refused by the budget, no FOLD_BEHIND, every batch under 26 h,
//	             P0's page silent and each store operation's p99 within 25 ms (§11.13's M2 exit), on a run that is
//	             evidence: not a smoke, FS-116 passed on its commit set, the whole mix with a restart
//	P0           each kind's share of the hour's refill, priced bytes times the phase's calibration, and §19.8's warn
//	             line at four times their sum, at most half the refill
//	format 2     the gate phase's bytes per push plus §11.13's per-push index operations priced by this table, which
//	             must fit too before 4 KiB blocks may be chosen
//
// and then the block size: 1 KiB when its gate passes, else 4 KiB when its gate passes and format 2's projection fits
// there, else none. The gate passes only with a block size chosen. The price table file is the chosen block size's
// evsservice.PriceTable as JSON, its keys the struct's own field names, so json.Unmarshal into evsservice.PriceTable
// loads it and leaves the provenance beside it unread.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/urnetwork/sidecar/evsservice"
	"github.com/urnetwork/sidecar/sidecartest/load"
)

// The modes a measurement file comes from.
const (
	ModeOps = "ops"
	ModeMix = "mix"
)

// The operation classes the ops mode runs alone (§22.8's nvme row).
const (
	OpsClassCreate    = "create"
	OpsClassOverwrite = "overwrite"
	OpsClassDelete    = "delete"
	OpsClassList      = "list"
	OpsClassGet       = "get"
)

// The mix phases (§22.8's nvme row): 1x and 2x year one, and the budget's limit.
const (
	MixPhaseYearOne    = "1x"
	MixPhaseTwiceYear  = "2x"
	MixPhaseAtTheLimit = "limit"
)

// The store operations whose p99 the mix row bounds.
const (
	StoreOperationGet    = "get"
	StoreOperationPut    = "put"
	StoreOperationList   = "list"
	StoreOperationDelete = "delete"
	StoreOperationHead   = "head"
)

// What FS-116 passing on a commit set looks like, as the fs116 mode records it.
type Fs116Evidence struct {
	// Each repository's commit the run read, none of them "+dirty".
	CommitSet map[string]string `json:"commit_set"`
	// The FS-116 tests go test listed, and those it answered pass.
	Listed []string  `json:"listed"`
	Passed []string  `json:"passed"`
	Time   time.Time `json:"time"`
	// The command it ran, for the record.
	Command string `json:"command"`
}

// Why the evidence does not show FS-116 passing on commitSet, or nil.
func (self *Fs116Evidence) Check(commitSet map[string]string) error {
	if self == nil {
		return errors.New("no FS-116 evidence")
	}
	if len(self.Listed) == 0 {
		return errors.New("the FS-116 run listed no test")
	}
	for _, name := range self.Listed {
		if !slices.Contains(self.Passed, name) {
			return fmt.Errorf("FS-116's %s did not pass", name)
		}
	}
	if len(self.CommitSet) == 0 {
		return errors.New("the FS-116 evidence names no commit set")
	}
	for repo, commit := range self.CommitSet {
		if strings.HasSuffix(commit, "+dirty") {
			return fmt.Errorf("FS-116 ran on %s's tree with changes", repo)
		}
	}
	if !commitSetsEqual(self.CommitSet, commitSet) {
		return fmt.Errorf("FS-116 passed on %s, not this run's %s", formatCommitSet(self.CommitSet), formatCommitSet(commitSet))
	}
	return nil
}

func commitSetsEqual(a map[string]string, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for repo, commit := range a {
		if b[repo] != commit {
			return false
		}
	}
	return true
}

func formatCommitSet(commitSet map[string]string) string {
	repos := []string{}
	for repo := range commitSet {
		repos = append(repos, repo)
	}
	sort.Strings(repos)
	parts := []string{}
	for _, repo := range repos {
		parts = append(parts, repo+"@"+commitSet[repo])
	}
	return "{" + strings.Join(parts, " ") + "}"
}

// One drive's counters over a window: what §19.6's counters row exports, and the host's XFS and SMART readings
// where a host stats command read them.
type DriveDelta struct {
	// pool-set-disk, from admin info.
	DriveId string `json:"drive_id"`
	// /sys/block stat field 7 in bytes, and fields 5 and 1.
	WrittenByteCount uint64 `json:"written_byte_count"`
	WritesCompleted  uint64 `json:"writes_completed"`
	ReadsCompleted   uint64 `json:"reads_completed"`
	// The XFS log's bytes and the file bytes written through XFS, nil where no host stats were read.
	XfsLogByteCount  *uint64 `json:"xfs_log_byte_count,omitempty"`
	XfsFileByteCount *uint64 `json:"xfs_file_byte_count,omitempty"`
	// Admin info showed it under heal at either end, so the budget's busiest drive passes over it.
	Healing bool `json:"healing"`
	// Its counters were not read at either end: the window measures nothing on it.
	Missing bool `json:"missing"`
}

// One drive's SMART log (§19.6's counters row) at a run's start or end.
type DriveSmart struct {
	DriveId string `json:"drive_id"`
	// In units of 1000 x 512 B.
	DataUnitsWritten uint64 `json:"data_units_written"`
	PercentageUsed   uint64 `json:"percentage_used"`
	AvailableSpare   uint64 `json:"available_spare"`
	MediaErrors      uint64 `json:"media_errors"`
}

// A window with no load, whose bytes are the cluster's background: the scanner, heals and the sentinel.
type IdleWindow struct {
	Seconds float64       `json:"seconds"`
	Drives  []*DriveDelta `json:"drives"`
}

// One operation class run alone.
type OpsResult struct {
	Class string `json:"class"`
	// The objects' size, for create, overwrite and delete, and the listed or read objects' for list and get.
	ObjectByteCount int64 `json:"object_byte_count"`
	// The measured window.
	Seconds float64 `json:"seconds"`
	// Operations that succeeded in it, and those that failed beside them.
	Count  int64 `json:"count"`
	Failed int64 `json:"failed"`
	// The successes' latency, from each operation's due time.
	LatencyMillis load.Percentiles `json:"latency_ms"`
	Drives        []*DriveDelta    `json:"drives"`
	// The cluster's raw bytes and inodes over the window, from admin info: an object class's bytes and inodes.
	RawUsageDelta int64 `json:"raw_usage_delta"`
	InodeDelta    int64 `json:"inode_delta"`
}

// One mix phase's steady hour.
type MixPhase struct {
	Name string `json:"name"`
	// The phase's pushes a second as a multiple of year one's.
	Factor float64 `json:"factor"`
	// The budget's refill for the run, in drive writes a day: §22.8's 0.8.
	RefillDwpd float64 `json:"refill_dwpd"`
	// The steady window.
	Seconds                float64 `json:"seconds"`
	OfferedPushesPerSecond float64 `json:"offered_pushes_per_second"`
	PushesCommitted        int64   `json:"pushes_committed"`
	// Pushes the write budget refused (429 RESOURCE_EXHAUSTED), those at FOLD_BEHIND, and every other failure.
	PushesRefusedByBudget int64            `json:"pushes_refused_by_budget"`
	PushesFoldBehind      int64            `json:"pushes_fold_behind"`
	PushesFailed          int64            `json:"pushes_failed"`
	PushFailureKinds      map[string]int64 `json:"push_failure_kinds"`
	PushLatencyMillis     load.Percentiles `json:"push_latency_ms"`
	// Each store operation's latency over the phase, by StoreOperation.
	OperationLatencyMillis map[string]load.Percentiles `json:"operation_latency_ms"`
	Drives                 []*DriveDelta               `json:"drives"`
	// The stack's own series over the phase: the budget's sheds and the refusals at FOLD_BEHIND it counted, the
	// priced bytes every class issued, P0's by kind, and the largest P0 refill ratio and oldest batch any sample saw.
	BudgetShedCount       int64            `json:"budget_shed_count"`
	FoldBehindCount       int64            `json:"fold_behind_count"`
	PricedByteCount       int64            `json:"priced_byte_count"`
	P0PricedByteCounts    map[string]int64 `json:"p0_priced_byte_counts"`
	P0RefillRatioMax      float64          `json:"p0_refill_ratio_max"`
	OldestBatchSecondsMax float64          `json:"oldest_batch_seconds_max"`
	// Whether every series above was read; a phase that read none of them cannot pass.
	SeriesRead bool `json:"series_read"`
	// Records per batch on each metadata cluster, from the batches sampled out of the phase's minutes.
	RecordsPerBatch map[string]float64 `json:"records_per_batch"`
	BatchesSampled  map[string]int     `json:"batches_sampled"`
	// Each cluster's raw bytes and inodes over the phase, and its raw capacity and free inodes at its end.
	RawUsageDelta        int64  `json:"raw_usage_delta"`
	InodeDelta           int64  `json:"inode_delta"`
	RawCapacityByteCount uint64 `json:"raw_capacity_byte_count"`
	RawUsageByteCount    uint64 `json:"raw_usage_byte_count"`
	UsedInodeCount       uint64 `json:"used_inode_count"`
	FreeInodeCount       uint64 `json:"free_inode_count"`
	// A restart with every reset incomplete ran inside the phase (§22.8's mix row).
	RestartIncluded bool `json:"restart_included"`
	// The event classes of §11.13's mix the target drove, and those it could not.
	EventsDriven  []string `json:"events_driven"`
	EventsMissing []string `json:"events_missing"`
}

// One run's measurements: see the file header.
type Measurement struct {
	Mode  string `json:"mode"`
	Label string `json:"label"`
	Host  string `json:"host"`
	// A run too small or too short, or on a cluster unlike the PM9A3 set: never evidence.
	Smoke   bool      `json:"smoke"`
	Started time.Time `json:"started"`
	Ended   time.Time `json:"ended"`
	// What the run says of itself: a counter unread, a host that lacked something.
	Notes     []string          `json:"notes"`
	CommitSet map[string]string `json:"commit_set"`
	// The effective settings the run used, features on (R-TEST-2).
	Settings map[string]string `json:"settings"`
	// The FS-116 evidence it started under; nil on a smoke.
	Fs116 *Fs116Evidence `json:"fs116"`

	ClusterId string `json:"cluster_id"`
	// The XFS block size the cluster's drives were formatted with, as the operator declared it.
	BlockByteCount int64 `json:"block_byte_count"`
	DriveCount     int   `json:"drive_count"`
	// The data shards an object's bytes spread over: 4 at EC:4 on 8 drives.
	DataShardCount int `json:"data_shard_count"`

	SmartBefore []*DriveSmart `json:"smart_before"`
	SmartAfter  []*DriveSmart `json:"smart_after"`
	Idle        *IdleWindow   `json:"idle"`
	Ops         []*OpsResult  `json:"ops"`
	Phases      []*MixPhase   `json:"phases"`
}

// A measurement file, strict: a field the shape does not hold refuses it, so a file from another version of the
// harness is read whole or not at all.
func ParseMeasurement(data []byte) (*Measurement, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	measurement := &Measurement{}
	if err := decoder.Decode(measurement); err != nil {
		return nil, err
	}
	if measurement.Mode != ModeOps && measurement.Mode != ModeMix {
		return nil, fmt.Errorf("mode %q", measurement.Mode)
	}
	return measurement, nil
}

func ReadMeasurement(path string) (*Measurement, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	measurement, err := ParseMeasurement(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return measurement, nil
}

func (self *Measurement) Json() ([]byte, error) {
	return json.MarshalIndent(self, "", "  ")
}

// The ops result of class at objectByteCount, nil when the run has none.
func (self *Measurement) OpsResult(class string, objectByteCount int64) *OpsResult {
	for _, result := range self.Ops {
		if result.Class == class && result.ObjectByteCount == objectByteCount {
			return result
		}
	}
	return nil
}

// The phase named name, nil when the run has none.
func (self *Measurement) Phase(name string) *MixPhase {
	for _, phase := range self.Phases {
		if phase.Name == name {
			return phase
		}
	}
	return nil
}

// D-TEST-11's numbers (§22.8's nvme and mix rows, §11.13, §19.6, §19.8). DefaultReportSettings is their only source.
type ReportSettings struct {
	// The phase the gate reads: 2x year one.
	GatePhase string
	// The refill the gate phase must have run at, in drive writes a day: 0.8, not production's 0.768.
	GateRefillDwpd float64
	// The most bytes the busiest drive may write per push: 43 KiB, 0.8 DWPD at 400 pushes a second.
	MaxPushByteCountPerDrive float64
	// The PM9A3's rated random-write IOPS (4K), and the share the busiest drive's writes may take: 85,000 and 50%.
	RatedWriteIops    float64
	MaxWriteIopsShare float64
	// The PM9A3's rated random-read IOPS, reported against, not gated: 800,000.
	RatedReadIops float64
	// The most any store operation's p99 may be: 25 ms.
	MaxOperationP99 time.Duration
	// The oldest a batch may get: 26 h.
	MaxBatchAge time.Duration
	// P0's measured bytes over the last hour against the refill at which §19.8 pages: 1.
	P0PageRatio float64
	// §19.8's P0 warn line: this many times P0's hourly share of the refill, at most P0WarnMaxShare of it: 4 and 0.5.
	P0WarnFactor   float64
	P0WarnMaxShare float64
	// R-OPS-12's drive, 1.92 TB, for refills and drive writes a day.
	DriveByteCount int64
	// Objects up to this size cost their row as it stands (evsservice.PriceTable): 14 KiB, §11.13's.
	SmallObjectByteCount int64
	// How far a measured cost may sit above its priced one before the fit is named in the report: 10%.
	PriceFitTolerance float64
	// Year two's pushes a second on one set, for its drive writes a day: 2,000 (§19.6).
	YearTwoPushesPerSecond float64
	// §19.9's lines a cluster stops taking repos at, for the projected fill: 80% raw and 70% inodes.
	FillRawShare   float64
	FillInodeShare float64
	// The index operations format 2 adds to a push, by XFS block size (D-TEST-11's projection: format 1's measured
	// table applied to §11.13's per-push index operations, the index on in every eligible repo). The defaults hold
	// §11.13's inferred figures, a share of a small overwrite each: ~4 KiB a push on 1 KiB blocks, one companion
	// push a minute per writer, and ~2 KiB on 4 KiB blocks, one every 5 minutes. EVSB-M10-08 replaces them with the
	// M10 index row's T3 counts (D-TEST-13).
	Format2IndexOperations map[int64][]evsservice.WriteOperation
}

func DefaultReportSettings() *ReportSettings {
	return &ReportSettings{
		GatePhase:                MixPhaseTwiceYear,
		GateRefillDwpd:           0.8,
		MaxPushByteCountPerDrive: 43 * 1024,
		RatedWriteIops:           85000,
		MaxWriteIopsShare:        0.5,
		RatedReadIops:            800000,
		MaxOperationP99:          25 * time.Millisecond,
		MaxBatchAge:              26 * time.Hour,
		P0PageRatio:              1,
		P0WarnFactor:             4,
		P0WarnMaxShare:           0.5,
		DriveByteCount:           1920 * 1000 * 1000 * 1000,
		SmallObjectByteCount:     14 * 1024,
		PriceFitTolerance:        0.1,
		YearTwoPushesPerSecond:   2000,
		FillRawShare:             0.8,
		FillInodeShare:           0.7,
		Format2IndexOperations: map[int64][]evsservice.WriteOperation{
			1024:     {{Kind: evsservice.WriteOperationOverwrite, ObjectByteCount: 1024, Count: 0.38}},
			4 * 1024: {{Kind: evsservice.WriteOperationOverwrite, ObjectByteCount: 1024, Count: 0.15}},
		},
	}
}

// The refill in drive bytes an hour at dwpd.
func (self *ReportSettings) refillByteCountPerHour(dwpd float64) float64 {
	return dwpd * float64(self.DriveByteCount) / 24
}

// One of the gate's clauses, as the report states it.
type GateClause struct {
	Name   string `json:"name"`
	Pass   bool   `json:"pass"`
	Detail string `json:"detail"`
}

// A cost the price table holds against what the ops run measured.
type PriceFitRow struct {
	Class           string  `json:"class"`
	ObjectByteCount int64   `json:"object_byte_count"`
	MeasuredPerOp   float64 `json:"measured_per_op"`
	PricedPerOp     int64   `json:"priced_per_op"`
	// (measured - priced) / priced
	Error float64 `json:"error"`
	// Measured past priced by more than the tolerance: the table underprices this size.
	Under bool `json:"under"`
}

// What one object class takes on the cluster, from the ops runs' admin info.
type ObjectFootprint struct {
	ObjectByteCount int64   `json:"object_byte_count"`
	RawBytesPerOp   float64 `json:"raw_bytes_per_op"`
	InodesPerOp     float64 `json:"inodes_per_op"`
}

// One phase's figures, for every phase the mix ran.
type PhaseSummary struct {
	Name                   string  `json:"name"`
	OfferedPushesPerSecond float64 `json:"offered_pushes_per_second"`
	PushesCommitted        int64   `json:"pushes_committed"`
	PushByteCountPerDrive  float64 `json:"push_byte_count_per_drive"`
	BusiestDriveDwpd       float64 `json:"busiest_drive_dwpd"`
	WriteIops              float64 `json:"write_iops"`
	ReadIops               float64 `json:"read_iops"`
	// Days until the cluster reaches §19.9's raw or inode line at the phase's growth; zero when it did not grow.
	FillDays float64 `json:"fill_days"`
}

// One block size's report.
type BlockSizeReport struct {
	BlockByteCount int64                  `json:"block_byte_count"`
	PriceTable     *evsservice.PriceTable `json:"price_table"`
	PriceFit       []*PriceFitRow         `json:"price_fit"`
	Footprints     []*ObjectFootprint     `json:"footprints"`
	Phases         []*PhaseSummary        `json:"phases"`
	Clauses        []*GateClause          `json:"clauses"`
	GatePass       bool                   `json:"gate_pass"`
	// The gate phase's figures.
	PushByteCountPerDrive float64 `json:"push_byte_count_per_drive"`
	WriteIopsShare        float64 `json:"write_iops_share"`
	// measured over priced bytes in the gate phase; 1 where nothing was priced
	Calibration float64 `json:"calibration"`
	// Each P0 kind's measured bytes as a share of the gate phase's hourly refill, their sum, and §19.8's warn line.
	P0Shares        map[string]float64 `json:"p0_shares"`
	P0Share         float64            `json:"p0_share"`
	P0WarnLine      float64            `json:"p0_warn_line"`
	RecordsPerBatch map[string]float64 `json:"records_per_batch"`
	// Year two's drive writes a day on one set at the gate phase's bytes per push.
	YearTwoDwpd float64 `json:"year_two_dwpd"`
	// Format 2's projected bytes per push and whether they fit the gate.
	Format2PushByteCountPerDrive float64 `json:"format2_push_byte_count_per_drive"`
	Format2Fits                  bool    `json:"format2_fits"`
	// What kept this block size from being judged, if anything.
	Errors []string `json:"errors"`
}

// D-TEST-11's report.
type NvmeReport struct {
	Generated time.Time          `json:"generated"`
	CommitSet map[string]string  `json:"commit_set"`
	Blocks    []*BlockSizeReport `json:"blocks"`
	// The block size chosen, 0 for none, and why.
	ChosenBlockByteCount int64  `json:"chosen_block_byte_count"`
	Choice               string `json:"choice"`
	Pass                 bool   `json:"pass"`
}

// The block size's report, nil when there is none.
func (self *NvmeReport) Block(blockByteCount int64) *BlockSizeReport {
	for _, block := range self.Blocks {
		if block.BlockByteCount == blockByteCount {
			return block
		}
	}
	return nil
}

// The chosen block size's price table, nil when none was chosen.
func (self *NvmeReport) PriceTable() *evsservice.PriceTable {
	if block := self.Block(self.ChosenBlockByteCount); block != nil {
		return block.PriceTable
	}
	return nil
}

// Builds the report from the ops and mix measurements of each block size. It refuses inputs it cannot judge at
// all, measurements whose commit sets differ or a block size missing a mode; a block size it can judge but whose
// run is no evidence fails its evidence clause instead.
func BuildNvmeReport(measurements []*Measurement, settings *ReportSettings, now time.Time) (*NvmeReport, error) {
	if settings == nil {
		settings = DefaultReportSettings()
	}
	if len(measurements) == 0 {
		return nil, errors.New("no measurement")
	}
	report := &NvmeReport{Generated: now, CommitSet: measurements[0].CommitSet}
	blockOps := map[int64]*Measurement{}
	blockMix := map[int64]*Measurement{}
	for _, measurement := range measurements {
		if !commitSetsEqual(measurement.CommitSet, report.CommitSet) {
			return nil, fmt.Errorf("measurements on %s and %s: one build only", formatCommitSet(report.CommitSet), formatCommitSet(measurement.CommitSet))
		}
		if measurement.BlockByteCount != 1024 && measurement.BlockByteCount != 4*1024 {
			return nil, fmt.Errorf("a measurement on %d B blocks: XFS blocks are 1 or 4 KiB (§19.6)", measurement.BlockByteCount)
		}
		into := blockOps
		if measurement.Mode == ModeMix {
			into = blockMix
		}
		if _, ok := into[measurement.BlockByteCount]; ok {
			return nil, fmt.Errorf("two %s measurements on %d B blocks", measurement.Mode, measurement.BlockByteCount)
		}
		into[measurement.BlockByteCount] = measurement
	}
	blockByteCounts := []int64{}
	for blockByteCount := range blockOps {
		blockByteCounts = append(blockByteCounts, blockByteCount)
	}
	for blockByteCount := range blockMix {
		if !slices.Contains(blockByteCounts, blockByteCount) {
			blockByteCounts = append(blockByteCounts, blockByteCount)
		}
	}
	slices.Sort(blockByteCounts)
	for _, blockByteCount := range blockByteCounts {
		report.Blocks = append(report.Blocks, buildBlockSizeReport(blockByteCount, blockOps[blockByteCount], blockMix[blockByteCount], settings))
	}

	// D-TEST-11: 1 KiB blocks, or 4 KiB only if format 2's projected mix also fits there
	oneKiB := report.Block(1024)
	fourKiB := report.Block(4 * 1024)
	switch {
	case oneKiB != nil && oneKiB.GatePass:
		report.ChosenBlockByteCount = 1024
		report.Choice = "1 KiB blocks: their gate passed (D-TEST-11's first choice)"
	case fourKiB != nil && fourKiB.GatePass && fourKiB.Format2Fits:
		report.ChosenBlockByteCount = 4 * 1024
		report.Choice = fmt.Sprintf("4 KiB blocks: their gate passed and format 2's projection fits there at %.1f KiB a push, while 1 KiB blocks' gate did not pass", fourKiB.Format2PushByteCountPerDrive/1024)
	case fourKiB != nil && fourKiB.GatePass:
		report.Choice = fmt.Sprintf("none: 1 KiB blocks' gate did not pass, and 4 KiB blocks' passed but format 2's projection, %.1f KiB a push, does not fit there", fourKiB.Format2PushByteCountPerDrive/1024)
	default:
		report.Choice = "none: no block size's gate passed"
	}
	report.Pass = report.ChosenBlockByteCount != 0
	return report, nil
}

// One block size's price table, gate and outputs.
func buildBlockSizeReport(blockByteCount int64, ops *Measurement, mix *Measurement, settings *ReportSettings) *BlockSizeReport {
	block := &BlockSizeReport{BlockByteCount: blockByteCount, Calibration: 1}
	addClause := func(name string, pass bool, detail string) {
		block.Clauses = append(block.Clauses, &GateClause{Name: name, Pass: pass, Detail: detail})
	}

	// evidence: each run a full run of this build, FS-116 passed on it first
	evidence := func(measurement *Measurement, mode string) {
		if measurement == nil {
			addClause("evidence-"+mode, false, fmt.Sprintf("no %s measurement on these blocks", mode))
			return
		}
		reasons := []string{}
		if measurement.Smoke {
			reasons = append(reasons, "it is a smoke run, which proves nothing")
		}
		if err := measurement.Fs116.Check(measurement.CommitSet); err != nil {
			reasons = append(reasons, err.Error())
		}
		for repo, commit := range measurement.CommitSet {
			if strings.HasSuffix(commit, "+dirty") {
				reasons = append(reasons, fmt.Sprintf("%s's tree held changes", repo))
			}
		}
		if len(reasons) == 0 {
			addClause("evidence-"+mode, true, fmt.Sprintf("a full %s run of %s after FS-116 passed on it", mode, formatCommitSet(measurement.CommitSet)))
		} else {
			sort.Strings(reasons)
			addClause("evidence-"+mode, false, strings.Join(reasons, "; "))
		}
	}
	evidence(ops, ModeOps)
	evidence(mix, ModeMix)

	if ops != nil {
		table, fit, footprints, err := DerivePriceTable(ops, settings)
		if err != nil {
			block.Errors = append(block.Errors, fmt.Sprintf("price table: %v", err))
		} else {
			block.PriceTable = table
			block.PriceFit = fit
			block.Footprints = footprints
		}
	}
	addClause("price-table", block.PriceTable != nil, func() string {
		if block.PriceTable == nil {
			return "no price table could be measured on these blocks"
		}
		under := []string{}
		for _, row := range block.PriceFit {
			if row.Under {
				under = append(under, fmt.Sprintf("%s at %d B (%.0f%% over)", row.Class, row.ObjectByteCount, 100*row.Error))
			}
		}
		if 0 < len(under) {
			// k carries a constant error; a size the model underprices is named for the table's next revision
			return "measured; the model underprices " + strings.Join(under, ", ")
		}
		return "measured, every size within the fit's tolerance"
	}())

	if mix == nil {
		addClause("gate-phase", false, "no mix measurement on these blocks")
		block.GatePass = false
		return block
	}
	for _, phase := range mix.Phases {
		block.Phases = append(block.Phases, summarizePhase(phase, settings))
	}
	phase := mix.Phase(settings.GatePhase)
	if phase == nil {
		addClause("gate-phase", false, fmt.Sprintf("no %s phase", settings.GatePhase))
		return block
	}
	addClause("gate-phase", true, fmt.Sprintf("%s: %.0f pushes a second offered for %.0f s", phase.Name, phase.OfferedPushesPerSecond, phase.Seconds))

	missingEvents := phase.EventsMissing
	addClause("whole-mix", len(missingEvents) == 0 && 0 < len(phase.EventsDriven), func() string {
		if 0 < len(missingEvents) {
			return "the mix lacked " + strings.Join(missingEvents, ", ")
		}
		if len(phase.EventsDriven) == 0 {
			return "the phase names no event it drove"
		}
		return "every event class of §11.13's mix was driven"
	}())
	addClause("restart", phase.RestartIncluded, map[bool]string{
		true:  "a restart with every reset incomplete ran in the phase",
		false: "no restart ran in the phase (§22.8's mix row)",
	}[phase.RestartIncluded])
	addClause("refill", phase.RefillDwpd == settings.GateRefillDwpd, fmt.Sprintf("the budget refilled at %.3f DWPD for the run, the gate's %.3f", phase.RefillDwpd, settings.GateRefillDwpd))

	busiest, busiestOk := busiestDrive(phase.Drives)
	drivesRead := busiestOk && !anyMissing(phase.Drives)
	addClause("drives-read", drivesRead, map[bool]string{
		true:  fmt.Sprintf("every one of the %d drives' counters read", len(phase.Drives)),
		false: "a drive's counters were not read, so no busiest drive can be named",
	}[drivesRead])
	if drivesRead && 0 < phase.PushesCommitted {
		block.PushByteCountPerDrive = float64(busiest.WrittenByteCount) / float64(phase.PushesCommitted)
	}
	addClause("push-bytes", drivesRead && 0 < phase.PushesCommitted && block.PushByteCountPerDrive <= settings.MaxPushByteCountPerDrive,
		fmt.Sprintf("%.2f KiB a push on the busiest drive over %d pushes, against %.0f KiB", block.PushByteCountPerDrive/1024, phase.PushesCommitted, settings.MaxPushByteCountPerDrive/1024))

	writeIops := 0.0
	if drivesRead && 0 < phase.Seconds {
		for _, drive := range phase.Drives {
			writeIops = max(writeIops, float64(drive.WritesCompleted)/phase.Seconds)
		}
		block.WriteIopsShare = writeIops / settings.RatedWriteIops
	}
	addClause("write-iops", drivesRead && 0 < phase.Seconds && block.WriteIopsShare <= settings.MaxWriteIopsShare,
		fmt.Sprintf("%.0f writes a second on the busiest drive, %.1f%% of the rated %.0f, against %.0f%%", writeIops, 100*block.WriteIopsShare, settings.RatedWriteIops, 100*settings.MaxWriteIopsShare))

	addClause("series-read", phase.SeriesRead, map[bool]string{
		true:  "the stack's budget, refusal and evacuation series were read",
		false: "the stack's series were not read, so refusals, P0 and batch ages are unknown",
	}[phase.SeriesRead])
	refused := phase.PushesRefusedByBudget + phase.BudgetShedCount
	addClause("no-budget-refusal", phase.SeriesRead && refused == 0,
		fmt.Sprintf("%d pushes refused 429 RESOURCE_EXHAUSTED, %d sheds the budget counted", phase.PushesRefusedByBudget, phase.BudgetShedCount))
	foldBehind := phase.PushesFoldBehind + phase.FoldBehindCount
	addClause("no-fold-behind", phase.SeriesRead && foldBehind == 0,
		fmt.Sprintf("%d pushes answered FOLD_BEHIND, %d refusals evs counted", phase.PushesFoldBehind, phase.FoldBehindCount))
	addClause("batch-age", phase.SeriesRead && phase.OldestBatchSecondsMax <= settings.MaxBatchAge.Seconds(),
		fmt.Sprintf("the oldest batch reached %.1f h, against %.0f h", phase.OldestBatchSecondsMax/3600, settings.MaxBatchAge.Hours()))
	addClause("p0-page-silent", phase.SeriesRead && phase.P0RefillRatioMax < settings.P0PageRatio,
		fmt.Sprintf("P0's refill ratio reached %.3f, the page at %.1f", phase.P0RefillRatioMax, settings.P0PageRatio))

	operations := []string{}
	for operation := range phase.OperationLatencyMillis {
		operations = append(operations, operation)
	}
	sort.Strings(operations)
	slowest := []string{}
	latencyPass := 0 < len(operations)
	for _, operation := range operations {
		percentiles := phase.OperationLatencyMillis[operation]
		if float64(settings.MaxOperationP99.Milliseconds()) < percentiles.P99 {
			latencyPass = false
		}
		slowest = append(slowest, fmt.Sprintf("%s %.1f ms over %d", operation, percentiles.P99, percentiles.Count))
	}
	addClause("operation-p99", latencyPass, func() string {
		if len(operations) == 0 {
			return "no store operation's latency was recorded"
		}
		return fmt.Sprintf("p99 %s, against %d ms", strings.Join(slowest, ", "), settings.MaxOperationP99.Milliseconds())
	}())

	// P0 by kind against the refill: priced bytes times the phase's calibration
	if drivesRead && 0 < phase.PricedByteCount {
		block.Calibration = float64(busiest.WrittenByteCount) / float64(phase.PricedByteCount)
	}
	block.P0Shares = map[string]float64{}
	if 0 < phase.Seconds {
		refill := settings.refillByteCountPerHour(phase.RefillDwpd) * phase.Seconds / 3600
		for kind, priced := range phase.P0PricedByteCounts {
			share := float64(priced) * block.Calibration / refill
			block.P0Shares[kind] = share
			block.P0Share += share
		}
	}
	block.P0WarnLine = min(settings.P0WarnFactor*block.P0Share, settings.P0WarnMaxShare)
	block.RecordsPerBatch = phase.RecordsPerBatch
	block.YearTwoDwpd = block.PushByteCountPerDrive * settings.YearTwoPushesPerSecond * 86400 / float64(settings.DriveByteCount)

	if block.PriceTable != nil && 0 < block.PushByteCountPerDrive {
		added, err := block.PriceTable.PriceOperations(settings.Format2IndexOperations[blockByteCount])
		if err != nil {
			block.Errors = append(block.Errors, fmt.Sprintf("format 2's projection: %v", err))
		} else {
			block.Format2PushByteCountPerDrive = block.PushByteCountPerDrive + float64(added)
			block.Format2Fits = block.Format2PushByteCountPerDrive <= settings.MaxPushByteCountPerDrive
		}
	}

	block.GatePass = len(block.Errors) == 0
	for _, clause := range block.Clauses {
		if !clause.Pass {
			block.GatePass = false
		}
	}
	return block
}

// The busiest drive not under heal, by bytes written: the drive the budget drains by (§19.6's budget row).
func busiestDrive(drives []*DriveDelta) (*DriveDelta, bool) {
	var busiest *DriveDelta
	for _, drive := range drives {
		if drive.Missing || drive.Healing {
			continue
		}
		if busiest == nil || busiest.WrittenByteCount < drive.WrittenByteCount {
			busiest = drive
		}
	}
	return busiest, busiest != nil
}

func anyMissing(drives []*DriveDelta) bool {
	for _, drive := range drives {
		if drive.Missing {
			return true
		}
	}
	return len(drives) == 0
}

func summarizePhase(phase *MixPhase, settings *ReportSettings) *PhaseSummary {
	summary := &PhaseSummary{Name: phase.Name, OfferedPushesPerSecond: phase.OfferedPushesPerSecond, PushesCommitted: phase.PushesCommitted}
	busiest, ok := busiestDrive(phase.Drives)
	if !ok || phase.Seconds <= 0 {
		return summary
	}
	if 0 < phase.PushesCommitted {
		summary.PushByteCountPerDrive = float64(busiest.WrittenByteCount) / float64(phase.PushesCommitted)
	}
	summary.BusiestDriveDwpd = float64(busiest.WrittenByteCount) / phase.Seconds * 86400 / float64(settings.DriveByteCount)
	for _, drive := range phase.Drives {
		summary.WriteIops = max(summary.WriteIops, float64(drive.WritesCompleted)/phase.Seconds)
		summary.ReadIops = max(summary.ReadIops, float64(drive.ReadsCompleted)/phase.Seconds)
	}
	days := []float64{}
	if 0 < phase.RawUsageDelta && 0 < phase.RawCapacityByteCount {
		left := settings.FillRawShare*float64(phase.RawCapacityByteCount) - float64(phase.RawUsageByteCount)
		days = append(days, max(0, left)/(float64(phase.RawUsageDelta)/phase.Seconds)/86400)
	}
	if inodes := phase.UsedInodeCount + phase.FreeInodeCount; 0 < phase.InodeDelta && 0 < inodes {
		left := settings.FillInodeShare*float64(inodes) - float64(phase.UsedInodeCount)
		days = append(days, max(0, left)/(float64(phase.InodeDelta)/phase.Seconds)/86400)
	}
	if 0 < len(days) {
		summary.FillDays = slices.Min(days)
	}
	return summary
}

// The busiest drive's bytes per operation over an ops window, the idle window's background taken off it.
func opsBytesPerOp(result *OpsResult, idle *IdleWindow) (float64, error) {
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
				idleRates[drive.DriveId] = float64(drive.WrittenByteCount) / idle.Seconds
			}
		}
	}
	busiest := math.Inf(-1)
	for _, drive := range result.Drives {
		if drive.Healing {
			continue
		}
		written := float64(drive.WrittenByteCount) - idleRates[drive.DriveId]*result.Seconds
		busiest = max(busiest, written)
	}
	if math.IsInf(busiest, -1) {
		return 0, fmt.Errorf("%s at %d B: every drive was under heal", result.Class, result.ObjectByteCount)
	}
	return max(0, busiest) / float64(result.Count), nil
}

// The price table one block size's ops run measures: see the file header. Footprints are each create size's raw
// bytes and inodes per object.
func DerivePriceTable(ops *Measurement, settings *ReportSettings) (*evsservice.PriceTable, []*PriceFitRow, []*ObjectFootprint, error) {
	if ops.Mode != ModeOps {
		return nil, nil, nil, fmt.Errorf("a %s measurement prices nothing", ops.Mode)
	}
	if ops.DataShardCount <= 0 {
		return nil, nil, nil, errors.New("no data shard count")
	}
	rows := map[string]float64{}
	smallSizes := 0
	for _, class := range []string{OpsClassCreate, OpsClassOverwrite, OpsClassDelete} {
		for _, result := range ops.Ops {
			if result.Class != class {
				continue
			}
			// a delete costs the same at any size, so every size counts toward its row
			if class != OpsClassDelete && settings.SmallObjectByteCount < result.ObjectByteCount {
				continue
			}
			perOp, err := opsBytesPerOp(result, ops.Idle)
			if err != nil {
				return nil, nil, nil, err
			}
			rows[class] = max(rows[class], perOp)
			if class == OpsClassCreate {
				smallSizes += 1
			}
		}
	}
	for _, class := range []string{OpsClassCreate, OpsClassOverwrite, OpsClassDelete} {
		if rows[class] <= 0 {
			return nil, nil, nil, fmt.Errorf("no %s measured at a small size", class)
		}
	}
	table := &evsservice.PriceTable{
		BlockByteCount:       ops.BlockByteCount,
		SmallObjectByteCount: settings.SmallObjectByteCount,
		CreateByteCount:      int64(math.Ceil(rows[OpsClassCreate])),
		OverwriteByteCount:   int64(math.Ceil(rows[OpsClassOverwrite])),
		DeleteByteCount:      int64(math.Ceil(rows[OpsClassDelete])),
		ShardCount:           int64(ops.DataShardCount),
	}

	kinds := map[string]evsservice.WriteOperationKind{
		OpsClassCreate:    evsservice.WriteOperationCreate,
		OpsClassOverwrite: evsservice.WriteOperationOverwrite,
		OpsClassDelete:    evsservice.WriteOperationDelete,
	}
	fit := []*PriceFitRow{}
	footprints := []*ObjectFootprint{}
	for _, result := range ops.Ops {
		kind, ok := kinds[result.Class]
		if !ok {
			continue
		}
		perOp, err := opsBytesPerOp(result, ops.Idle)
		if err != nil {
			return nil, nil, nil, err
		}
		priced, err := table.Price(evsservice.WriteOperation{Kind: kind, ObjectByteCount: result.ObjectByteCount, Count: 1})
		if err != nil {
			return nil, nil, nil, err
		}
		row := &PriceFitRow{Class: result.Class, ObjectByteCount: result.ObjectByteCount, MeasuredPerOp: perOp, PricedPerOp: priced}
		if 0 < priced {
			row.Error = (perOp - float64(priced)) / float64(priced)
			row.Under = settings.PriceFitTolerance < row.Error
		}
		fit = append(fit, row)
		if result.Class == OpsClassCreate && 0 < result.Count {
			footprints = append(footprints, &ObjectFootprint{
				ObjectByteCount: result.ObjectByteCount,
				RawBytesPerOp:   float64(result.RawUsageDelta) / float64(result.Count),
				InodesPerOp:     float64(result.InodeDelta) / float64(result.Count),
			})
		}
	}
	return table, fit, footprints, nil
}

// The price table file: evsservice.PriceTable's fields under their own names, and the run it came from beside them.
type priceTableFile struct {
	*evsservice.PriceTable
	Source priceTableSource `json:"source"`
}

type priceTableSource struct {
	// D-TEST-11's report it came from.
	Generated time.Time         `json:"generated"`
	CommitSet map[string]string `json:"commit_set"`
	Choice    string            `json:"choice"`
}

// Writes the chosen block size's price table: see the file header. It refuses when the report chose none, so no
// table from a failed gate reaches config.
func WritePriceTableFile(out io.Writer, report *NvmeReport) error {
	table := report.PriceTable()
	if !report.Pass || table == nil {
		return errors.New("the report chose no block size, so it has no price table")
	}
	data, err := json.MarshalIndent(&priceTableFile{
		PriceTable: table,
		Source:     priceTableSource{Generated: report.Generated, CommitSet: report.CommitSet, Choice: report.Choice},
	}, "", "  ")
	if err != nil {
		return err
	}
	_, err = out.Write(append(data, '\n'))
	return err
}

// A price table file as evsservice.PriceTable, refused unless it holds a table the budget can price with.
func ParsePriceTableFile(data []byte) (*evsservice.PriceTable, error) {
	table := &evsservice.PriceTable{}
	if err := json.Unmarshal(data, table); err != nil {
		return nil, err
	}
	if table.BlockByteCount != 1024 && table.BlockByteCount != 4*1024 {
		return nil, fmt.Errorf("a price table for %d B blocks", table.BlockByteCount)
	}
	if table.SmallObjectByteCount <= 0 || table.CreateByteCount <= 0 || table.OverwriteByteCount <= 0 || table.DeleteByteCount <= 0 || table.ShardCount <= 0 {
		return nil, fmt.Errorf("a price table with a row not positive: %+v", table)
	}
	return table, nil
}

// The report as text, for the review.
func WriteNvmeReport(out io.Writer, report *NvmeReport) error {
	b := &strings.Builder{}
	verdict := map[bool]string{true: "pass", false: "fail"}
	fmt.Fprintf(b, "D-TEST-11's nvme report, %s\n", report.Generated.UTC().Format(time.RFC3339))
	fmt.Fprintf(b, "build %s\n", formatCommitSet(report.CommitSet))
	fmt.Fprintf(b, "verdict %s; block size: %s\n", verdict[report.Pass], report.Choice)
	for _, block := range report.Blocks {
		fmt.Fprintf(b, "\n%d B blocks: gate %s\n", block.BlockByteCount, verdict[block.GatePass])
		for _, clause := range block.Clauses {
			fmt.Fprintf(b, "  %-18s %-4s %s\n", clause.Name, verdict[clause.Pass], clause.Detail)
		}
		for _, err := range block.Errors {
			fmt.Fprintf(b, "  error %s\n", err)
		}
		if table := block.PriceTable; table != nil {
			fmt.Fprintf(b, "  price table: create %d B, overwrite %d B, delete %d B a drive, objects to %d B as they stand, past it a shard of %d\n",
				table.CreateByteCount, table.OverwriteByteCount, table.DeleteByteCount, table.SmallObjectByteCount, table.ShardCount)
			for _, row := range block.PriceFit {
				fmt.Fprintf(b, "    %-9s %7d B: measured %9.0f, priced %7d (%+.1f%%)\n", row.Class, row.ObjectByteCount, row.MeasuredPerOp, row.PricedPerOp, 100*row.Error)
			}
			for _, footprint := range block.Footprints {
				fmt.Fprintf(b, "    object %7d B: %.0f raw bytes and %.2f inodes on the cluster\n", footprint.ObjectByteCount, footprint.RawBytesPerOp, footprint.InodesPerOp)
			}
		}
		for _, phase := range block.Phases {
			fmt.Fprintf(b, "  phase %-5s %6.0f pushes/s offered, %d committed, %.2f KiB a push, %.3f DWPD, %.0f write and %.0f read IOPS, fill in %.0f d\n",
				phase.Name, phase.OfferedPushesPerSecond, phase.PushesCommitted, phase.PushByteCountPerDrive/1024, phase.BusiestDriveDwpd, phase.WriteIops, phase.ReadIops, phase.FillDays)
		}
		kinds := []string{}
		for kind := range block.P0Shares {
			kinds = append(kinds, kind)
		}
		sort.Strings(kinds)
		for _, kind := range kinds {
			fmt.Fprintf(b, "  p0 %-10s %.4f of the hour's refill\n", kind, block.P0Shares[kind])
		}
		fmt.Fprintf(b, "  p0 share %.4f; §19.8's warn line %.4f of the refill (k %.3f)\n", block.P0Share, block.P0WarnLine, block.Calibration)
		clusterIds := []string{}
		for clusterId := range block.RecordsPerBatch {
			clusterIds = append(clusterIds, clusterId)
		}
		sort.Strings(clusterIds)
		for _, clusterId := range clusterIds {
			fmt.Fprintf(b, "  records per batch on %s: %.2f\n", clusterId, block.RecordsPerBatch[clusterId])
		}
		fmt.Fprintf(b, "  year two on one set: %.2f DWPD; format 2 projected %.2f KiB a push (fits: %t)\n", block.YearTwoDwpd, block.Format2PushByteCountPerDrive/1024, block.Format2Fits)
	}
	_, err := io.WriteString(out, b.String())
	return err
}
