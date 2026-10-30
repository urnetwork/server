package controller

// The SN Top board (LEADERTOP.md, owner decision 2026-10-29): which networks
// the SN25 validator would rank in its Top 200 if they claimed now.
//
//   - `GET /sn/top` — public, optional auth: the top 400 of one snapshot,
//     keyset paged, plus the caller's own row wherever it ranks
//   - `GET /sn/top/me` — client auth: the caller's detail and 90-day history
//   - ComputeSnTopSnapshot — the taskworker task that folds each tempo
//
// The model owns the projection (model/sn_top_projection.go) and the tables
// (model/sn_top_model.go); this file owns the wire shapes, the cursor, the
// privacy rules, the tempo anchor, the task's inputs and its schedule.
//
// Claimed fleets, the validator's head summary and on-chain incentive come
// from the chain sync through snTopSources (sn_top_chain_sources.go). Before
// each has data its fields stay null.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

const (
	snTopDefaultLimit = 50
	snTopMaxLimit     = 200
	// a snapshot older than this many tempos is stale
	snTopStaleTempos = 3
	// how often the task checks for a completed tempo
	snTopCheckInterval = 5 * time.Minute
	// a tempo is folded this long after it ends, once the reliability rollup
	// has drained its last blocks and its last trails have completed
	snTopSettleDelay = 3 * time.Minute
	// never fold twice within this, whatever the tempo source says
	snTopMinFoldInterval = SnTopMinFoldInterval
	// the history days /sn/top/me returns
	snTopHistoryDays = model.SnTopHistoryDays
)

// SnTopMinFoldInterval keeps a tempo source change (the clock to the chain,
// or the chain to the validator's summary) from folding one tempo twice.
const SnTopMinFoldInterval = model.SnTopTempoDuration / 2

// Fleet binding is blocked on mainnet (LEADERTOP.md §3.6): the commitment
// oracle is not deployed and the fleet runtime authority has not shipped.
// While it is, every response says so and the claim prompt stays a "claiming
// opens soon" note (Q16). Opening it is a release decision.
const (
	snTopBindingOpen       = false
	snTopBindingClosedNote = "Top 200 binding isn't open on mainnet yet"
	snTopBindingOpenNote   = ""
)

/**
 * Sources owned by the chain sync (server-chain)
 */

// SnTopValidatorSummaryEntry is one eligible UID of the validator's head
// summary (sn docs/spec/head-summary-v1.md).
type SnTopValidatorSummaryEntry struct {
	Rank int
	Uid  uint16
	// the hotkey holding the UID at the summary's snapshot
	Hotkey   [32]byte
	Score    *big.Rat
	Selected bool
	// the live fleet identities that make up the score
	Fleets []SnTopValidatorSummaryFleet
}

type SnTopValidatorSummaryFleet struct {
	FleetId    [32]byte
	Generation uint64
}

// SnTopValidatorSummary is the validator's latest head summary.
type SnTopValidatorSummary struct {
	NativeEpoch         uint64
	NativeSnapshotBlock uint64
	SettlementEpoch     uint64
	MaximumHeadFleets   int
	// the eligible head UIDs: a rank's "of"
	EntryCount int
	ReceivedAt time.Time
	// by rank
	Entries []SnTopValidatorSummaryEntry
}

// entryFor is the summary's entry for one fleet identity: its UID's entry,
// while the identity's hotkey holds the UID and the identity is one of
// those that make up the UID's score.
func (self *SnTopValidatorSummary) entryFor(key model.SnTopFleetKey) *SnTopValidatorSummaryEntry {
	for i := range self.Entries {
		entry := &self.Entries[i]
		if entry.Uid != key.Uid || entry.Hotkey != key.Hotkey {
			continue
		}
		for _, fleet := range entry.Fleets {
			if fleet.FleetId == key.FleetId && fleet.Generation == key.Generation {
				return entry
			}
		}
	}
	return nil
}

func (self *SnTopValidatorSummary) rankOf() int {
	if 0 < self.EntryCount {
		return self.EntryCount
	}
	return len(self.Entries)
}

// SnTopIncentive is the on-chain incentive of each UID, read once a tempo.
type SnTopIncentive struct {
	At time.Time
	// each UID's Incentive as the chain stores it: a u16 share of 65535
	ByUid map[uint16]uint16
	// the hotkey holding each UID at the read, where it was read. A fleet's
	// incentive is its UID's only while the fleet's hotkey holds it: a UID
	// that changed hands earns for its new holder
	Holders map[uint16][32]byte
	// the validator's fallback row, its weights outside the activity cutoff,
	// or its trust well below 1 (LEADERTOP.md §6.2)
	Stale bool
}

// SnTopSources are the board's inputs from the chain sync. Each returns
// nothing until its source has data; until then the board's claimed fleet,
// validator and incentive fields stay null.
type SnTopSources struct {
	// the live claimed fleets of the settlement epoch, with member clients
	ClaimedFleets func(ctx context.Context, settlementEpoch uint64) []model.SnTopFleetInput
	// the validator's latest head summary, nil without one
	ValidatorSummary func(ctx context.Context) *SnTopValidatorSummary
	// the latest on-chain incentive read, nil without one
	Incentive func(ctx context.Context) *SnTopIncentive
}

// The chain sync's readers (sn_top_chain_sources.go).
var snTopSources = SnTopSources{
	ClaimedFleets:    snTopChainClaimedFleets,
	ValidatorSummary: snTopChainValidatorSummary,
	Incentive:        snTopChainIncentive,
}

// SetSnTopSources installs the chain sync's sources; nil members keep the
// current ones. It returns a function that restores the previous sources.
func SetSnTopSources(sources SnTopSources) (restore func()) {
	previous := snTopSources
	if sources.ClaimedFleets != nil {
		snTopSources.ClaimedFleets = sources.ClaimedFleets
	}
	if sources.ValidatorSummary != nil {
		snTopSources.ValidatorSummary = sources.ValidatorSummary
	}
	if sources.Incentive != nil {
		snTopSources.Incentive = sources.Incentive
	}
	return func() { snTopSources = previous }
}

