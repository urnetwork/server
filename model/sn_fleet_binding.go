// The coordinator's live fleet binding census for the SN top board
// (LEADERTOP.md §5.7). A continuous finality-safe sync
// (controller/sn_fleet_binding.go) mirrors every FleetBound,
// FleetBindingRevoked and FleetBindingCleaned log into st_event and applies
// it, in log order and in the same transaction as its checkpoint, to one row
// per binding version in sn_fleet_binding. The rows replay the coordinator's
// own rules (sn evm/src/STCoordinator.sol bindFleetMember, revokeFleetBinding,
// cleanupFleetBinding and bindingAt):
//   - a client's versions are appended in bind order, each with a strictly
//     larger generation;
//   - a revocation shortens its generation's validity to end before the
//     effective epoch, and only ever shortens it;
//   - a cleanup ends the client's latest version at its cleanup epoch;
//   - bindingAt reads the latest version already valid at an epoch.
//
// bindingAt also requires that the binding's hotkey still holds its UID. That
// test needs native storage and is applied with the incentive reader's
// metagraph census (SnClaimedFleetsAt).
//
// The existing FleetBound-only scan of settlement v2 head exclusion
// (st_fleet_bound.go) keeps its own checkpoint and is unchanged; both write the
// same FleetBound rows, which st_event admits once.
package model

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/urnetwork/server"
)

// The st_event kinds of the two binding events the event mirror does not
// decode. FleetBound rows use StFleetBoundEventKind.
const (
	SnFleetBindingRevokedEventKind = "FleetBindingRevoked"
	SnFleetBindingCleanedEventKind = "FleetBindingCleaned"
)

// A mirrored FleetBindingRevoked's data_json, encoded like every mirrored
// event: bytes as 0x-hex and integers as decimal strings.
type SnFleetBindingRevokedEventData struct {
	ClientId       string `json:"client_id"`
	Generation     string `json:"generation"`
	EffectiveEpoch string `json:"effective_epoch"`
}

// A mirrored FleetBindingCleaned's data_json.
type SnFleetBindingCleanedEventData struct {
	ClientId       string `json:"client_id"`
	CleanedAtEpoch string `json:"cleaned_at_epoch"`
}

// The binding sync's progress: every binding log before NextBlock is applied,
// and BlockHash is the canonical hash of block NextBlock-1 when it was.
type SnFleetBindingCheckpoint struct {
	NextBlock uint64
	BlockHash string
}

// A range whose logs contradict the census applied so far: a revocation or
// cleanup of a version the census never saw, a second cleanup, or two binds
// of one generation. The range is not applied and the checkpoint stays.
var ErrSnFleetBindingCensus = errors.New("fleet binding census contradicts the coordinator")

type snFleetBindingCensusPanic struct {
	err error
}

// One binding version of one client.
type SnFleetBindingVersion struct {
	ClientId   server.Id
	Generation uint64
	FleetId    [32]byte
	Hotkey     [32]byte
	// the UID the hotkey held when the binding was made
	Uid            uint16
	ValidFromEpoch uint64
	// as signed and bound
	SignedValidToEpoch uint64
	// after every revocation
	ValidToEpoch uint64
	Revoked      bool
	Cleaned      bool
	// with Cleaned, the first epoch the version is no longer active
	CleanedAtEpoch uint64
	BoundBlock     uint64
	BoundLogIndex  int
}

// bindingAt's window test without its live UID test: active at epoch when
// the epoch is within the validity and before any cleanup.
func (self *SnFleetBindingVersion) activeAt(epoch uint64) bool {
	return self.ValidFromEpoch <= epoch && epoch <= self.ValidToEpoch && (!self.Cleaned || epoch < self.CleanedAtEpoch)
}

