// The runtime move control (connect/MOVE.md §5.4, D14): the hash
// `{connect_move}control`, which ops set to stop, slow or point the mover
// without a connect deploy. Every connect process reads it each mover tick.
//
// Fields, all optional:
//   - `mode`: off, dry_run or on.
//   - `maxPerMinute`: the fleet budget of moves per minute.
//   - `tolerance`: the band around the fleet mean.
//   - `evacuateHosts`: hosts whose processes move all their residents, comma or
//     space separated.
//   - `drainMoves`: whether drains send compulsory moves with intents (D13);
//     true or false.
//   - `placement`: whether platform-hosted clients dial the edge the load
//     model chooses (server edgeplacement, MOVE.md §13); true or false. Read
//     by the processes that place, not by the mover.
//   - `transportDelay`: whether the connect handlers accept the per-message
//     delay header on new connections (connect DELAY.md §11); true or false.
//     Read by the connect handlers, not by the mover.
//   - `localTransport`: the connect blocks whose exchanges deliver a transport
//     in process when its resident is in the same process (connect
//     resident_local_transport.go): off, all, or blocks separated by commas
//     or spaces. Read by the exchanges, not by the mover.
//   - `localForward`: the connect blocks whose exchanges deliver a resident's
//     forward in process when its destination's resident is in the same
//     process (connect resident_local_forward.go): off, all, or blocks, as
//     `localTransport`. Read by the exchanges, not by the mover.
//   - `stuckProviderSignal`: the api blocks whose processes report a provider
//     stuck retrying terminal refusals (controller
//     connect_control_stuck_provider.go): off, all, or blocks, as
//     `localTransport`. Read by the api processes.
//   - `stuckProviderRecycle`: the connect blocks whose exchanges recycle a
//     reported provider they host (connect client_recycle.go): off, all, or
//     blocks, as `localTransport`. Read by the exchanges, not by the mover.
//   - `windowStrictHandover`: the platform's override of the apps' strict
//     window handover (connect `MultiClientSettings.StrictWindowHandover`),
//     which every find-providers2 answer the api serves over http carries as
//     `window_strict_handover` (controller window_strict_handover_control.go):
//     off turns the handover off in the apps from their next listing, and on
//     leaves each app's own setting; true or false, 1 or 0, on or off. Read by
//     the api processes. Hosted devices discover in process and never see it.
//
// The control can only narrow the code settings, never widen them:
//   - the mode never goes above what `EnableMove` allows. `MoveDefaultMode` is
//     the mode while the control does not set one, not a ceiling: an operator
//     turns the mover on with the control.
//   - `maxPerMinute` is at most the code's fleet budget.
//   - `tolerance` is at least the code's band, since a narrower band moves
//     more clients.
//   - `drainMoves` can turn drain moves off, never on past `EnableDrainMoves`.
//   - `placement` can turn placement off, never on past `EnablePlacement`.
//   - `transportDelay` can turn the delay header off, never on past
//     `EnableTransportDelay`.
//   - `localTransport` chooses the blocks, never past `EnableLocalTransport`.
//     `LocalTransportDefault` is the choice while the control sets none, not
//     a ceiling: an operator turns in-process delivery on with the control.
//   - `localForward` chooses blocks the same way, never past
//     `EnableLocalForward`, with `LocalForwardDefault` as its choice while the
//     control sets none. The two switches never read each other's field.
//   - `stuckProviderSignal` and `stuckProviderRecycle` choose blocks the same
//     way, never past `EnableStuckProviderSignal` and
//     `EnableStuckProviderRecycle`, with `StuckProviderSignalDefault` and
//     `StuckProviderRecycleDefault` as their choices while the control sets
//     none.
//   - `windowStrictHandover` can turn the apps' handover off, never on past an
//     app's own setting.
//
// A field that does not parse narrows as far as it can: mode off,
// maxPerMinute 0, an infinite tolerance (no balance sources), drain moves off,
// placement off, transport delay off. A `localTransport`, `localForward`,
// `stuckProviderSignal` or `stuckProviderRecycle` list names only the blocks
// it lists, so a value that is neither off nor all and names no real block
// turns its switch off everywhere. A `windowStrictHandover` that does not
// parse overrides nothing, as an absent one does: the field is an off switch
// whose default is each app's own setting.
// An absent key or field leaves the code default. A reader that cannot read
// the control acts as if the mode were off (the mover does); the connect
// handlers keep the transport delay of their last read, and the exchanges
// the local transport and local forward of theirs.
package model

import (
	"context"
	"math"
	"strconv"
	"strings"

	"github.com/urnetwork/server/v2026"
)

// The control hash. A singleton key on the `{connect_move}` slot.
const connectMoveControlKey = "{connect_move}control"