/**
 * Wire shapes (CONTRACT.md)
 */

type SnTopSnapshot struct {
	Id                 string     `json:"id"`
	TempoIndex         int64      `json:"tempo_index"`
	Block              uint64     `json:"block"`
	ComputedAt         time.Time  `json:"computed_at"`
	SettlementEpoch    uint64     `json:"settlement_epoch"`
	TotalRanked        int        `json:"total_ranked"`
	HeadSeats          int        `json:"head_seats"`
	ClaimedFleets      int        `json:"claimed_fleets"`
	OpenSeats          int        `json:"open_seats"`
	ProjectedCutoff    float64    `json:"projected_cutoff"`
	ClaimedCutoff      *float64   `json:"claimed_cutoff"`
	BindingOpen        bool       `json:"binding_open"`
	BindingNote        string     `json:"binding_note"`
	MeasuredProviders  int        `json:"measured_providers"`
	CohortCapacity     int        `json:"cohort_capacity"`
	ValidatorSummaryAt *time.Time `json:"validator_summary_at"`
	IncentiveAt        *time.Time `json:"incentive_at"`
	PrecisionAt200     *float64   `json:"precision_at_200"`
	Stale              bool       `json:"stale"`
}

type SnTopFleet struct {
	Uid             int      `json:"uid"`
	Generation      uint64   `json:"generation"`
	ValidToEpoch    uint64   `json:"valid_to_epoch"`
	ReplicaScore    float64  `json:"replica_score"`
	ValidatorScore  *float64 `json:"validator_score"`
	ValidatorRank   *int     `json:"validator_rank"`
	ValidatorRankOf *int     `json:"validator_rank_of"`
	Selected        *bool    `json:"selected"`
	Incentive       *float64 `json:"incentive"`
	IncentiveStale  bool     `json:"incentive_stale"`
}

// SnTopRow is one network's row. It never carries the network id, and the
// name only for a network that turned on points_leaderboard_public (Q12).
type SnTopRow struct {
	Position       int          `json:"position"`
	Name           string       `json:"name"`
	Tag            string       `json:"tag"`
	ProjectedScore float64      `json:"projected_score"`
	RawLastTempo   float64      `json:"raw_last_tempo"`
	Margin         float64      `json:"margin"`
	Trend          int          `json:"trend"`
	Prefixes       int          `json:"prefixes"`
	SharedPrefixes int          `json:"shared_prefixes"`
	Presence       float64      `json:"presence"`
	Label          string       `json:"label"`
	Measurable     bool         `json:"measurable"`
	VerifyEligible bool         `json:"verify_eligible"`
	ClaimPrompt    bool         `json:"claim_prompt"`
	IsMe           bool         `json:"is_me"`
	Fleets         []SnTopFleet `json:"fleets"`
}

type SnTopHistoryPoint struct {
	Day   string  `json:"day"`
	Rank  int     `json:"rank"`
	Score float64 `json:"score"`
}

type SnTopMe struct {
	Row                 SnTopRow            `json:"row"`
	SoloScore           float64             `json:"solo_score"`
	DistinctPrefixes    int                 `json:"distinct_prefixes"`
	SharedPrefixes      int                 `json:"shared_prefixes"`
	SharedWithUnits     int                 `json:"shared_with_units"`
	NotMeasurableReason string              `json:"not_measurable_reason"`
	History             []SnTopHistoryPoint `json:"history"`
}

// SnTopArgs are the `GET /sn/top` query parameters.
type SnTopArgs struct {
	Limit    int    `json:"limit,omitempty"`
	Cursor   string `json:"cursor,omitempty"`
	Filter   string `json:"filter,omitempty"`
	Snapshot string `json:"snapshot,omitempty"`
}

type SnTopResult struct {
	Snapshot *SnTopSnapshot `json:"snapshot"`
	Rows     []SnTopRow     `json:"rows"`
	Me       *SnTopRow      `json:"me"`
	Next     *string        `json:"next"`
}

type SnTopMeResult struct {
	Snapshot *SnTopSnapshot `json:"snapshot"`
	Me       *SnTopMe       `json:"me"`
}

// snTopCursor pins a page sequence to one snapshot and one filter: the next
// page starts after Position.
type snTopCursor struct {
	SnapshotId server.Id `json:"s"`
	Filter     string    `json:"f"`
	Position   int       `json:"p"`
}

func encodeSnTopCursor(cursor snTopCursor) string {
	cursorJson, err := json.Marshal(cursor)
	server.Raise(err)
	return base64.RawURLEncoding.EncodeToString(cursorJson)
}

func decodeSnTopCursor(value string) (snTopCursor, error) {
	cursorJson, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(value))
	if err != nil {
		return snTopCursor{}, err
	}
	var cursor snTopCursor
	if err := json.Unmarshal(cursorJson, &cursor); err != nil {
		return snTopCursor{}, err
	}
	if _, ok := snTopFilter(cursor.Filter); !ok || cursor.Position < 0 || model.SnTopPagedPositions < cursor.Position {
		return snTopCursor{}, errors.New("invalid cursor")
	}
	return cursor, nil
}

func snTopFilter(filter string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(filter)) {
	case "", model.SnTopFilterAll:
		return model.SnTopFilterAll, true
	case model.SnTopFilterUnclaimed:
		return model.SnTopFilterUnclaimed, true
	case model.SnTopFilterClaimed:
		return model.SnTopFilterClaimed, true
	}
	return "", false
}

func snTopLimit(limit int) int {
	if limit <= 0 {
		return snTopDefaultLimit
	}
	return min(limit, snTopMaxLimit)
}