// One parsed binding event.
type snFleetBindingEvent struct {
	event      *StChainEvent
	clientId   server.Id
	generation uint64
	// FleetBound
	fleetId   [32]byte
	hotkey    [32]byte
	uid       uint16
	validFrom uint64
	validTo   uint64
	// FleetBindingRevoked
	effectiveEpoch uint64
	// FleetBindingCleaned
	cleanedAtEpoch uint64
}

func snParseClientId(value string) (server.Id, error) {
	raw, err := hex.DecodeString(strings.TrimPrefix(value, "0x"))
	if err != nil || len(raw) != 16 || !strings.HasPrefix(value, "0x") {
		return server.Id{}, fmt.Errorf("client id %q is not 16 bytes", value)
	}
	return server.IdFromBytes(raw)
}

func snParseBytes32(label string, value string) ([32]byte, error) {
	raw, err := hex.DecodeString(strings.TrimPrefix(value, "0x"))
	if err != nil || len(raw) != 32 || !strings.HasPrefix(value, "0x") {
		return [32]byte{}, fmt.Errorf("%s %q is not 32 bytes", label, value)
	}
	return [32]byte(raw), nil
}

func snParseDecimal(label string, value string, maximum uint64) (uint64, error) {
	parsed, err := strconv.ParseUint(value, 10, 64)
	if err != nil || maximum < parsed || strconv.FormatUint(parsed, 10) != value {
		return 0, fmt.Errorf("%s %q is not a decimal at most %d", label, value, maximum)
	}
	return parsed, nil
}

// Parses one mirrored binding event of any of the three kinds.
func parseSnFleetBindingEvent(event *StChainEvent) (*snFleetBindingEvent, error) {
	if event == nil {
		return nil, errors.New("binding event is nil")
	}
	parsed := &snFleetBindingEvent{event: event}
	var err error
	switch event.Kind {
	case StFleetBoundEventKind:
		var data StFleetBoundEventData
		if err := json.Unmarshal([]byte(event.DataJson), &data); err != nil {
			return nil, err
		}
		var uid uint64
		parsed.clientId, err = snParseClientId(data.ClientId)
		if err == nil {
			parsed.fleetId, err = snParseBytes32("fleet id", data.FleetId)
		}
		if err == nil {
			parsed.hotkey, err = snParseBytes32("hotkey", data.Hotkey)
		}
		if err == nil {
			uid, err = snParseDecimal("uid", data.Uid, math.MaxUint16)
			parsed.uid = uint16(uid)
		}
		if err == nil {
			parsed.generation, err = snParseDecimal("generation", data.Generation, math.MaxUint64)
		}
		if err == nil {
			parsed.validFrom, err = snParseDecimal("valid from epoch", data.ValidFromEpoch, math.MaxUint64)
		}
		if err == nil {
			parsed.validTo, err = snParseDecimal("valid to epoch", data.ValidToEpoch, math.MaxUint64)
		}
	case SnFleetBindingRevokedEventKind:
		var data SnFleetBindingRevokedEventData
		if err := json.Unmarshal([]byte(event.DataJson), &data); err != nil {
			return nil, err
		}
		parsed.clientId, err = snParseClientId(data.ClientId)
		if err == nil {
			parsed.generation, err = snParseDecimal("generation", data.Generation, math.MaxUint64)
		}
		if err == nil {
			parsed.effectiveEpoch, err = snParseDecimal("effective epoch", data.EffectiveEpoch, math.MaxUint64)
		}
		if err == nil && parsed.effectiveEpoch == 0 {
			// the coordinator requires an effective epoch after the current one
			err = errors.New("revocation effective at epoch 0")
		}
	case SnFleetBindingCleanedEventKind:
		var data SnFleetBindingCleanedEventData
		if err := json.Unmarshal([]byte(event.DataJson), &data); err != nil {
			return nil, err
		}
		parsed.clientId, err = snParseClientId(data.ClientId)
		if err == nil {
			parsed.cleanedAtEpoch, err = snParseDecimal("cleaned at epoch", data.CleanedAtEpoch, math.MaxUint64)
		}
	default:
		return nil, fmt.Errorf("kind %q is not a binding event", event.Kind)
	}
	if err != nil {
		return nil, err
	}
	return parsed, nil
}