// The control's fields.
const (
	ConnectMoveControlFieldMode           = "mode"
	ConnectMoveControlFieldMaxPerMinute   = "maxPerMinute"
	ConnectMoveControlFieldTolerance      = "tolerance"
	ConnectMoveControlFieldEvacuateHosts  = "evacuateHosts"
	ConnectMoveControlFieldDrainMoves     = "drainMoves"
	ConnectMoveControlFieldPlacement      = "placement"
	ConnectMoveControlFieldTransportDelay = "transportDelay"
	ConnectMoveControlFieldLocalTransport = "localTransport"
	ConnectMoveControlFieldLocalForward   = "localForward"

	ConnectMoveControlFieldStuckProviderSignal  = "stuckProviderSignal"
	ConnectMoveControlFieldStuckProviderRecycle = "stuckProviderRecycle"

	ConnectMoveControlFieldWindowStrictHandover = "windowStrictHandover"
)

// The values of a block list field (`localTransport`, `localForward`,
// `stuckProviderSignal`, `stuckProviderRecycle`) that are not block lists.
const (
	ConnectLocalTransportOff = "off"
	ConnectLocalTransportAll = "all"
)

// The mover's mode, ordered from narrowest to widest.
type ConnectMoveMode int

const (
	// Computes and exports the load, moves nothing.
	ConnectMoveModeOff ConnectMoveMode = 0
	// Does everything except the cooldown, the intent and the send, and counts
	// the moves it would send.
	ConnectMoveModeDryRun ConnectMoveMode = 1
	// Moves clients.
	ConnectMoveModeOn ConnectMoveMode = 2
)

// The mode of a setting or control value: off, dry_run or on. False for any
// other value.
func ParseConnectMoveMode(value string) (ConnectMoveMode, bool) {
	switch strings.TrimSpace(value) {
	case "off":
		return ConnectMoveModeOff, true
	case "dry_run":
		return ConnectMoveModeDryRun, true
	case "on":
		return ConnectMoveModeOn, true
	default:
		return ConnectMoveModeOff, false
	}
}

// The mode's control value. Any value outside the modes prints off.
func (self ConnectMoveMode) String() string {
	switch self {
	case ConnectMoveModeDryRun:
		return "dry_run"
	case ConnectMoveModeOn:
		return "on"
	default:
		return "off"
	}
}

// The code settings the control narrows.
type ConnectMoveControlCeiling struct {
	// `EnableMove`: false forces the mode off and drain moves off.
	EnableMove bool
	// `EnableDrainMoves`.
	EnableDrainMoves bool
	// `MoveDefaultMode`: the mode while the control sets none. A value that
	// does not parse is off.
	DefaultMode string
	// `MoveMaxPerMinute`.
	MaxPerMinute int
	// `MoveBalanceTolerance`.
	Tolerance float64
	// The placing process's switch for edge placement. The mover leaves it
	// false and ignores the control's placement.
	EnablePlacement bool
	// The connect handler's switch for the delay header,
	// `EnableTransportDelay`. The mover and the placement leave it false and
	// ignore the control's transportDelay.
	EnableTransportDelay bool
	// The exchange's switch for in-process transport delivery,
	// `EnableLocalTransport`. The mover, the placement and the handlers leave
	// it false and ignore the control's localTransport.
	EnableLocalTransport bool
	// `LocalTransportDefault`: the blocks while the control sets none, in the
	// control's syntax. Not a ceiling.
	LocalTransportDefault string
	// The exchange's switch for in-process forward delivery,
	// `EnableLocalForward`. Every other reader leaves it false and ignores the
	// control's localForward.
	EnableLocalForward bool
	// `LocalForwardDefault`: the blocks while the control sets none, in the
	// control's syntax. Not a ceiling.
	LocalForwardDefault string
	// The api's switch for reporting stuck providers,
	// `EnableStuckProviderSignal`. Every other reader leaves it false and
	// ignores the control's stuckProviderSignal.
	EnableStuckProviderSignal bool
	// `StuckProviderSignalDefault`: the api blocks while the control sets
	// none, in the control's syntax. Not a ceiling.
	StuckProviderSignalDefault string
	// The exchange's switch for recycling reported providers,
	// `EnableStuckProviderRecycle`. Every other reader leaves it false and
	// ignores the control's stuckProviderRecycle.
	EnableStuckProviderRecycle bool
	// `StuckProviderRecycleDefault`: the connect blocks while the control sets
	// none, in the control's syntax. Not a ceiling.
	StuckProviderRecycleDefault string
}