func snTopBindingNote(open bool) string {
	if open {
		return snTopBindingOpenNote
	}
	return snTopBindingClosedNote
}

func snTopSnapshotFromModel(snapshot *model.SnTopSnapshot, now time.Time) *SnTopSnapshot {
	return &SnTopSnapshot{
		Id:                 snapshot.SnapshotId.String(),
		TempoIndex:         snapshot.TempoIndex,
		Block:              snapshot.Block,
		ComputedAt:         snapshot.ComputedAt.UTC(),
		SettlementEpoch:    snapshot.SettlementEpoch,
		TotalRanked:        snapshot.TotalRanked,
		HeadSeats:          snapshot.HeadSeats,
		ClaimedFleets:      snapshot.ClaimedFleets,
		OpenSeats:          snapshot.OpenSeats,
		ProjectedCutoff:    model.SnTopFinite(snapshot.ProjectedCutoff),
		ClaimedCutoff:      snapshot.ClaimedCutoff,
		BindingOpen:        snapshot.BindingOpen,
		BindingNote:        snapshot.BindingNote,
		MeasuredProviders:  snapshot.MeasuredProviders,
		CohortCapacity:     snapshot.CohortCapacity,
		ValidatorSummaryAt: snapshot.ValidatorSummaryAt,
		IncentiveAt:        snapshot.IncentiveAt,
		PrecisionAt200:     snapshot.PrecisionAt200,
		Stale:              snTopStale(snapshot.ComputedAt, now),
	}
}

// The API's reads of the newest snapshot share one header for this long. A
// snapshot lands once a tempo, so the public board costs one header read a
// process every 30 seconds instead of one a request.
const snTopLatestSnapshotCacheTtl = 30 * time.Second

var snTopLatestSnapshotCache = struct {
	lock     sync.Mutex
	snapshot *model.SnTopSnapshot
	expiry   time.Time
}{}

// snTopLatestSnapshot is the newest snapshot header for API reads, cached
// briefly; nil before the first snapshot.
func snTopLatestSnapshot(ctx context.Context) *model.SnTopSnapshot {
	now := server.NowUtc()
	snTopLatestSnapshotCache.lock.Lock()
	if now.Before(snTopLatestSnapshotCache.expiry) {
		snapshot := snTopLatestSnapshotCache.snapshot
		snTopLatestSnapshotCache.lock.Unlock()
		return snapshot
	}
	snTopLatestSnapshotCache.lock.Unlock()
	snapshot := model.GetLatestSnTopSnapshot(ctx)
	snTopLatestSnapshotCache.lock.Lock()
	snTopLatestSnapshotCache.snapshot = snapshot
	snTopLatestSnapshotCache.expiry = now.Add(snTopLatestSnapshotCacheTtl)
	snTopLatestSnapshotCache.lock.Unlock()
	return snapshot
}

// snTopResetLatestSnapshotCache makes the next API read see the newest
// snapshot, after this process wrote one.
func snTopResetLatestSnapshotCache() {
	snTopLatestSnapshotCache.lock.Lock()
	snTopLatestSnapshotCache.snapshot = nil
	snTopLatestSnapshotCache.expiry = time.Time{}
	snTopLatestSnapshotCache.lock.Unlock()
}

func snTopStale(computedAt time.Time, now time.Time) bool {
	return snTopStaleTempos*model.SnTopTempoDuration < now.Sub(computedAt)
}

// snTopEmptySnapshot stands in before the first snapshot: nothing ranked,
// every seat open.
func snTopEmptySnapshot() *SnTopSnapshot {
	return &SnTopSnapshot{
		HeadSeats:   model.SnTopHeadSeats,
		OpenSeats:   model.SnTopHeadSeats,
		BindingOpen: snTopBindingOpen,
		BindingNote: snTopBindingNote(snTopBindingOpen),
	}
}

func snTopFleetFromModel(fleet *model.SnTopFleetRow) SnTopFleet {
	return SnTopFleet{
		Uid:             int(fleet.Key.Uid),
		Generation:      fleet.Key.Generation,
		ValidToEpoch:    fleet.ValidToEpoch,
		ReplicaScore:    model.SnTopFinite(fleet.ReplicaScore),
		ValidatorScore:  fleet.ValidatorScore,
		ValidatorRank:   fleet.ValidatorRank,
		ValidatorRankOf: fleet.ValidatorRankOf,
		Selected:        fleet.Selected,
		Incentive:       fleet.Incentive,
		IncentiveStale:  fleet.IncentiveStale,
	}
}

// snTopRowFromModel applies the privacy rules (Q12): no network id ever; the
// name only when the network turned on points_leaderboard_public and it
// passed the profanity check; the tag always.
func snTopRowFromModel(row *model.SnTopNetworkRow, fleets []*model.SnTopFleetRow, callerNetworkId *server.Id) SnTopRow {
	out := SnTopRow{
		Position:       row.Position,
		Tag:            row.EmojiTag,
		ProjectedScore: model.SnTopFinite(row.ProjectedScore),
		RawLastTempo:   model.SnTopFinite(row.RawLastTempo),
		Margin:         model.SnTopFinite(row.Margin),
		Trend:          row.Trend,
		Prefixes:       row.Prefixes,
		SharedPrefixes: row.SharedPrefixes,
		Presence:       model.SnTopFinite(row.Presence),
		Label:          row.Label,
		Measurable:     row.Measurable,
		VerifyEligible: row.VerifyEligible,
		ClaimPrompt:    row.ClaimPrompt,
		IsMe:           callerNetworkId != nil && *callerNetworkId == row.NetworkId,
		Fleets:         []SnTopFleet{},
	}
	if row.PointsLeaderboardPublic && !row.ContainsProfanity {
		out.Name = row.NetworkName
	}
	for _, fleet := range fleets {
		out.Fleets = append(out.Fleets, snTopFleetFromModel(fleet))
	}
	return out
}

