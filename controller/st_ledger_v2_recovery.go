package controller

// st_ledger_v2_recovery.go — the phase C recovery component
// (VERSION2-FINAL.md §16 step 6; owner decision 2026-10-31). The v1 sweep
// moves v1 carry into vault2, where the owner assigns it as surplus to a v2
// epoch's funding. A scheduled source epoch's missed-root funding (mainnet
// pool 1: epoch 3, 4,132.715621039 α) is then allocated by that source
// epoch's own provider rows, and the receiving epoch's rows allocate the
// rest, f_k − R, which includes every other part of the surplus (epoch 1's
// share, the reveal-lag capture).
//
// Where the recovery lands is read from the chain, as the validator reads
// it: the first epoch whose own vault2 SurplusAssigned amounts reach the
// scheduled total (ledger.PlanRecovery), from the mirrored events, with each
// source amount checked against vault1's RootMissed entitlement. The source
// epoch's rows are never built inside the driver's epoch step: they are
// built once from a preserved usage artifact by an operator command and
// recorded as the source epoch's fixed inputs, so the receiving root's step
// reads no second epoch of usage.
//
// The usage artifact preserves the source epoch's census, the input that
// retention moves and that is expensive to read: `st v2-recovery-usage`
// reads it read-only, in one REPEATABLE READ, READ ONLY transaction through
// the whole-window reader, and writes content-addressed objects. `st
// v2-recovery-record` builds the provider rows from it with the v2 rules,
// wallet selection by retained consents and head exclusion at the source
// epoch's boundaries, publishes their chunks to the ledger store, retains
// the wallet resolutions and records the rows.

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/holiman/uint256"

	"github.com/urnetwork/glog"

	"github.com/urfoundation/sn/ledger"
	"github.com/urfoundation/sn/protocol"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/startifact"
)

var (
	// ErrStLedgerV2RecoveryRows refuses a build whose recovery component's
	// source epoch has no recorded rows: `st v2-recovery-record` records
	// them, and `st v2-unrecoverable` declares them null.
	ErrStLedgerV2RecoveryRows = errors.New("st ledger v2: a recovery component's source rows are not recorded")
	// ErrStLedgerV2RecoveryMirror refuses a recovery plan while the event
	// mirror has not applied the covered epochs' funding.
	ErrStLedgerV2RecoveryMirror = errors.New("st ledger v2: the event mirror has not reached the root's funding")
	// ErrStLedgerV2RecoveryRefused refuses the operator's recovery commands.
	ErrStLedgerV2RecoveryRefused = errors.New("st ledger v2: recovery refused")
)

// StLedgerV2AlertRecoveryRows raises a receiving root that cannot be built
// because its recovery component's source rows are not recorded.
const StLedgerV2AlertRecoveryRows = "v2_recovery_rows_missing"

// StLedgerV2AlertRecoveryIncomplete raises a root whose epochs received
// surplus that left part of the scheduled recovery unplaced, such as a split
// or short assignment, or that passes the schedule's last epoch with part of
// it unplaced, which no later surplus places.
const StLedgerV2AlertRecoveryIncomplete = "v2_recovery_incomplete"

// stLedgerV2RecoverySchedule returns a pool's recovery schedule; tests
// replace it.
var stLedgerV2RecoverySchedule = ledger.ScheduledRecovery

// StLedgerV2Recovery is one recovery component of a covered epoch: a
// scheduled source epoch's v1 funding and the source epoch's recorded rows,
// nil when none are recorded.
type StLedgerV2Recovery struct {
	ledger.Recovery
	Rows *model.StLedgerV2EpochRows
}

// StLedgerV2RecoveryProviders is a recovery component's source chunks as a
// header lists them; Null marks providers: null.
type StLedgerV2RecoveryProviders struct {
	Epoch          uint64
	Amount         uint64
	ProviderChunks []string
	Null           bool
}

