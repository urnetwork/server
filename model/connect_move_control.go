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
//
// The control can only narrow the code settings, never widen them:
//   - the mode never goes above what `EnableMove` allows. `MoveDefaultMode` is
//     the mode while the control does not set one, not a ceiling: an operator
//     turns the mover on with the control.
//   - `maxPerMinute` is at most the code's fleet budget.
//   - `tolerance` is at least the code's band, since a narrower band moves
//     more clients.
//   - `drainMoves` can turn drain moves off, never on past `EnableDrainMoves`.
//
// A field that does not parse narrows as far as it can: mode off,
// maxPerMinute 0, an infinite tolerance (no balance sources), drain moves off.
// An absent key or field leaves the code default. A reader that cannot read
// the control acts as if the mode were off (the mover does).
package model

import (
	"context"
	"math"
	"strconv"
	"strings"

	"github.com/urnetwork/server"
)

// The control hash. A singleton key on the `{connect_move}` slot.
const connectMoveControlKey = "{connect_move}control"

// The control's fields.
const (
	ConnectMoveControlFieldMode          = "mode"
	ConnectMoveControlFieldMaxPerMinute  = "maxPerMinute"
	ConnectMoveControlFieldTolerance     = "tolerance"
	ConnectMoveControlFieldEvacuateHosts = "evacuateHosts"
	ConnectMoveControlFieldDrainMoves    = "drainMoves"
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
}

// The control the fields narrow the ceiling to. Pure: fields is the control
// hash as read, empty when the key is absent.
func NarrowConnectMoveControl(fields map[string]string, ceiling ConnectMoveControlCeiling) *ConnectMoveControl {
	control := &ConnectMoveControl{
		Present:       0 < len(fields),
		MaxPerMinute:  ceiling.MaxPerMinute,
		Tolerance:     ceiling.Tolerance,
		EvacuateHosts: map[string]bool{},
		DrainMoves:    ceiling.EnableDrainMoves,
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

	if !ceiling.EnableMove {
		control.Mode = ConnectMoveModeOff
		control.DrainMoves = false
	}
	return control
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