func snTopCallerNetworkId(clientSession *session.ClientSession) *server.Id {
	if clientSession == nil || clientSession.ByJwt == nil {
		return nil
	}
	networkId := clientSession.ByJwt.NetworkId
	return &networkId
}

// GetSnTop pages one snapshot's top 400 (`GET /sn/top`). Public; a signed-in
// caller also gets its own row as `me`, wherever it ranks.
func GetSnTop(args *SnTopArgs, clientSession *session.ClientSession) (*SnTopResult, error) {
	ctx := clientSession.Ctx
	now := server.NowUtc()
	filter, ok := snTopFilter(args.Filter)
	if !ok {
		return nil, errors.New("400 Unknown filter.")
	}
	limit := snTopLimit(args.Limit)

	var snapshot *model.SnTopSnapshot
	afterPosition := 0
	if args.Cursor != "" {
		cursor, err := decodeSnTopCursor(args.Cursor)
		if err != nil {
			return nil, errors.New("400 Invalid cursor.")
		}
		if args.Filter != "" && cursor.Filter != filter {
			return nil, errors.New("400 The cursor belongs to another filter.")
		}
		if args.Snapshot != "" && args.Snapshot != cursor.SnapshotId.String() {
			return nil, errors.New("400 The cursor belongs to another snapshot.")
		}
		filter = cursor.Filter
		afterPosition = cursor.Position
		snapshot = model.GetSnTopSnapshot(ctx, cursor.SnapshotId)
		if snapshot == nil {
			return nil, errors.New("410 The snapshot expired. Reload from the top.")
		}
	} else if args.Snapshot != "" {
		snapshotId, err := server.ParseId(args.Snapshot)
		if err != nil {
			return nil, errors.New("400 Invalid snapshot.")
		}
		snapshot = model.GetSnTopSnapshot(ctx, snapshotId)
		if snapshot == nil {
			return nil, errors.New("404 Unknown snapshot.")
		}
	} else {
		snapshot = snTopLatestSnapshot(ctx)
	}

	result := &SnTopResult{Rows: []SnTopRow{}}
	if snapshot == nil {
		result.Snapshot = snTopEmptySnapshot()
		return result, nil
	}
	result.Snapshot = snTopSnapshotFromModel(snapshot, now)

	callerNetworkId := snTopCallerNetworkId(clientSession)
	// one extra row says whether another page exists
	rows := model.GetSnTopPage(ctx, snapshot.SnapshotId, afterPosition, limit+1, filter)
	hasNext := limit < len(rows)
	if hasNext {
		rows = rows[:limit]
	}
	var meRow *model.SnTopNetworkRow
	if callerNetworkId != nil {
		meRow = model.GetSnTopNetworkRow(ctx, snapshot.SnapshotId, *callerNetworkId)
		if meRow != nil && meRow.Position <= 0 {
			// an unranked row is not a rank
			meRow = nil
		}
	}
	networkIds := []server.Id{}
	for _, row := range rows {
		if row.Claimed {
			networkIds = append(networkIds, row.NetworkId)
		}
	}
	if meRow != nil && meRow.Claimed {
		networkIds = append(networkIds, meRow.NetworkId)
	}
	fleets := model.GetSnTopFleets(ctx, snapshot.SnapshotId, networkIds)
	for _, row := range rows {
		result.Rows = append(result.Rows, snTopRowFromModel(row, fleets[row.NetworkId], callerNetworkId))
	}
	if meRow != nil {
		me := snTopRowFromModel(meRow, fleets[meRow.NetworkId], callerNetworkId)
		result.Me = &me
	}
	if hasNext && 0 < len(rows) {
		next := encodeSnTopCursor(snTopCursor{
			SnapshotId: snapshot.SnapshotId,
			Filter:     filter,
			Position:   rows[len(rows)-1].Position,
		})
		result.Next = &next
	}
	return result, nil
}

// GetSnTopMe is the caller's detail (`GET /sn/top/me`) in the newest
// snapshot, with its daily rank history, at most 90 days. Before the first
// snapshot the board is empty and gives no not-measurable reason: it knows
// nothing about the caller yet.
func GetSnTopMe(clientSession *session.ClientSession) (*SnTopMeResult, error) {
	ctx := clientSession.Ctx
	now := server.NowUtc()
	networkId := clientSession.ByJwt.NetworkId
	snapshot := snTopLatestSnapshot(ctx)
	result := &SnTopMeResult{}
	me := &SnTopMe{
		Row: SnTopRow{
			Label:  model.SnTopLabelNotMeasurable,
			IsMe:   true,
			Fleets: []SnTopFleet{},
		},
		NotMeasurableReason: model.SnTopReasonNone,
		History:             []SnTopHistoryPoint{},
	}
	result.Me = me
	if snapshot == nil {
		result.Snapshot = snTopEmptySnapshot()
	} else {
		result.Snapshot = snTopSnapshotFromModel(snapshot, now)
		// a snapshot without the caller's row: no verify-eligible provider
		// of the caller's was present in the tempo
		me.NotMeasurableReason = model.SnTopReasonNotProviding
	}
	var row *model.SnTopNetworkRow
	if snapshot != nil {
		row = model.GetSnTopNetworkRow(ctx, snapshot.SnapshotId, networkId)
	}
	if row != nil {
		fleets := map[server.Id][]*model.SnTopFleetRow{}
		if row.Claimed {
			fleets = model.GetSnTopFleets(ctx, snapshot.SnapshotId, []server.Id{networkId})
		}
		me.Row = snTopRowFromModel(row, fleets[networkId], &networkId)
		me.SoloScore = model.SnTopFinite(row.SoloScore)
		me.DistinctPrefixes = row.Prefixes
		me.SharedPrefixes = row.SharedPrefixes
		me.SharedWithUnits = row.SharedWithUnits
		me.NotMeasurableReason = row.NotMeasurableReason
	} else {
		settings := model.GetNetworkPointsLeaderboardSettings(ctx, networkId)
		me.Row.Tag = settings.EmojiTag
	}

	since := now.AddDate(0, 0, -(snTopHistoryDays - 1))
	history := model.GetSnTopNetworkHistory(ctx, networkId, since)
	today := snTopHistoryDay(now)
	for _, point := range history {
		day := snTopHistoryDay(point.Day)
		if day == today {
			continue
		}
		me.History = append(me.History, SnTopHistoryPoint{Day: day, Rank: point.Rank, Score: model.SnTopFinite(point.Score)})
	}
	// today is the newest snapshot, not yet a recorded day
	if row != nil && 0 < row.Position {
		me.History = append(me.History, SnTopHistoryPoint{Day: today, Rank: row.Position, Score: model.SnTopFinite(row.ProjectedScore)})
	}
	if snTopHistoryDays < len(me.History) {
		me.History = me.History[len(me.History)-snTopHistoryDays:]
	}
	return result, nil
}