// stLedgerV2RecoveryPlan returns the recovery components the covered epochs
// p+1 … e must carry, read as the validator reads them: the pool's mirrored
// SurplusAssigned events through epoch e's funding, every one of which the
// mirror has applied, placed by ledger.PlanRecovery, each source amount
// checked against vault1's RootMissed entitlement. A pool without a schedule
// reads nothing.
func stLedgerV2RecoveryPlan(ctx context.Context, cfg *StConfig, firstEpoch uint64, p uint64, e uint64) (map[uint64][]ledger.Recovery, error) {
	schedule := stLedgerV2RecoverySchedule(cfg.VaultV2, cfg.NoId)
	if schedule == nil {
		return nil, nil
	}
	key := cfg.DeploymentKey()
	// Surplus for an epoch up to e is assigned before e is funded.
	funding := model.GetStLedgerV2Funding(ctx, key, cfg.NoId, e)
	if funding == nil {
		return nil, fmt.Errorf("%w: epoch %d's funding is not mirrored", ErrStLedgerV2RecoveryMirror, e)
	}
	if next := model.GetStChainCheckpoint(ctx, key).NextBlock; next <= funding.BlockNumber {
		return nil, fmt.Errorf("%w: the mirror is at block %d, before epoch %d's funding at block %d", ErrStLedgerV2RecoveryMirror, next, e, funding.BlockNumber)
	}
	surplus, err := model.GetStLedgerV2SurplusAssigned(ctx, key, cfg.NoId, schedule.FromBlock, funding.BlockNumber)
	if err != nil {
		return nil, err
	}
	plan := ledger.PlanRecovery(schedule, surplus, firstEpoch, p, e)
	if remaining := ledger.RecoveryRemaining(schedule, surplus, firstEpoch, e); !remaining.IsZero() {
		received := false
		for k, amount := range surplus {
			received = received || (p < k && k <= e && !amount.IsZero())
		}
		if received || schedule.ThroughEpoch <= e {
			stLedgerV2Alert(cfg, StLedgerV2AlertRecoveryIncomplete, fmt.Sprintf("epoch %d", e), "%s rao of the scheduled recovery stay unplaced through epoch %d (the schedule places it through epoch %d); assign it in one call before the schedule's last epoch closes", remaining.Dec(), e, schedule.ThroughEpoch)
		}
	}
	if len(plan) == 0 {
		return nil, nil
	}
	client := stClient()
	if client == nil {
		return nil, errStNotConfigured
	}
	for _, components := range plan {
		for _, component := range components {
			pool, err := client.PoolState(ctx, component.Epoch, cfg.NoId)
			if err != nil {
				return nil, fmt.Errorf("st ledger v2: vault1 entitlement of recovered epoch %d: %w", component.Epoch, err)
			}
			total, err := stUint256(pool.PoolTotalRao)
			if err != nil {
				return nil, err
			}
			if err := schedule.CheckSource(component.Epoch, pool.Status, &total); err != nil {
				return nil, err
			}
		}
	}
	return plan, nil
}

// stLedgerV2AttachRecovery attaches each planned recovery component to its
// covered epoch with its source epoch's recorded rows.
func stLedgerV2AttachRecovery(ctx context.Context, cfg *StConfig, input *StLedgerV2BuildInput, plan map[uint64][]ledger.Recovery) {
	for _, covered := range input.Epochs {
		for _, component := range plan[covered.Epoch] {
			rows := model.GetStLedgerV2EpochRows(ctx, cfg.DeploymentKey(), cfg.NoId, component.Epoch, component.Epoch)[component.Epoch]
			covered.Recovery = append(covered.Recovery, &StLedgerV2Recovery{Recovery: component, Rows: rows})
		}
	}
}