// The effective control: the code settings as the Redis control narrowed them.
type ConnectMoveControl struct {
	// The control key was present.
	Present bool
	Mode    ConnectMoveMode
	// The fleet budget per minute. Non-positive allows none.
	MaxPerMinute int
	// The band around the fleet mean. Infinite when the control's tolerance
	// did not parse: no host is a balance source.
	Tolerance float64
	// Hosts whose processes move all their residents (MOVE.md §5.6).
	EvacuateHosts map[string]bool
	// Drains send compulsory moves with intents.
	DrainMoves bool
	// Platform-hosted clients dial the edge the load model chooses.
	Placement bool
	// Connect handlers accept the delay header on new connections that offer
	// it.
	TransportDelay bool
	// Every connect block delivers transports in process.
	LocalTransportAll bool
	// The connect blocks that deliver transports in process, when not all.
	// Empty for none.
	LocalTransportBlocks map[string]bool
	// Every connect block delivers forwards in process.
	LocalForwardAll bool
	// The connect blocks that deliver forwards in process, when not all. Empty
	// for none.
	LocalForwardBlocks map[string]bool
	// Every api block reports stuck providers.
	StuckProviderSignalAll bool
	// The api blocks that report stuck providers, when not all. Empty for
	// none.
	StuckProviderSignalBlocks map[string]bool
	// Every connect block recycles reported providers.
	StuckProviderRecycleAll bool
	// The connect blocks that recycle reported providers, when not all. Empty
	// for none.
	StuckProviderRecycleBlocks map[string]bool
	// The platform's override of the apps' strict window handover: false turns
	// it off, true leaves each app's own setting. Nil overrides nothing.
	WindowStrictHandover *bool
}

// Whether the exchanges of block deliver transports in process.
func (self *ConnectMoveControl) LocalTransportAllows(block string) bool {
	return self.LocalTransportAll || self.LocalTransportBlocks[block]
}

// Whether the exchanges of block deliver forwards in process.
func (self *ConnectMoveControl) LocalForwardAllows(block string) bool {
	return self.LocalForwardAll || self.LocalForwardBlocks[block]
}

// Whether the api processes of block report stuck providers.
func (self *ConnectMoveControl) StuckProviderSignalAllows(block string) bool {
	return self.StuckProviderSignalAll || self.StuckProviderSignalBlocks[block]
}

// Whether the exchanges of block recycle reported providers.
func (self *ConnectMoveControl) StuckProviderRecycleAllows(block string) bool {
	return self.StuckProviderRecycleAll || self.StuckProviderRecycleBlocks[block]
}