func snTopHistoryDay(t time.Time) string {
	return t.UTC().Format("2006-01-02")
}

/**
 * Snapshot task
 */

type ComputeSnTopSnapshotArgs struct {
}

type ComputeSnTopSnapshotResult struct {
	SnapshotId  *server.Id `json:"snapshot_id,omitempty"`
	TempoIndex  int64      `json:"tempo_index"`
	TempoSource string     `json:"tempo_source"`
	// why no snapshot was written this run, "" when one was
	Skipped           string `json:"skipped,omitempty"`
	TotalRanked       int    `json:"total_ranked"`
	EligibleProviders int    `json:"eligible_providers"`
	TrailHops         int    `json:"trail_hops"`
	RemovedTrailHops  int64  `json:"removed_trail_hops"`
}

const computeSnTopSnapshotKey = "compute_sn_top_snapshot"

// ScheduleComputeSnTopSnapshot arms the board's check chain at startup.
func ScheduleComputeSnTopSnapshot(clientSession *session.ClientSession, tx server.PgTx) {
	scheduleComputeSnTopSnapshotAfter(clientSession, tx, time.Minute)
}

func scheduleComputeSnTopSnapshotAfter(clientSession *session.ClientSession, tx server.PgTx, delay time.Duration) {
	task.ScheduleTaskInTx(
		tx,
		ComputeSnTopSnapshot,
		&ComputeSnTopSnapshotArgs{},
		clientSession,
		task.RunOnce(computeSnTopSnapshotKey),
		task.RunAt(server.NowUtc().Add(delay)),
		task.MaxTime(30*time.Minute),
	)
}

func ComputeSnTopSnapshotPost(
	args *ComputeSnTopSnapshotArgs,
	result *ComputeSnTopSnapshotResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	scheduleComputeSnTopSnapshotAfter(clientSession, tx, snTopCheckInterval)
	return nil
}

// snTopTempoAnchor anchors the tempo cadence: the chain head the st sync
// reads, else the st event sync's high-water block, each with the mirrored
// settlement epoch; nil without a chain (the wall clock).
func snTopTempoAnchor(ctx context.Context) *model.SnTopTempoAnchor {
	cfg, _, state := snChainState(ctx)
	if state != nil && 0 < state.HeadBlock && !state.HeadBlockTime.IsZero() {
		return &model.SnTopTempoAnchor{
			Source:          model.SnTopTempoSourceChain,
			Block:           state.HeadBlock,
			TimeMs:          state.HeadBlockTime.UnixMilli(),
			Epoch:           state.Epoch,
			EpochStartBlock: state.EpochStartBlock,
			EpochBlocks:     state.TEpochBlocks,
		}
	}
	if cfg == nil {
		cfg = stConfig()
	}
	if cfg == nil {
		return nil
	}
	deploymentKey := cfg.DeploymentKey()
	summary := model.GetStEpochSummaryCache(ctx, deploymentKey)
	block, at, ok := model.GetStChainSyncAnchor(ctx, deploymentKey)
	if summary == nil || !ok {
		return nil
	}
	return &model.SnTopTempoAnchor{
		Source:          model.SnTopTempoSourceStEpoch,
		Block:           block,
		TimeMs:          at.UnixMilli(),
		Epoch:           summary.Epoch,
		EpochStartBlock: summary.StartBlock,
		EpochBlocks:     summary.TEpochBlocks,
	}
}

// snTopFoldTempo is the tempo to fold now: the validator's own when its
// head summary names a newer one (exact), else the anchored cadence's last
// completed tempo.
func snTopFoldTempo(anchor *model.SnTopTempoAnchor, summary *SnTopValidatorSummary, previous *model.SnTopSnapshot, now time.Time) model.SnTopTempo {
	if snTopSummaryCurrent(summary, now) {
		start := summary.ReceivedAt.Add(-model.SnTopTempoDuration)
		if previous != nil && previous.TempoSource == model.SnTopTempoSourceSummary && previous.TempoEndTime.Before(summary.ReceivedAt) {
			start = previous.TempoEndTime
		}
		startBlock := uint64(0)
		if model.SnTopTempoBlocks <= summary.NativeSnapshotBlock {
			startBlock = summary.NativeSnapshotBlock - model.SnTopTempoBlocks
		}
		return model.SnTopTempo{
			Source:          model.SnTopTempoSourceSummary,
			Index:           int64(summary.NativeEpoch),
			StartBlock:      startBlock,
			EndBlock:        summary.NativeSnapshotBlock,
			StartTime:       start,
			EndTime:         summary.ReceivedAt,
			SettlementEpoch: summary.SettlementEpoch,
		}
	}
	return model.SnTopCompletedTempo(anchor, now)
}