// stLedgerV2RecoveryInputs reads a covered epoch's recovery components'
// recorded source rows back from their published chunks.
func stLedgerV2RecoveryInputs(ctx context.Context, covered *StLedgerV2CoveredEpoch) ([]ledger.RecoveryInput, error) {
	var inputs []ledger.RecoveryInput
	for _, component := range covered.Recovery {
		input := ledger.RecoveryInput{Recovery: component.Recovery}
		switch {
		case component.Rows == nil:
			return nil, fmt.Errorf("%w: epoch %d recovers epoch %d", ErrStLedgerV2RecoveryRows, covered.Epoch, component.Epoch)
		case component.Rows.Null():
			input.Null = true
		default:
			chunks := make([][]byte, len(component.Rows.ProviderChunks))
			input.Chunks = make([]ledger.Ref, len(chunks))
			for i, ref := range component.Rows.ProviderChunks {
				chunk, err := GetStLedgerV2Object(ctx, ref)
				if err != nil {
					return nil, fmt.Errorf("st ledger v2: epoch %d recovery %d: %w", covered.Epoch, component.Epoch, err)
				}
				chunks[i], input.Chunks[i] = chunk, ledger.RefOf(chunk)
			}
			rows, err := ledger.DecodeProviderChunks(chunks)
			if err != nil {
				return nil, fmt.Errorf("st ledger v2: epoch %d recovery %d: %w", covered.Epoch, component.Epoch, err)
			}
			input.Rows = rows
		}
		inputs = append(inputs, input)
	}
	return inputs, nil
}

// ---------------------------------------------------------------------------
// The usage artifact
// ---------------------------------------------------------------------------

// StLedgerV2RecoveryUsageSchema names the usage artifact's manifest.
const StLedgerV2RecoveryUsageSchema = "urnetwork-recovery-usage-v1"

// stLedgerV2RecoveryUsageRowBytes is a usage row: client_id(16) ‖
// network_id(16) ‖ payout_bytes(8), big-endian, in client id order.
const stLedgerV2RecoveryUsageRowBytes = 40

// StLedgerV2RecoveryBoundary is one of the source epoch's boundaries.
type StLedgerV2RecoveryBoundary struct {
	Block uint64 `json:"block"`
	Hash  string `json:"hash"`
	Time  string `json:"time"`
}

// StLedgerV2RecoveryUsageManifest describes a source epoch's preserved usage
// census. It is compact JSON in field order, and its sha256 names it.
type StLedgerV2RecoveryUsageManifest struct {
	Schema       string                     `json:"schema"`
	DeploymentId string                     `json:"deployment_id"`
	NoId         uint64                     `json:"no_id"`
	Epoch        uint64                     `json:"epoch"`
	PolicyHash   string                     `json:"policy_hash"`
	Start        StLedgerV2RecoveryBoundary `json:"start"`
	End          StLedgerV2RecoveryBoundary `json:"end"`
	Rows         uint64                     `json:"rows"`
	PayoutBytes  string                     `json:"payout_bytes"`
	Chunks       []string                   `json:"chunks"`
}

// StLedgerV2RecoveryUsage is a usage artifact: its manifest, the manifest's
// bytes and reference, and its chunks in order.
type StLedgerV2RecoveryUsage struct {
	Manifest      *StLedgerV2RecoveryUsageManifest
	ManifestBytes []byte
	Ref           string
	Chunks        [][]byte
}

func stLedgerV2RecoveryBoundary(boundary protocol.ClientKeyEffectiveBoundary, at time.Time) StLedgerV2RecoveryBoundary {
	return StLedgerV2RecoveryBoundary{Block: boundary.Block, Hash: "0x" + hex.EncodeToString(boundary.Hash[:]), Time: at.UTC().Format(time.RFC3339Nano)}
}

