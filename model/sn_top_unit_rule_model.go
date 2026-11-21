package model

// What the SN Top board's unit rule (a) reads and writes besides the export's
// admission set (sn_top_admission_model.go, owner decisions 2026-11-20):
//
//   - the autonomous systems of each provider's networks, which the connect
//     announce records for probed providers (SetProviderConnectionNetworkAsn),
//     for the network-level UDP excuse;
//   - the shadow board: until the rule's first settlement epoch the live board
//     folds without the rule and a shadow board with it, so everyone can see
//     the change an epoch ahead (GET /sn/top?board=shadow); the shadow keeps
//     its own network EMA state, which the live board adopts at the switch;
//   - each network's unit status from the newest fold: which providers count
//     and why the others do not (GET /sn/top/me).

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/server"
)

// The first settlement epoch whose board the rule ranks. The fold before it is
// the shadow run: the epoch before it, in full, when the rule is deployed
// before that epoch starts. A dated switch in code, so the switch needs no
// release and no setting (owner decision 2026-11-20).
const SnTopUnitRuleFirstEpoch uint64 = 9

// Whether the rule ranks a board of the settlement epoch.
func SnTopUnitRuleLive(settlementEpoch uint64) bool {
	return SnTopUnitRuleFirstEpoch <= settlementEpoch
}

// GetProviderConnectionNetworkAsnsForClients returns the autonomous systems
// recorded for each client's networks (SetProviderConnectionNetworkAsn), each
// system once, read 512 clients at a time. A client with none recorded is
// absent.
func GetProviderConnectionNetworkAsnsForClients(ctx context.Context, clientIds []server.Id) map[server.Id][]uint32 {
	asns := map[server.Id][]uint32{}
	if len(clientIds) == 0 {
		return asns
	}
	server.Redis(ctx, func(r server.RedisClient) {
		for start := 0; start < len(clientIds); start += 512 {
			chunk := clientIds[start:min(start+512, len(clientIds))]
			cmds := make([]*redis.MapStringStringCmd, len(chunk))
			_, err := r.Pipelined(ctx, func(pipe redis.Pipeliner) error {
				for i, clientId := range chunk {
					cmds[i] = pipe.HGetAll(ctx, providerConnectionNetworkAsnKey(clientId))
				}
				return nil
			})
			if err != nil && !errors.Is(err, redis.Nil) {
				server.Raise(err)
			}
			for i, clientId := range chunk {
				values, err := cmds[i].Result()
				if errors.Is(err, redis.Nil) {
					continue
				}
				server.Raise(err)
				seen := map[uint32]bool{}
				for field, value := range values {
					if addressHash, err := hex.DecodeString(field); err != nil || len(addressHash) != 32 {
						continue
					}
					asn, err := strconv.ParseUint(value, 10, 32)
					if err != nil || asn == 0 || seen[uint32(asn)] {
						continue
					}
					seen[uint32(asn)] = true
					asns[clientId] = append(asns[clientId], uint32(asn))
				}
			}
		}
	})
	return asns
}

/**
 * The shadow board
 */

const (
	snTopShadowBoardKey  = "{sn_top_shadow}board"
	snTopShadowStatesKey = "{sn_top_shadow}states"
	// the shadow outlives an idle fold by a day; its states restart past it
	snTopShadowTtl = 24 * time.Hour
	// the rows the shadow board keeps, as the board pages the top 400
	SnTopShadowRows = SnTopPagedPositions
)

// One network's row on the shadow board, beside its live row.
type SnTopShadowRow struct {
	NetworkId         server.Id `json:"network_id"`
	Position          int       `json:"position"`
	ProjectedScore    float64   `json:"projected_score"`
	RawLastTempo      float64   `json:"raw_last_tempo"`
	Prefixes          int       `json:"prefixes"`
	Label             string    `json:"label"`
	Claimed           bool      `json:"claimed"`
	LivePosition      int       `json:"live_position"`
	LivePrefixes      int       `json:"live_prefixes"`
	WithheldProviders int       `json:"withheld_providers"`
}

// The shadow board of one fold.
type SnTopShadowBoard struct {
	TempoIndex      int64            `json:"tempo_index"`
	SettlementEpoch uint64           `json:"settlement_epoch"`
	ComputedAt      time.Time        `json:"computed_at"`
	RuleFirstEpoch  uint64           `json:"rule_first_epoch"`
	TotalRanked     int              `json:"total_ranked"`
	Rows            []SnTopShadowRow `json:"rows"`
}

type snTopShadowState struct {
	NetworkId server.Id      `json:"network_id"`
	State     SnTopUnitState `json:"state"`
}

type snTopShadowStates struct {
	TempoIndex int64              `json:"tempo_index"`
	Networks   []snTopShadowState `json:"networks"`
}

// SetSnTopShadow stores a fold's shadow board and the shadow's network states.
func SetSnTopShadow(ctx context.Context, board *SnTopShadowBoard, networkStates map[server.Id]SnTopUnitState) {
	boardJson, err := json.Marshal(board)
	server.Raise(err)
	states := &snTopShadowStates{TempoIndex: board.TempoIndex, Networks: make([]snTopShadowState, 0, len(networkStates))}
	for networkId, state := range networkStates {
		states.Networks = append(states.Networks, snTopShadowState{NetworkId: networkId, State: state})
	}
	statesJson, err := json.Marshal(states)
	server.Raise(err)
	server.Redis(ctx, func(r server.RedisClient) {
		server.Raise(r.Set(ctx, snTopShadowBoardKey, boardJson, snTopShadowTtl).Err())
		server.Raise(r.Set(ctx, snTopShadowStatesKey, statesJson, snTopShadowTtl).Err())
	})
}