// A summary drives the tempo while it keeps arriving; once the validator
// stops sending them the anchored cadence takes over again.
func snTopSummaryCurrent(summary *SnTopValidatorSummary, now time.Time) bool {
	return summary != nil && 0 < summary.NativeSnapshotBlock && !summary.ReceivedAt.IsZero() &&
		now.Sub(summary.ReceivedAt) < 2*model.SnTopTempoDuration
}

// snTopShouldFold decides whether the tempo still needs a snapshot.
func snTopShouldFold(tempo model.SnTopTempo, latest *model.SnTopSnapshot, now time.Time) (bool, string) {
	if tempo.Source != model.SnTopTempoSourceSummary && now.Before(tempo.EndTime.Add(snTopSettleDelay)) {
		return false, "settling"
	}
	if latest == nil {
		return true, ""
	}
	if latest.TempoSource == tempo.Source && tempo.Index <= latest.TempoIndex {
		return false, "folded"
	}
	if now.Sub(latest.ComputedAt) < snTopMinFoldInterval {
		return false, "recent"
	}
	return true, ""
}

// ComputeSnTopSnapshot folds the last completed tempo into a new snapshot:
// each eligible provider's pinned prefix and presence, the complete-trail
// hops, the claimed fleets and the prior EMA state in; the ranked rows, the
// next EMA state and the agreement metrics out (LEADERTOP.md §8).
func ComputeSnTopSnapshot(
	args *ComputeSnTopSnapshotArgs,
	clientSession *session.ClientSession,
) (*ComputeSnTopSnapshotResult, error) {
	ctx := clientSession.Ctx
	now := server.NowUtc()
	result := &ComputeSnTopSnapshotResult{}
	if !StEnabled() {
		result.Skipped = "st_disabled"
		return result, nil
	}
	start := time.Now()

	anchor := snTopTempoAnchor(ctx)
	if anchor != nil {
		model.SetSnTopTempoAnchor(ctx, anchor)
	}
	summary := snTopSources.ValidatorSummary(ctx)
	latest := model.GetLatestSnTopSnapshot(ctx)
	if latest != nil {
		observeSnTopSnapshotTime(latest.ComputedAt)
	}
	tempo := snTopFoldTempo(anchor, summary, latest, now)
	result.TempoIndex = tempo.Index
	result.TempoSource = tempo.Source
	if fold, reason := snTopShouldFold(tempo, latest, now); !fold {
		result.Skipped = reason
		return result, nil
	}

	settings := VerifySettings()
	inputs := snTopGatherInputs(ctx, settings, tempo, latest, summary)
	projection := model.ProjectSnTop(inputs.projection)
	snTopMeasureReplica(&inputs.agreement, projection, summary)
	snTopMeasureIncentive(&inputs.agreement, summary, inputs.incentive)

	snapshot := &model.SnTopSnapshot{
		SnapshotId:        server.NewId(),
		TempoIndex:        tempo.Index,
		TempoSource:       tempo.Source,
		Block:             tempo.EndBlock,
		TempoStartTime:    tempo.StartTime,
		TempoEndTime:      tempo.EndTime,
		ComputedAt:        now,
		SettlementEpoch:   tempo.SettlementEpoch,
		TotalRanked:       projection.TotalRanked,
		HeadSeats:         model.SnTopHeadSeats,
		ClaimedFleets:     projection.ClaimedFleets,
		OpenSeats:         projection.OpenSeats,
		ProjectedCutoff:   projection.ProjectedCutoff,
		ClaimedCutoff:     projection.ClaimedCutoff,
		BindingOpen:       snTopBindingOpen,
		BindingNote:       snTopBindingNote(snTopBindingOpen),
		EligibleProviders: projection.EligibleProviders,
		MeasuredProviders: inputs.measuredProviders,
		CohortCapacity:    inputs.cohortCapacity,
		PresenceSource:    inputs.presenceSource,
		PrecisionAt200:    inputs.projection.PrecisionAt200,
	}
	if summary != nil {
		at := summary.ReceivedAt.UTC()
		snapshot.ValidatorSummaryAt = &at
	}
	if inputs.incentive != nil {
		at := inputs.incentive.At.UTC()
		snapshot.IncentiveAt = &at
	}
	model.WriteSnTopSnapshot(ctx, snapshot, projection)
	snTopResetLatestSnapshotCache()
	result.RemovedTrailHops = model.RemoveOldSnTopData(ctx, now)

	result.SnapshotId = &snapshot.SnapshotId
	result.TotalRanked = projection.TotalRanked
	result.EligibleProviders = projection.EligibleProviders
	result.TrailHops = inputs.trailHops
	observeSnTopSnapshot(snapshot, projection, inputs, time.Since(start))
	glog.Infof(
		"[sntop]snapshot %s tempo %d (%s): %d ranked, %d eligible providers (%d measured), %d trail hops, cutoff %.4f, %d claimed fleets, presence %s, %s\n",
		snapshot.SnapshotId, tempo.Index, tempo.Source, projection.TotalRanked, projection.EligibleProviders,
		inputs.measuredProviders, inputs.trailHops, projection.ProjectedCutoff, projection.ClaimedFleets,
		inputs.presenceSource, time.Since(start),
	)
	return result, nil
}

type snTopInputs struct {
	projection        *model.SnTopProjectionInput
	measuredProviders int
	cohortCapacity    int
	presenceSource    string
	trailHops         int
	incentive         *SnTopIncentive
	agreement         snTopAgreement
}

const (
	// the per-minute reliability rows of the tempo
	snTopPresenceReliability = "reliability"
	// no drained reliability block in the tempo: every provider eligible now
	// counts as present the whole tempo
	snTopPresenceEligibleNow = "eligible_now"
)