// encodeStLedgerV2RecoveryUsage encodes a census, one usage per client in
// client id order, as usage chunks of ledger.ChunkRows rows.
func encodeStLedgerV2RecoveryUsage(usages []*model.StProviderUsage) ([][]byte, *uint256.Int, error) {
	total := new(uint256.Int)
	var chunks [][]byte
	var chunk []byte
	for i, usage := range usages {
		switch {
		case usage == nil || usage.ClientId == (server.Id{}) || usage.NetworkId == (server.Id{}) || usage.PayoutByteCount < 0:
			return nil, nil, fmt.Errorf("%w: census row %d is not a provider's settled usage", ErrStLedgerV2RecoveryRefused, i)
		case i > 0 && bytes.Compare(usages[i-1].ClientId[:], usage.ClientId[:]) >= 0:
			return nil, nil, fmt.Errorf("%w: census row %d is not after row %d in client id order", ErrStLedgerV2RecoveryRefused, i, i-1)
		}
		chunk = append(chunk, usage.ClientId[:]...)
		chunk = append(chunk, usage.NetworkId[:]...)
		chunk = binary.BigEndian.AppendUint64(chunk, uint64(usage.PayoutByteCount))
		total.AddUint64(total, uint64(usage.PayoutByteCount))
		if len(chunk) == ledger.ChunkRows*stLedgerV2RecoveryUsageRowBytes {
			chunks, chunk = append(chunks, chunk), nil
		}
	}
	if len(chunk) > 0 {
		chunks = append(chunks, chunk)
	}
	return chunks, total, nil
}

// decodeStLedgerV2RecoveryUsage decodes usage chunks, refusing a chunk list
// that is not full chunks and a non-empty last one, or rows that are not in
// strictly ascending client id order with network ids.
func decodeStLedgerV2RecoveryUsage(chunks [][]byte) ([]*model.StProviderUsage, *uint256.Int, error) {
	total := new(uint256.Int)
	var usages []*model.StProviderUsage
	for j, chunk := range chunks {
		full := ledger.ChunkRows * stLedgerV2RecoveryUsageRowBytes
		if len(chunk) == 0 || len(chunk)%stLedgerV2RecoveryUsageRowBytes != 0 || len(chunk) > full || (j < len(chunks)-1 && len(chunk) != full) {
			return nil, nil, fmt.Errorf("%w: usage chunk %d of %d has %d bytes", ErrStLedgerV2RecoveryRefused, j, len(chunks), len(chunk))
		}
		for offset := 0; offset < len(chunk); offset += stLedgerV2RecoveryUsageRowBytes {
			usage := &model.StProviderUsage{}
			copy(usage.ClientId[:], chunk[offset:offset+16])
			copy(usage.NetworkId[:], chunk[offset+16:offset+32])
			bytesCount := binary.BigEndian.Uint64(chunk[offset+32 : offset+40])
			if bytesCount > 1<<63-1 {
				return nil, nil, fmt.Errorf("%w: usage row of %d bytes", ErrStLedgerV2RecoveryRefused, bytesCount)
			}
			usage.PayoutByteCount = int64(bytesCount)
			if usage.ClientId == (server.Id{}) || usage.NetworkId == (server.Id{}) {
				return nil, nil, fmt.Errorf("%w: usage row %d has a zero id", ErrStLedgerV2RecoveryRefused, len(usages))
			}
			if n := len(usages); n > 0 && bytes.Compare(usages[n-1].ClientId[:], usage.ClientId[:]) >= 0 {
				return nil, nil, fmt.Errorf("%w: usage row %d is not after row %d in client id order", ErrStLedgerV2RecoveryRefused, n, n-1)
			}
			total.AddUint64(total, bytesCount)
			usages = append(usages, usage)
		}
	}
	return usages, total, nil
}