// The binding sync's checkpoint; zero before its first range.
func GetSnFleetBindingCheckpoint(ctx context.Context, deploymentKey StDeploymentKey) SnFleetBindingCheckpoint {
	key := requireStDeploymentKey(deploymentKey)
	checkpoint := SnFleetBindingCheckpoint{}
	server.Db(ctx, func(conn server.PgConn) {
		rows, err := conn.Query(ctx, `SELECT next_block, block_hash FROM sn_fleet_binding_sync WHERE deployment_key = $1`, key)
		server.WithPgResult(rows, err, func() {
			if rows.Next() {
				var nextBlock int64
				server.Raise(rows.Scan(&nextBlock, &checkpoint.BlockHash))
				checkpoint.NextBlock = uint64(nextBlock)
			}
		})
	})
	return checkpoint
}

// ApplySnFleetBindingRange applies one finalized, canonical range of binding
// logs: it mirrors them into st_event, applies them in log order to the
// binding versions and moves the checkpoint, in one transaction. A range that
// contradicts the census returns ErrSnFleetBindingCensus and changes nothing.
func ApplySnFleetBindingRange(ctx context.Context, deploymentKey StDeploymentKey, events []*StChainEvent, next SnFleetBindingCheckpoint) (returnErr error) {
	key := requireStDeploymentKey(deploymentKey)
	if next.NextBlock == 0 || next.NextBlock > math.MaxInt64 || next.BlockHash == "" {
		return errors.New("fleet binding checkpoint is empty")
	}
	parsed := make([]*snFleetBindingEvent, 0, len(events))
	for _, event := range events {
		bindingEvent, err := parseSnFleetBindingEvent(event)
		if err != nil {
			return fmt.Errorf("binding event %d:%d: %w", event.BlockNumber, event.LogIndex, err)
		}
		if next.NextBlock <= event.BlockNumber {
			return fmt.Errorf("binding event %d:%d is at or after the checkpoint %d", event.BlockNumber, event.LogIndex, next.NextBlock)
		}
		parsed = append(parsed, bindingEvent)
	}
	slices.SortStableFunc(parsed, func(a *snFleetBindingEvent, b *snFleetBindingEvent) int {
		if a.event.BlockNumber != b.event.BlockNumber {
			if a.event.BlockNumber < b.event.BlockNumber {
				return -1
			}
			return 1
		}
		return a.event.LogIndex - b.event.LogIndex
	})
	defer func() {
		if r := recover(); r != nil {
			if census, ok := r.(*snFleetBindingCensusPanic); ok {
				returnErr = census.err
				return
			}
			panic(r)
		}
	}()
	server.Tx(ctx, func(tx server.PgTx) {
		upsertStEventsInTx(ctx, tx, key, events)
		for _, event := range parsed {
			applySnFleetBindingEventInTx(ctx, tx, key, event)
		}
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO sn_fleet_binding_sync (deployment_key, next_block, block_hash, update_time)
				VALUES ($1, $2, $3, $4)
				ON CONFLICT (deployment_key) DO UPDATE
				SET next_block = EXCLUDED.next_block, block_hash = EXCLUDED.block_hash, update_time = EXCLUDED.update_time
			`,
			key,
			int64(next.NextBlock),
			next.BlockHash,
			server.NowUtc(),
		))
	})
	return nil
}

func snFleetBindingCensusFail(format string, args ...any) {
	panic(&snFleetBindingCensusPanic{err: fmt.Errorf("%w: %s", ErrSnFleetBindingCensus, fmt.Sprintf(format, args...))})
}

func applySnFleetBindingEventInTx(ctx context.Context, tx server.PgTx, key string, event *snFleetBindingEvent) {
	block, logIndex := int64(event.event.BlockNumber), event.event.LogIndex
	switch event.event.Kind {
	case StFleetBoundEventKind:
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO sn_fleet_binding (
					deployment_key, client_id, generation, fleet_id, hotkey, uid,
					valid_from_epoch, signed_valid_to_epoch, valid_to_epoch,
					bound_block, bound_log_index
				)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $8, $9, $10)
				ON CONFLICT (deployment_key, client_id, generation) DO NOTHING
			`,
			key, event.clientId, int64(event.generation), event.fleetId[:], event.hotkey[:], int(event.uid),
			int64(event.validFrom), int64(event.validTo), block, logIndex,
		))
		var boundBlock int64
		var boundLogIndex int
		rows, err := tx.Query(ctx, `SELECT bound_block, bound_log_index FROM sn_fleet_binding WHERE deployment_key = $1 AND client_id = $2 AND generation = $3`, key, event.clientId, int64(event.generation))
		server.WithPgResult(rows, err, func() {
			if rows.Next() {
				server.Raise(rows.Scan(&boundBlock, &boundLogIndex))
			}
		})
		if boundBlock != block || boundLogIndex != logIndex {
			snFleetBindingCensusFail("client %s generation %d is bound at %d:%d and again at %d:%d", event.clientId, event.generation, boundBlock, boundLogIndex, block, logIndex)
		}
		// a strictly larger generation than every earlier version
		for _, prior := range getSnFleetBindingVersionsInTx(ctx, tx, key, &event.clientId) {
			if (prior.BoundBlock < uint64(block) || prior.BoundBlock == uint64(block) && prior.BoundLogIndex < logIndex) && event.generation <= prior.Generation {
				snFleetBindingCensusFail("client %s generation %d at %d:%d does not exceed generation %d", event.clientId, event.generation, block, logIndex, prior.Generation)
			}
		}
	case SnFleetBindingRevokedEventKind:
		var validTo int64
		var revoked *int64
		found := false
		rows, err := tx.Query(
			ctx,
			`
				SELECT valid_to_epoch, revoked_effective_epoch FROM sn_fleet_binding
				WHERE deployment_key = $1 AND client_id = $2 AND generation = $3 AND (bound_block, bound_log_index) < ($4, $5)
				FOR UPDATE
			`,
			key, event.clientId, int64(event.generation), block, logIndex,
		)
		server.WithPgResult(rows, err, func() {
			if rows.Next() {
				found = true
				server.Raise(rows.Scan(&validTo, &revoked))
			}
		})
		if !found {
			snFleetBindingCensusFail("revocation at %d:%d of client %s generation %d, which was never bound", block, logIndex, event.clientId, event.generation)
		}
		// a revocation only shortens a validity, so applying one twice or out
		// of order leaves the same result
		end := min(uint64(validTo), event.effectiveEpoch-1)
		effective := event.effectiveEpoch
		if revoked != nil {
			effective = min(uint64(*revoked), effective)
		}
		server.RaisePgResult(tx.Exec(
			ctx,
			`UPDATE sn_fleet_binding SET valid_to_epoch = $4, revoked_effective_epoch = $5 WHERE deployment_key = $1 AND client_id = $2 AND generation = $3`,
			key, event.clientId, int64(event.generation), int64(end), int64(effective),
		))
	case SnFleetBindingCleanedEventKind:
		var generation int64
		var cleanedBlock *int64
		var cleanedLogIndex *int
		found := false
		rows, err := tx.Query(
			ctx,
			`
				SELECT generation, cleaned_block, cleaned_log_index FROM sn_fleet_binding
				WHERE deployment_key = $1 AND client_id = $2 AND (bound_block, bound_log_index) < ($3, $4)
				ORDER BY bound_block DESC, bound_log_index DESC
				LIMIT 1
				FOR UPDATE
			`,
			key, event.clientId, block, logIndex,
		)
		server.WithPgResult(rows, err, func() {
			if rows.Next() {
				found = true
				server.Raise(rows.Scan(&generation, &cleanedBlock, &cleanedLogIndex))
			}
		})
		if !found {
			snFleetBindingCensusFail("cleanup at %d:%d of client %s, which was never bound", block, logIndex, event.clientId)
		}
		if cleanedBlock != nil {
			if *cleanedBlock == block && cleanedLogIndex != nil && *cleanedLogIndex == logIndex {
				return
			}
			snFleetBindingCensusFail("cleanup at %d:%d of client %s generation %d, which was already cleaned at %d", block, logIndex, event.clientId, uint64(generation), *cleanedBlock)
		}
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				UPDATE sn_fleet_binding SET cleaned_at_epoch = $4, cleaned_block = $5, cleaned_log_index = $6
				WHERE deployment_key = $1 AND client_id = $2 AND generation = $3
			`,
			key, event.clientId, generation, int64(event.cleanedAtEpoch), block, logIndex,
		))
	}
}

const snFleetBindingVersionColumns = `
	client_id, generation, fleet_id, hotkey, uid, valid_from_epoch,
	signed_valid_to_epoch, valid_to_epoch, revoked_effective_epoch,
	cleaned_at_epoch, bound_block, bound_log_index
`

func scanSnFleetBindingVersion(rows server.PgResult) *SnFleetBindingVersion {
	version := &SnFleetBindingVersion{}
	var generation, validFrom, signedValidTo, validTo, boundBlock int64
	var revoked, cleanedAt *int64
	var fleetId, hotkey []byte
	var uid int
	server.Raise(rows.Scan(&version.ClientId, &generation, &fleetId, &hotkey, &uid, &validFrom, &signedValidTo, &validTo, &revoked, &cleanedAt, &boundBlock, &version.BoundLogIndex))
	version.Generation, version.ValidFromEpoch, version.SignedValidToEpoch, version.ValidToEpoch = uint64(generation), uint64(validFrom), uint64(signedValidTo), uint64(validTo)
	version.FleetId, version.Hotkey, version.Uid, version.BoundBlock = [32]byte(fleetId), [32]byte(hotkey), uint16(uid), uint64(boundBlock)
	version.Revoked = revoked != nil
	if cleanedAt != nil {
		version.Cleaned, version.CleanedAtEpoch = true, uint64(*cleanedAt)
	}
	return version
}

func getSnFleetBindingVersionsInTx(ctx context.Context, tx server.PgTx, key string, clientId *server.Id) []*SnFleetBindingVersion {
	versions := []*SnFleetBindingVersion{}
	rows, err := tx.Query(
		ctx,
		`SELECT `+snFleetBindingVersionColumns+` FROM sn_fleet_binding WHERE deployment_key = $1 AND client_id = $2 ORDER BY bound_block, bound_log_index`,
		key, *clientId,
	)
	server.WithPgResult(rows, err, func() {
		for rows.Next() {
			versions = append(versions, scanSnFleetBindingVersion(rows))
		}
	})
	return versions
}

// GetSnFleetBindingVersions returns every binding version of the deployment,
// by client and then in bind order. The census is bounded by bindings, which
// the coordinator caps per fleet and the head caps at 200 fleets.
func GetSnFleetBindingVersions(ctx context.Context, deploymentKey StDeploymentKey) []*SnFleetBindingVersion {
	key := requireStDeploymentKey(deploymentKey)
	versions := []*SnFleetBindingVersion{}
	server.Db(ctx, func(conn server.PgConn) {
		rows, err := conn.Query(
			ctx,
			`SELECT `+snFleetBindingVersionColumns+` FROM sn_fleet_binding WHERE deployment_key = $1 ORDER BY client_id, bound_block, bound_log_index`,
			key,
		)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				versions = append(versions, scanSnFleetBindingVersion(rows))
			}
		})
	})
	return versions
}

// SnFleetBindingsAt is bindingAt for every client without the live UID test:
// each client's latest version already valid at epoch, kept when it is active
// there. versions is any order; the result is by client id.
func SnFleetBindingsAt(versions []*SnFleetBindingVersion, epoch uint64) []*SnFleetBindingVersion {
	byClient := map[server.Id][]*SnFleetBindingVersion{}
	for _, version := range versions {
		byClient[version.ClientId] = append(byClient[version.ClientId], version)
	}
	active := []*SnFleetBindingVersion{}
	for _, clientVersions := range byClient {
		slices.SortFunc(clientVersions, func(a *SnFleetBindingVersion, b *SnFleetBindingVersion) int {
			if a.BoundBlock != b.BoundBlock {
				if a.BoundBlock < b.BoundBlock {
					return -1
				}
				return 1
			}
			return a.BoundLogIndex - b.BoundLogIndex
		})
		for i := len(clientVersions) - 1; 0 <= i; i-- {
			version := clientVersions[i]
			if epoch < version.ValidFromEpoch {
				continue
			}
			if version.activeAt(epoch) {
				active = append(active, version)
			}
			break
		}
	}
	slices.SortFunc(active, func(a *SnFleetBindingVersion, b *SnFleetBindingVersion) int {
		return a.ClientId.Cmp(b.ClientId)
	})
	return active
}

// The validator's fleet identity (sn validator FleetScoreKey): a fleet's
// members share it, and a new generation is a new identity.
type SnFleetKey struct {
	FleetId    [32]byte
	Hotkey     [32]byte
	Generation uint64
	Uid        uint16
}

func (self SnFleetKey) compare(other SnFleetKey) int {
	if self.Uid != other.Uid {
		return int(self.Uid) - int(other.Uid)
	}
	if order := strings.Compare(string(self.FleetId[:]), string(other.FleetId[:])); order != 0 {
		return order
	}
	if order := strings.Compare(string(self.Hotkey[:]), string(other.Hotkey[:])); order != 0 {
		return order
	}
	if self.Generation != other.Generation {
		if self.Generation < other.Generation {
			return -1
		}
		return 1
	}
	return 0
}

// The holder of one UID at a finalized native block: its hotkey and the
// block it registered at. A hotkey that deregisters and registers again gets
// a new registration block, so the pair identifies one registration.
type SnUidHolder struct {
	Hotkey              [32]byte
	BlockAtRegistration uint64
}

// The UID holders of one metagraph census.
type SnUidHolders struct {
	SubnetEpochIndex uint64
	Block            uint64
	ReadAt           time.Time
	Holders          map[uint16]SnUidHolder
}

// Whether a binding's hotkey still holds its UID at the metagraph census.
const (
	// Keys(netuid, uid) is the binding's hotkey
	SnUidHeldYes = "held"
	// another hotkey holds the UID, or nobody does
	SnUidHeldNo = "lost"
	// the census has not read the UID's holder
	SnUidHeldUnknown = "unknown"
)

// One member of a claimed fleet.
type SnClaimedFleetMember struct {
	ClientId server.Id
	// from network_client; nil when the client has no network
	NetworkId      *server.Id
	ValidFromEpoch uint64
	ValidToEpoch   uint64
}

// One claimed fleet at a settlement epoch.
type SnClaimedFleet struct {
	Key SnFleetKey
	// the latest validity among the members: the fleet stays bound until then
	ValidToEpoch uint64
	// SnUidHeldYes, SnUidHeldNo or SnUidHeldUnknown
	UidHeld string
	// the holder's registration block at the census, when held
	UidRegisteredBlock uint64
	Members            []*SnClaimedFleetMember
}

// Live: the window, generation and UID checks all pass, so the validator
// counts the fleet's members (sn validator releaseMeasurementBindingObservations).
func (self *SnClaimedFleet) Live() bool {
	return self.UidHeld == SnUidHeldYes
}

// Every fleet with a binding active at one settlement epoch.
type SnClaimedFleets struct {
	SettlementEpoch uint64
	// every binding log before this block is applied
	SyncedNextBlock uint64
	// the metagraph census the UID test used; zero without one
	UidSubnetEpochIndex uint64
	UidBlock            uint64
	UidReadAt           time.Time
	// by UID, then fleet id, hotkey and generation
	Fleets []*SnClaimedFleet
}

// The live fleets.
func (self *SnClaimedFleets) LiveFleets() []*SnClaimedFleet {
	live := []*SnClaimedFleet{}
	for _, fleet := range self.Fleets {
		if fleet.Live() {
			live = append(live, fleet)
		}
	}
	return live
}

// SnClaimedFleetsAt groups the bindings active at epoch into the validator's
// fleets and applies bindingAt's live UID test with the census holders:
// Keys(netuid, uid) must be the binding's hotkey. A nil holders leaves every
// fleet unknown. networkIds joins members to their networks.
func SnClaimedFleetsAt(versions []*SnFleetBindingVersion, epoch uint64, holders *SnUidHolders, networkIds map[server.Id]server.Id) *SnClaimedFleets {
	claimed := &SnClaimedFleets{SettlementEpoch: epoch, Fleets: []*SnClaimedFleet{}}
	if holders != nil {
		claimed.UidSubnetEpochIndex, claimed.UidBlock, claimed.UidReadAt = holders.SubnetEpochIndex, holders.Block, holders.ReadAt
	}
	fleets := map[SnFleetKey]*SnClaimedFleet{}
	for _, version := range SnFleetBindingsAt(versions, epoch) {
		key := SnFleetKey{FleetId: version.FleetId, Hotkey: version.Hotkey, Generation: version.Generation, Uid: version.Uid}
		fleet, ok := fleets[key]
		if !ok {
			fleet = &SnClaimedFleet{Key: key, UidHeld: SnUidHeldUnknown}
			if holders != nil {
				if holder, read := holders.Holders[key.Uid]; read {
					if holder.Hotkey == key.Hotkey {
						fleet.UidHeld, fleet.UidRegisteredBlock = SnUidHeldYes, holder.BlockAtRegistration
					} else {
						fleet.UidHeld = SnUidHeldNo
					}
				}
			}
			fleets[key] = fleet
			claimed.Fleets = append(claimed.Fleets, fleet)
		}
		member := &SnClaimedFleetMember{ClientId: version.ClientId, ValidFromEpoch: version.ValidFromEpoch, ValidToEpoch: version.ValidToEpoch}
		if networkId, ok := networkIds[version.ClientId]; ok {
			member.NetworkId = &networkId
		}
		fleet.Members = append(fleet.Members, member)
		fleet.ValidToEpoch = max(fleet.ValidToEpoch, version.ValidToEpoch)
	}
	slices.SortFunc(claimed.Fleets, func(a *SnClaimedFleet, b *SnClaimedFleet) int {
		return a.Key.compare(b.Key)
	})
	return claimed
}

// The client ids of every binding version, for the network_client join.
func SnFleetBindingClientIds(versions []*SnFleetBindingVersion) []server.Id {
	clientIds := []server.Id{}
	seen := map[server.Id]bool{}
	for _, version := range versions {
		if !seen[version.ClientId] {
			seen[version.ClientId] = true
			clientIds = append(clientIds, version.ClientId)
		}
	}
	return clientIds
}

// The UIDs every binding active at one of the epochs names, ascending: the
// UIDs whose holders the metagraph census reads.
func SnFleetBindingUids(versions []*SnFleetBindingVersion, epochs ...uint64) []uint16 {
	uids := []uint16{}
	for _, epoch := range epochs {
		for _, version := range SnFleetBindingsAt(versions, epoch) {
			uids = append(uids, version.Uid)
		}
	}
	slices.Sort(uids)
	return slices.Compact(uids)
}