func snTopGatherInputs(
	ctx context.Context,
	settings *model.VerifySettings,
	tempo model.SnTopTempo,
	latest *model.SnTopSnapshot,
	summary *SnTopValidatorSummary,
) *snTopInputs {
	inputs := &snTopInputs{}
	eligible := model.GetVerifyEligibleClientIds(ctx)
	_, pins := model.GetVerifyHeadPrefixPins(ctx, eligible, settings)
	presence := model.GetSnTopPresence(ctx, tempo.StartTime, tempo.EndTime)
	evidence := model.GetSnTopTrailEvidence(ctx, tempo.StartTime, tempo.EndTime)
	inputs.trailHops = evidence.Hops
	inputs.presenceSource = snTopPresenceReliability
	if presence.CoveredBlocks == 0 {
		inputs.presenceSource = snTopPresenceEligibleNow
	}
	clientIds := map[server.Id]bool{}
	for _, clientId := range eligible {
		clientIds[clientId] = true
	}
	for clientId := range evidence.Prefixes {
		clientIds[clientId] = true
	}
	lookup := []server.Id{}
	networkOf := map[server.Id]server.Id{}
	for clientId := range clientIds {
		if networkId, ok := presence.ClientNetworks[clientId]; ok {
			networkOf[clientId] = networkId
		} else if networkId, ok := evidence.Networks[clientId]; ok {
			networkOf[clientId] = networkId
		} else {
			lookup = append(lookup, clientId)
		}
	}
	for clientId, networkId := range model.GetNetworkIdsForClientIds(ctx, lookup) {
		networkOf[clientId] = networkId
	}
	eligibleSet := map[server.Id]bool{}
	for _, clientId := range eligible {
		eligibleSet[clientId] = true
	}

	// The cohort the validator measures from (verify_cohort_model.go): a
	// provider is measurable while this settlement epoch's cohort holds it.
	// Without a cohort every eligible provider can be drawn.
	cohortEnabled := 0 < settings.CohortSize
	inputs.cohortCapacity = max(0, settings.CohortSize)
	cohort := map[server.Id]bool{}
	if cohortEnabled {
		cohortClientIds := make([]server.Id, 0, len(clientIds))
		for clientId := range clientIds {
			cohortClientIds = append(cohortClientIds, clientId)
		}
		cohort, _ = model.GetVerifyCohortMembership(ctx, settings, cohortClientIds)
		if status, ok := model.GetVerifyCohortStatus(ctx, settings); ok {
			inputs.measuredProviders = status.Members
			inputs.cohortCapacity = status.Capacity
		}
	} else {
		inputs.measuredProviders = len(eligible)
	}

	providers := []model.SnTopProvider{}
	networks := map[server.Id]bool{}
	for clientId := range clientIds {
		networkId, ok := networkOf[clientId]
		if !ok {
			continue
		}
		provider := model.SnTopProvider{
			ClientId:      clientId,
			NetworkId:     networkId,
			Eligible:      eligibleSet[clientId],
			TrailPrefixes: evidence.Prefixes[clientId],
		}
		provider.PrefixHash, provider.HasPrefix = pins[clientId]
		if inputs.presenceSource == snTopPresenceEligibleNow {
			if provider.Eligible {
				provider.Presence = 1
			}
		} else {
			provider.Presence = presence.Clients[clientId]
		}
		// a provider a complete trail went through was drawn from the cohort
		provider.Measured = !cohortEnabled || cohort[clientId] || 0 < len(provider.TrailPrefixes)
		providers = append(providers, provider)
		networks[networkId] = true
	}
	sort.Slice(providers, func(i, j int) bool { return providers[i].ClientId.Less(providers[j].ClientId) })

	fleets := snTopSources.ClaimedFleets(ctx, snTopFleetEpoch(tempo))
	inputs.incentive = snTopSources.Incentive(ctx)
	fleets = snTopAttachChainData(fleets, summary, inputs.incentive)

	priorNetworks, priorFleets := model.GetSnTopEmaState(ctx)
	for networkId := range priorNetworks {
		networks[networkId] = true
	}
	networkIds := make([]server.Id, 0, len(networks))
	for networkId := range networks {
		networkIds = append(networkIds, networkId)
	}

	inputs.agreement.precisionAt200 = snTopMeasurePrecision(ctx, latest, fleets, summary)
	inputs.projection = &model.SnTopProjectionInput{
		TempoIndex:         tempo.Index,
		Providers:          providers,
		ProvidingNetworks:  presence.ProvidingNetworks,
		Fleets:             fleets,
		NetworkCreateTimes: model.GetNetworkCreateTimes(ctx, networkIds),
		PriorNetworks:      priorNetworks,
		PriorFleets:        priorFleets,
		PrecisionAt200:     inputs.agreement.precisionAt200,
		Math:               model.GetSnTopHeadMath(),
	}
	return inputs
}

// snTopAttachChainData copies the validator's summary and the on-chain
// incentive onto each fleet: the summary's entry for the fleet's identity,
// and its UID's incentive while its hotkey holds the UID.
func snTopAttachChainData(fleets []model.SnTopFleetInput, summary *SnTopValidatorSummary, incentive *SnTopIncentive) []model.SnTopFleetInput {
	for i := range fleets {
		fleet := &fleets[i]
		if summary != nil {
			rankOf := summary.rankOf()
			selected := false
			score := 0.0
			fleet.ValidatorRankOf = &rankOf
			if entry := summary.entryFor(fleet.Key); entry != nil {
				rank := entry.Rank
				fleet.ValidatorRank = &rank
				selected = entry.Selected
				if entry.Score != nil {
					score, _ = entry.Score.Float64()
				}
			}
			fleet.ValidatorScore = &score
			fleet.Selected = &selected
		}
		if incentive != nil {
			holder, holderRead := incentive.Holders[fleet.Key.Uid]
			if raw, ok := incentive.ByUid[fleet.Key.Uid]; ok && holderRead && holder == fleet.Key.Hotkey {
				value := SnTopIncentiveFraction(raw)
				fleet.Incentive = &value
			}
			fleet.IncentiveStale = incentive.Stale
		}
	}
	return fleets
}