// stLedgerV2RecoveryArtifact names a census's chunks in a manifest for the
// epoch's authenticated window.
func stLedgerV2RecoveryArtifact(cfg *StConfig, authority *StPayoutEpochAuthority, usages []*model.StProviderUsage) (*StLedgerV2RecoveryUsage, error) {
	chunks, total, err := encodeStLedgerV2RecoveryUsage(usages)
	if err != nil {
		return nil, err
	}
	manifest := &StLedgerV2RecoveryUsageManifest{
		Schema: StLedgerV2RecoveryUsageSchema, DeploymentId: cfg.DeploymentId, NoId: cfg.NoId, Epoch: authority.Epoch,
		PolicyHash: "0x" + hex.EncodeToString(authority.PolicyHash[:]),
		Start:      stLedgerV2RecoveryBoundary(authority.Start, authority.StartTime), End: stLedgerV2RecoveryBoundary(authority.End, authority.EndTime),
		Rows: uint64(len(usages)), PayoutBytes: total.Dec(), Chunks: []string{},
	}
	for _, chunk := range chunks {
		manifest.Chunks = append(manifest.Chunks, startifact.LedgerObjectRef(chunk))
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		return nil, err
	}
	return &StLedgerV2RecoveryUsage{Manifest: manifest, ManifestBytes: data, Ref: startifact.LedgerObjectRef(data), Chunks: chunks}, nil
}

// stLedgerV2RecoveryScheduled refuses a source epoch the configured pool's
// schedule does not recover.
func stLedgerV2RecoveryScheduled(cfg *StConfig, epoch uint64) (*ledger.RecoverySchedule, error) {
	schedule := stLedgerV2RecoverySchedule(cfg.VaultV2, cfg.NoId)
	if schedule == nil || !slices.ContainsFunc(schedule.Sources, func(source ledger.Recovery) bool { return source.Epoch == epoch }) {
		return nil, fmt.Errorf("%w: epoch %d is not a scheduled recovery source of pool %d", ErrStLedgerV2RecoveryRefused, epoch, cfg.NoId)
	}
	return schedule, nil
}

// BuildStLedgerV2RecoveryUsage builds a scheduled source epoch's usage artifact:
// the epoch's authenticated window from chain, then its census in one
// REPEATABLE READ, READ ONLY transaction through the whole-window reader.
// It writes nothing: the caller runs it in a read-only process and stores
// the result.
func BuildStLedgerV2RecoveryUsage(ctx context.Context, epoch uint64) (*StLedgerV2RecoveryUsage, error) {
	if !stConfig().LedgerV2Enabled() {
		return nil, errStLedgerV2NotConfigured
	}
	cfg, client, err := stRequire()
	if err != nil {
		return nil, err
	}
	if _, err := stLedgerV2RecoveryScheduled(cfg, epoch); err != nil {
		return nil, err
	}
	authority, err := stLedgerV2EpochAuthority(ctx, cfg, client, epoch)
	if err != nil {
		return nil, err
	}
	usages, err := model.GetStEpochProviderUsageAtEpoch(ctx, epoch, authority.StartTime, authority.EndTime)
	if err != nil {
		return nil, err
	}
	return stLedgerV2RecoveryArtifact(cfg, authority, usages)
}

// recoveryUsageFile names an object of the artifact in a directory.
func recoveryUsageFile(dir string, ref string) string {
	return filepath.Join(dir, strings.Replace(ref, ":", "-", 1))
}

// WriteStLedgerV2RecoveryUsage writes the artifact's objects into dir, each
// in a new file named by its reference, and the manifest last.
func WriteStLedgerV2RecoveryUsage(usage *StLedgerV2RecoveryUsage, dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	write := func(ref string, data []byte) error {
		file, err := os.OpenFile(recoveryUsageFile(dir, ref), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, os.ErrExist) {
			existing, readErr := os.ReadFile(recoveryUsageFile(dir, ref))
			if readErr == nil && bytes.Equal(existing, data) {
				return nil
			}
			return errors.Join(fmt.Errorf("%s already holds other bytes", recoveryUsageFile(dir, ref)), readErr)
		}
		if err != nil {
			return err
		}
		_, writeErr := file.Write(data)
		return errors.Join(writeErr, file.Sync(), file.Close())
	}
	for i, chunk := range usage.Chunks {
		if err := write(usage.Manifest.Chunks[i], chunk); err != nil {
			return err
		}
	}
	return write(usage.Ref, usage.ManifestBytes)
}