// GetSnTopShadowBoard returns the newest shadow board, nil without one.
func GetSnTopShadowBoard(ctx context.Context) (board *SnTopShadowBoard, returnErr error) {
	server.Redis(ctx, func(r server.RedisClient) {
		value, err := r.Get(ctx, snTopShadowBoardKey).Bytes()
		if errors.Is(err, redis.Nil) {
			return
		}
		if err != nil {
			returnErr = err
			return
		}
		board = &SnTopShadowBoard{}
		if err := json.Unmarshal(value, board); err != nil {
			board, returnErr = nil, err
		}
	})
	return
}

// GetSnTopShadowStates returns the shadow's network states and the tempo they
// follow, nil without them.
func GetSnTopShadowStates(ctx context.Context) (tempoIndex int64, networks map[server.Id]SnTopUnitState, returnErr error) {
	server.Redis(ctx, func(r server.RedisClient) {
		value, err := r.Get(ctx, snTopShadowStatesKey).Bytes()
		if errors.Is(err, redis.Nil) {
			return
		}
		if err != nil {
			returnErr = err
			return
		}
		states := &snTopShadowStates{}
		if err := json.Unmarshal(value, states); err != nil {
			returnErr = err
			return
		}
		tempoIndex = states.TempoIndex
		networks = make(map[server.Id]SnTopUnitState, len(states.Networks))
		for _, state := range states.Networks {
			networks[state.NetworkId] = state.State
		}
	})
	return
}

/**
 * Each network's unit status
 */

// the providers a status lists by name, the rest only counted
const SnTopNetworkUnitsListLimit = 2000

// One withheld provider and why.
type SnTopProviderUnit struct {
	ClientId server.Id `json:"client_id"`
	Reason   string    `json:"reason"`
}

// A network's units at the newest fold.
type SnTopNetworkUnits struct {
	TempoIndex     int64     `json:"tempo_index"`
	ComputedAt     time.Time `json:"computed_at"`
	RuleLive       bool      `json:"rule_live"`
	RuleFirstEpoch uint64    `json:"rule_first_epoch"`
	// providers whose prefix counts, and the withheld ones by reason
	CountedProviders  int                 `json:"counted_providers"`
	WithheldByReason  map[string]int      `json:"withheld_by_reason"`
	WithheldProviders []SnTopProviderUnit `json:"withheld_providers"`
}

func snTopNetworkUnitsKey(networkId server.Id) string {
	return fmt.Sprintf("{sn_top_units_%s}", networkId)
}

// SetSnTopNetworkUnits stores each network's units of a fold, for a day.
func SetSnTopNetworkUnits(ctx context.Context, units map[server.Id]*SnTopNetworkUnits) {
	if len(units) == 0 {
		return
	}
	type entry struct {
		key   string
		value []byte
	}
	entries := make([]entry, 0, len(units))
	for networkId, networkUnits := range units {
		value, err := json.Marshal(networkUnits)
		server.Raise(err)
		entries = append(entries, entry{key: snTopNetworkUnitsKey(networkId), value: value})
	}
	server.Redis(ctx, func(r server.RedisClient) {
		for start := 0; start < len(entries); start += 512 {
			chunk := entries[start:min(start+512, len(entries))]
			_, err := r.Pipelined(ctx, func(pipe redis.Pipeliner) error {
				for _, e := range chunk {
					pipe.Set(ctx, e.key, e.value, snTopShadowTtl)
				}
				return nil
			})
			server.Raise(err)
		}
	})
}

// GetSnTopNetworkUnits returns the network's units at the newest fold, nil
// without them.
func GetSnTopNetworkUnits(ctx context.Context, networkId server.Id) (units *SnTopNetworkUnits, returnErr error) {
	server.Redis(ctx, func(r server.RedisClient) {
		value, err := r.Get(ctx, snTopNetworkUnitsKey(networkId)).Bytes()
		if errors.Is(err, redis.Nil) {
			return
		}
		if err != nil {
			returnErr = err
			return
		}
		units = &SnTopNetworkUnits{}
		if err := json.Unmarshal(value, units); err != nil {
			units, returnErr = nil, err
		}
	})
	return
}

/**
 * Display settings for the shadow board's rows
 */

// The display settings the live board joins for its rows (snTopNetworkRowSelect).
type SnTopNetworkDisplay struct {
	NetworkName             string
	EmojiTag                string
	PointsLeaderboardPublic bool
	ContainsProfanity       bool
}

// GetSnTopNetworkDisplays reads the display settings of the networks.
func GetSnTopNetworkDisplays(ctx context.Context, networkIds []server.Id) map[server.Id]*SnTopNetworkDisplay {
	displays := map[server.Id]*SnTopNetworkDisplay{}
	if len(networkIds) == 0 {
		return displays
	}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT
					network_id,
					COALESCE(network_name, ''),
					COALESCE(emoji_tag, ''),
					COALESCE(points_leaderboard_public, false),
					COALESCE(contains_profanity, false)
				FROM network
				WHERE network_id = ANY($1)
			`,
			networkIds,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var networkId server.Id
				display := &SnTopNetworkDisplay{}
				server.Raise(result.Scan(&networkId, &display.NetworkName, &display.EmojiTag, &display.PointsLeaderboardPublic, &display.ContainsProfanity))
				displays[networkId] = display
			}
		})
	})
	return displays
}