// SnTopIncentiveFraction is a UID's on-chain incentive as the board serves
// it: its share of SN25 incentive, a fraction from 0 to 1 (contract
// amendment 2026-10-29; the apps show 0.018 as "1.8%"). Subtensor stores
// Incentive as a u16 share of 65535.
func SnTopIncentiveFraction(raw uint16) float64 {
	return float64(raw) / float64(^uint16(0))
}

// snTopAgreement is the board's agreement with the validator and the chain
// (LEADERTOP.md §6.3). Each check is unavailable (its counts 0, its value
// nil) until its source has data.
type snTopAgreement struct {
	// the replica against the validator's exact score, by UID
	replicaMatches    int
	replicaMismatches int
	// the previous snapshot's projected top 200 against the validator's
	// selection, among claimed networks
	precisionAt200 *float64
	// selected UIDs earning on-chain incentive
	selectedEarning    int
	selectedNotEarning int
}

func snTopSelectedUids(summary *SnTopValidatorSummary) map[uint16]bool {
	selectedUids := map[uint16]bool{}
	if summary != nil {
		for _, entry := range summary.Entries {
			if entry.Selected {
				selectedUids[entry.Uid] = true
			}
		}
	}
	return selectedUids
}

// snTopMeasurePrecision is the precision at 200 of the previous snapshot's
// projection: of the claimed networks it projected inside the head, the
// share whose fleet the validator then selected. Nil without a summary or a
// claimed network projected inside.
func snTopMeasurePrecision(ctx context.Context, latest *model.SnTopSnapshot, fleets []model.SnTopFleetInput, summary *SnTopValidatorSummary) *float64 {
	if latest == nil || summary == nil || len(fleets) == 0 {
		return nil
	}
	selectedUids := snTopSelectedUids(summary)
	networkUids := map[server.Id][]uint16{}
	networkIds := []server.Id{}
	for _, fleet := range fleets {
		for _, networkId := range fleet.NetworkIds {
			if networkUids[networkId] == nil {
				networkIds = append(networkIds, networkId)
			}
			networkUids[networkId] = append(networkUids[networkId], fleet.Key.Uid)
		}
	}
	projectedInside, selected := 0, 0
	for _, networkId := range networkIds {
		row := model.GetSnTopNetworkRow(ctx, latest.SnapshotId, networkId)
		if row == nil || row.Position <= 0 || model.SnTopHeadSeats < row.Position {
			continue
		}
		projectedInside += 1
		for _, uid := range networkUids[networkId] {
			if selectedUids[uid] {
				selected += 1
				break
			}
		}
	}
	if projectedInside == 0 {
		return nil
	}
	precision := float64(selected) / float64(projectedInside)
	return &precision
}

// snTopMeasureReplica compares the replica just folded with the validator's
// exact score for the same tempo, UID by UID. Exact equality is expected.
func snTopMeasureReplica(agreement *snTopAgreement, projection *model.SnTopProjection, summary *SnTopValidatorSummary) {
	if summary == nil {
		return
	}
	replicaByUid := map[uint16]*big.Rat{}
	for _, fleet := range projection.Fleets {
		uid := fleet.Fleet.Key.Uid
		if replicaByUid[uid] == nil {
			replicaByUid[uid] = new(big.Rat)
		}
		if fleet.ReplicaExact != nil {
			replicaByUid[uid].Add(replicaByUid[uid], fleet.ReplicaExact)
		}
	}
	for _, entry := range summary.Entries {
		replica, ok := replicaByUid[entry.Uid]
		if !ok || entry.Score == nil {
			continue
		}
		if replica.Cmp(entry.Score) == 0 {
			agreement.replicaMatches += 1
		} else {
			agreement.replicaMismatches += 1
		}
	}
}

// snTopMeasureIncentive counts the selected UIDs that earn on-chain incentive.
func snTopMeasureIncentive(agreement *snTopAgreement, summary *SnTopValidatorSummary, incentive *SnTopIncentive) {
	if summary == nil || incentive == nil {
		return
	}
	for uid := range snTopSelectedUids(summary) {
		if 0 < incentive.ByUid[uid] {
			agreement.selectedEarning += 1
		} else {
			agreement.selectedNotEarning += 1
		}
	}
}

/**
 * Head estimate (`GET /sn/head`) from the newest snapshot
 */

// snTopHeadFromSnapshot fills the head estimate from the snapshot the board
// shows, so /sn/head, the Top 200 page and the Protocol tab agree. It
// reports false when there is no snapshot yet.
func snTopHeadFromSnapshot(ctx context.Context, networkId server.Id, result *SnHeadResult) (bound snHeadBound, ok bool) {
	snapshot := snTopLatestSnapshot(ctx)
	if snapshot == nil {
		return
	}
	ok = true
	result.Floor = model.SnTopFinite(snapshot.ProjectedCutoff)
	row := model.GetSnTopNetworkRow(ctx, snapshot.SnapshotId, networkId)
	if row == nil || row.Position <= 0 {
		return
	}
	result.Score = model.SnTopFinite(row.ProjectedScore)
	result.RankEstimate = row.Position
	result.Eligible = 0 < result.Score && row.Position <= SnHeadCutoff
	if row.Claimed {
		if fleets := model.GetSnTopFleets(ctx, snapshot.SnapshotId, []server.Id{networkId})[networkId]; 0 < len(fleets) {
			bound = snHeadBound{bound: true, hotkey: fleets[0].Key.Hotkey, uid: uint64(fleets[0].Key.Uid)}
		}
	}
	return
}

// snTopSafe turns a failed read into a missing snapshot, so /sn/head keeps
// its old degraded answer instead of an error page.
func snTopSafe[R any](read func() (R, bool)) (value R, ok bool) {
	defer func() {
		if r := recover(); r != nil {
			glog.Infof("[sntop]snapshot read failed: %v\n", r)
			var empty R
			value, ok = empty, false
		}
	}()
	return read()
}