// StoreStLedgerV2RecoveryUsage puts the artifact's objects in the blob store,
// each once under its sha256 (startifact.PutRecoveryObject), the manifest last.
func StoreStLedgerV2RecoveryUsage(ctx context.Context, usage *StLedgerV2RecoveryUsage) error {
	store, err := stLedgerV2Store()
	if err != nil {
		return err
	}
	for _, chunk := range usage.Chunks {
		if _, err := startifact.PutRecoveryObject(ctx, store, chunk); err != nil {
			return err
		}
	}
	_, err = startifact.PutRecoveryObject(ctx, store, usage.ManifestBytes)
	return err
}

// ReadStLedgerV2RecoveryUsage reads an artifact by its manifest reference
// from dir, or from the blob store when dir is empty, checking every object
// against its reference, the manifest's canonical form and its chunks.
func ReadStLedgerV2RecoveryUsage(ctx context.Context, ref string, dir string) (*StLedgerV2RecoveryUsage, []*model.StProviderUsage, error) {
	read := func(ref string) ([]byte, error) {
		if _, err := startifact.ParseLedgerObjectRef(ref); err != nil {
			return nil, err
		}
		var data []byte
		var err error
		if dir != "" {
			file, openErr := os.Open(recoveryUsageFile(dir, ref))
			if openErr != nil {
				return nil, openErr
			}
			data, err = io.ReadAll(io.LimitReader(file, startifact.MaximumLedgerObjectBytes+1))
			err = errors.Join(err, file.Close())
		} else {
			store, storeErr := stLedgerV2Store()
			if storeErr != nil {
				return nil, storeErr
			}
			data, err = startifact.GetRecoveryObject(ctx, store, ref)
		}
		if err != nil {
			return nil, err
		}
		if startifact.LedgerObjectRef(data) != ref {
			return nil, fmt.Errorf("%w: object %s does not match its reference", ErrStLedgerV2RecoveryRefused, ref)
		}
		return data, nil
	}
	data, err := read(ref)
	if err != nil {
		return nil, nil, err
	}
	manifest := &StLedgerV2RecoveryUsageManifest{}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(manifest); err != nil {
		return nil, nil, fmt.Errorf("%w: manifest %s: %v", ErrStLedgerV2RecoveryRefused, ref, err)
	}
	if canonical, err := json.Marshal(manifest); err != nil || !bytes.Equal(canonical, data) || manifest.Schema != StLedgerV2RecoveryUsageSchema {
		return nil, nil, fmt.Errorf("%w: manifest %s is not a canonical %s manifest", ErrStLedgerV2RecoveryRefused, ref, StLedgerV2RecoveryUsageSchema)
	}
	usage := &StLedgerV2RecoveryUsage{Manifest: manifest, ManifestBytes: data, Ref: ref}
	for _, chunkRef := range manifest.Chunks {
		chunk, err := read(chunkRef)
		if err != nil {
			return nil, nil, err
		}
		usage.Chunks = append(usage.Chunks, chunk)
	}
	usages, total, err := decodeStLedgerV2RecoveryUsage(usage.Chunks)
	if err != nil {
		return nil, nil, err
	}
	if uint64(len(usages)) != manifest.Rows || total.Dec() != manifest.PayoutBytes {
		return nil, nil, fmt.Errorf("%w: manifest %s counts %d rows and %s bytes, its chunks %d and %s", ErrStLedgerV2RecoveryRefused, ref, manifest.Rows, manifest.PayoutBytes, len(usages), total.Dec())
	}
	return usage, usages, nil
}