// The control the fields narrow the ceiling to. Pure: fields is the control
// hash as read, empty when the key is absent.
func NarrowConnectMoveControl(fields map[string]string, ceiling ConnectMoveControlCeiling) *ConnectMoveControl {
	control := &ConnectMoveControl{
		Present:        0 < len(fields),
		MaxPerMinute:   ceiling.MaxPerMinute,
		Tolerance:      ceiling.Tolerance,
		EvacuateHosts:  map[string]bool{},
		DrainMoves:     ceiling.EnableDrainMoves,
		Placement:      ceiling.EnablePlacement,
		TransportDelay: ceiling.EnableTransportDelay,
	}

	modeValue, modeSet := fields[ConnectMoveControlFieldMode]
	if !modeSet {
		modeValue = ceiling.DefaultMode
	}
	// a value that does not parse is off
	control.Mode, _ = ParseConnectMoveMode(modeValue)

	if value, ok := fields[ConnectMoveControlFieldMaxPerMinute]; ok {
		if maxPerMinute, err := strconv.Atoi(strings.TrimSpace(value)); err == nil {
			control.MaxPerMinute = min(ceiling.MaxPerMinute, maxPerMinute)
		} else {
			control.MaxPerMinute = 0
		}
	}

	if value, ok := fields[ConnectMoveControlFieldTolerance]; ok {
		tolerance, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if err == nil && !math.IsNaN(tolerance) {
			control.Tolerance = max(ceiling.Tolerance, tolerance)
		} else {
			control.Tolerance = math.Inf(1)
		}
	}

	if value, ok := fields[ConnectMoveControlFieldEvacuateHosts]; ok {
		for _, host := range strings.FieldsFunc(value, func(r rune) bool {
			return r == ',' || r == ' ' || r == '\t' || r == '\n'
		}) {
			control.EvacuateHosts[host] = true
		}
	}

	if value, ok := fields[ConnectMoveControlFieldDrainMoves]; ok {
		drainMoves, parsed := parseConnectMoveControlBool(value)
		control.DrainMoves = ceiling.EnableDrainMoves && parsed && drainMoves
	}

	if value, ok := fields[ConnectMoveControlFieldPlacement]; ok {
		placement, parsed := parseConnectMoveControlBool(value)
		control.Placement = ceiling.EnablePlacement && parsed && placement
	}

	if value, ok := fields[ConnectMoveControlFieldTransportDelay]; ok {
		transportDelay, parsed := parseConnectMoveControlBool(value)
		control.TransportDelay = ceiling.EnableTransportDelay && parsed && transportDelay
	}

	if ceiling.EnableLocalTransport {
		localTransportValue, localTransportSet := fields[ConnectMoveControlFieldLocalTransport]
		if !localTransportSet {
			localTransportValue = ceiling.LocalTransportDefault
		}
		control.LocalTransportAll, control.LocalTransportBlocks = parseConnectControlBlocks(localTransportValue)
	}

	if ceiling.EnableLocalForward {
		localForwardValue, localForwardSet := fields[ConnectMoveControlFieldLocalForward]
		if !localForwardSet {
			localForwardValue = ceiling.LocalForwardDefault
		}
		control.LocalForwardAll, control.LocalForwardBlocks = parseConnectControlBlocks(localForwardValue)
	}

	if ceiling.EnableStuckProviderSignal {
		signalValue, signalSet := fields[ConnectMoveControlFieldStuckProviderSignal]
		if !signalSet {
			signalValue = ceiling.StuckProviderSignalDefault
		}
		control.StuckProviderSignalAll, control.StuckProviderSignalBlocks = parseConnectControlBlocks(signalValue)
	}

	if ceiling.EnableStuckProviderRecycle {
		recycleValue, recycleSet := fields[ConnectMoveControlFieldStuckProviderRecycle]
		if !recycleSet {
			recycleValue = ceiling.StuckProviderRecycleDefault
		}
		control.StuckProviderRecycleAll, control.StuckProviderRecycleBlocks = parseConnectControlBlocks(recycleValue)
	}

	if value, ok := fields[ConnectMoveControlFieldWindowStrictHandover]; ok {
		// a value that does not parse overrides nothing
		if windowStrictHandover, parsed := parseConnectMoveControlBool(value); parsed {
			control.WindowStrictHandover = &windowStrictHandover
		}
	}

	if !ceiling.EnableMove {
		control.Mode = ConnectMoveModeOff
		control.DrainMoves = false
	}
	return control
}

// off, all, or a list of blocks separated by commas or white space. An empty
// value or list names no block.
func parseConnectControlBlocks(value string) (all bool, blocks map[string]bool) {
	value = strings.TrimSpace(value)
	switch value {
	case ConnectLocalTransportAll:
		return true, nil
	case ConnectLocalTransportOff:
		return false, nil
	}
	for _, block := range strings.FieldsFunc(value, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n'
	}) {
		if blocks == nil {
			blocks = map[string]bool{}
		}
		blocks[block] = true
	}
	return false, blocks
}

// true/false, 1/0 and on/off.
func parseConnectMoveControlBool(value string) (bool, bool) {
	value = strings.TrimSpace(value)
	switch value {
	case "on":
		return true, true
	case "off":
		return false, true
	}
	parsed, err := strconv.ParseBool(value)
	return parsed, err == nil
}

// Reads the control and narrows the ceiling with it. One bounded Redis read,
// never retried: ctx must carry a deadline. On an error the caller acts as if
// the mode were off.
func GetConnectMoveControl(ctx context.Context, ceiling ConnectMoveControlCeiling) (control *ConnectMoveControl, returnErr error) {
	var fields map[string]string
	returnErr = server.RedisWithDeadline(ctx, func(r server.RedisClient) error {
		var err error
		fields, err = r.HGetAll(ctx, connectMoveControlKey).Result()
		return err
	}, server.OptRedisNoPreflight())
	if returnErr != nil {
		return nil, returnErr
	}
	return NarrowConnectMoveControl(fields, ceiling), nil
}

// Sets control fields, keeping the others. For operator tools and tests.
// ctx must carry a deadline.
func SetConnectMoveControl(ctx context.Context, fields map[string]string) error {
	if len(fields) == 0 {
		return nil
	}
	values := make([]any, 0, 2*len(fields))
	for field, value := range fields {
		values = append(values, field, value)
	}
	return server.RedisWithDeadline(ctx, func(r server.RedisClient) error {
		return r.HSet(ctx, connectMoveControlKey, values...).Err()
	}, server.OptRedisNoPreflight())
}

// Deletes the control, so the code defaults apply. ctx must carry a deadline.
func RemoveConnectMoveControl(ctx context.Context) error {
	return server.RedisWithDeadline(ctx, func(r server.RedisClient) error {
		return r.Del(ctx, connectMoveControlKey).Err()
	}, server.OptRedisNoPreflight())
}