// StLedgerV2RecoveryRecord summarizes recorded source rows by counts only.
// AlreadyRecorded reports a record that was in place before the command.
type StLedgerV2RecoveryRecord struct {
	Epoch           uint64
	Rows            int
	EligibleRows    int
	Coldkeys        int
	PayoutBytes     *uint256.Int
	EligibleBytes   *uint256.Int
	ProviderChunks  []string
	AlreadyRecorded bool
}

// stLedgerV2RecoveryRepeat answers a record command for an epoch whose rows
// are already recorded. The record stands. The census's clients and bytes,
// after a configured roster fixes the provider universe, must be the recorded
// rows'; another census is ErrStLedgerV2EpochRowsFixed. It selects no wallets
// and reads no heads, whose consents and bindings may have advanced since the
// record, so repeating the command with its usage reports the record.
func stLedgerV2RecoveryRepeat(ctx context.Context, cfg *StConfig, authority *StPayoutEpochAuthority, ref string, usages []*model.StProviderUsage, existing *model.StLedgerV2EpochRows) (*StLedgerV2RecoveryRecord, error) {
	epoch := authority.Epoch
	work, _, err := stLoadProviderWorkAuthority(ctx, cfg, authority)
	if err == nil {
		usages, err = stProviderWorkUsages(ctx, work, usages)
	}
	if err != nil {
		return nil, err
	}
	inputs, err := stLedgerV2RecoveryInputs(ctx, &StLedgerV2CoveredEpoch{Epoch: epoch, Recovery: []*StLedgerV2Recovery{{Recovery: ledger.Recovery{Epoch: epoch, Amount: 1}, Rows: existing}}})
	if err != nil {
		return nil, err
	}
	recorded := inputs[0].Rows
	type censusRow struct {
		clientId [16]byte
		bytes    uint64
	}
	census := make([]censusRow, 0, len(usages))
	for _, usage := range usages {
		// rows are the providers with settled bytes, as stLedgerV2SelectRows builds them
		if usage.PayoutByteCount > 0 {
			census = append(census, censusRow{stId16(usage.ClientId), uint64(usage.PayoutByteCount)})
		}
	}
	rows := make([]censusRow, 0, len(recorded))
	for _, row := range recorded {
		rows = append(rows, censusRow{row.ClientID, row.UsageBytes})
	}
	byClient := func(a censusRow, b censusRow) int {
		return bytes.Compare(a.clientId[:], b.clientId[:])
	}
	slices.SortFunc(census, byClient)
	slices.SortFunc(rows, byClient)
	if !slices.Equal(census, rows) {
		return nil, fmt.Errorf("%w: epoch %d's recorded rows are not the census of usage %s", model.ErrStLedgerV2EpochRowsFixed, epoch, ref)
	}
	record := stLedgerV2RecoveryRecordOf(epoch, recorded, existing.ProviderChunks)
	record.AlreadyRecorded = true
	return record, nil
}

// stLedgerV2RecoveryRecordOf counts rows for the record's summary.
func stLedgerV2RecoveryRecordOf(epoch uint64, rows []protocol.LedgerUsageRow, refs []string) *StLedgerV2RecoveryRecord {
	record := &StLedgerV2RecoveryRecord{Epoch: epoch, Rows: len(rows), PayoutBytes: new(uint256.Int), EligibleBytes: new(uint256.Int), ProviderChunks: refs}
	coldkeys := map[[32]byte]bool{}
	for _, row := range rows {
		record.PayoutBytes.AddUint64(record.PayoutBytes, row.UsageBytes)
		if row.Coldkey != ([32]byte{}) && !row.HeadExcluded {
			record.EligibleRows++
			record.EligibleBytes.AddUint64(record.EligibleBytes, row.UsageBytes)
			coldkeys[row.Coldkey] = true
		}
	}
	record.Coldkeys = len(coldkeys)
	return record
}

// StLedgerV2RecordRecoveryRows is the operator's explicit command that
// builds a scheduled source epoch's provider rows from its preserved usage
// artifact and records them as the source epoch's fixed inputs. The rows
// follow the v2 rules over the source epoch's own window: the usage census
// of the artifact, which must be for this deployment's pool and exactly the
// epoch's authenticated boundaries; wallet selection by retained consents
// (the provider's, its network's, its network's hotkey delegation); and head
// exclusion at both of the epoch's boundaries. Their chunks are published
// first; then the wallet resolutions and the record are written in one
// transaction (model.RecordStLedgerV2RecoveryRows), so a failed record
// writes nothing and can be retried at once. For a source epoch already
// recorded, the artifact is checked against the record and the record is
// returned (stLedgerV2RecoveryRepeat), so a repeat changes nothing and
// another census is refused. It is refused under a v2 pair other than the recorded
// activation's (SV-9), for an epoch the schedule does not recover, for one
// declared unrecoverable, and when a resolution would select another wallet
// than one retained for the epoch.
func StLedgerV2RecordRecoveryRows(ctx context.Context, epoch uint64, ref string, dir string, recordedBy string) (*StLedgerV2RecoveryRecord, error) {
	if recordedBy == "" {
		return nil, fmt.Errorf("%w: the record must name who makes it", ErrStLedgerV2RecoveryRefused)
	}
	if !stConfig().LedgerV2Enabled() {
		return nil, errStLedgerV2NotConfigured
	}
	cfg, client, err := stRequire()
	if err != nil {
		return nil, err
	}
	if err := stLedgerV2ActivationAdmission(ctx, cfg); err != nil {
		return nil, err
	}
	if _, err := stLedgerV2RecoveryScheduled(cfg, epoch); err != nil {
		return nil, err
	}
	key := cfg.DeploymentKey()
	existing := model.GetStLedgerV2EpochRows(ctx, key, cfg.NoId, epoch, epoch)[epoch]
	if existing != nil && existing.Null() {
		return nil, fmt.Errorf("%w: epoch %d was declared unrecoverable by %s", ErrStLedgerV2RecoveryRefused, epoch, existing.DeclaredBy)
	}
	usage, usages, err := ReadStLedgerV2RecoveryUsage(ctx, ref, dir)
	if err != nil {
		return nil, err
	}
	authority, err := stLedgerV2EpochAuthority(ctx, cfg, client, epoch)
	if err != nil {
		return nil, err
	}
	want, err := stLedgerV2RecoveryArtifact(cfg, authority, usages)
	if err != nil {
		return nil, err
	}
	if want.Ref != usage.Ref {
		return nil, fmt.Errorf("%w: usage %s is not epoch %d of pool %d of deployment %s over its authenticated boundaries", ErrStLedgerV2RecoveryRefused, ref, epoch, cfg.NoId, cfg.DeploymentId)
	}
	if existing != nil {
		return stLedgerV2RecoveryRepeat(ctx, cfg, authority, ref, usages, existing)
	}
	rows, resolutions, err := stLedgerV2SelectRows(ctx, cfg, client, authority, usages)
	if err != nil {
		return nil, err
	}
	chunks := ledger.EncodeProviderChunks(rows)
	refs := make([]string, len(chunks))
	for i, chunk := range chunks {
		if refs[i], err = PutStLedgerV2Object(ctx, chunk); err != nil {
			return nil, err
		}
	}
	if err := model.RecordStLedgerV2RecoveryRows(ctx, key, cfg.NoId, epoch, refs, resolutions); err != nil {
		return nil, err
	}
	record := stLedgerV2RecoveryRecordOf(epoch, rows, refs)
	glog.Infof("[st]v2 recovery source epoch %d rows recorded from usage %s by %s: %d rows, %d eligible\n", epoch, ref, recordedBy, record.Rows, record.EligibleRows)
	return record, nil
}
