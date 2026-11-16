package model

import (
	"context"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/urnetwork/server"
)

// The key history retention's runtime brake: the Redis hash
// `{client_key_history_retention}control`, apart from the contract horizon
// reaper's control. It stops or slows the deletes without a deploy and is read
// before every batch, so a change applies from the next batch. Its fields, all
// optional:
//   - `mode`: on or off. Off, or any other value, deletes nothing.
//   - `maxClientsPerSecond`: gone records the deletes examine per second, and
//     so the most gone clients they delete per second, at most the code's
//     MaxDeleteClientsPerSecond; zero, or a value that does not parse, is off.
//   - `horizonDays`: the retention's horizon in whole days: history goes once
//     this long has passed since its client went. It only narrows: it is never
//     below the guard's floor (server.ClientKeyHistoryRetentionFloor, 7 days),
//     which a smaller value counts as, nor above the code's Retention (7
//     days), which a larger value or one that does not parse counts as. While
//     Retention is the floor, every value is 7 days, the 30 set on 2026-11-02
//     included. It is read once a run, so a change applies from the next run,
//     and removing it returns the retention to Retention.
//
// Without the hash the retention deletes at DefaultDeleteClientsPerSecond, at
// Retention. A run that cannot read the hash deletes nothing more in that
// run. The weekly discovery of clients gone without a record only records
// them, and runs either way.
const stClientKeyHistoryRetentionControlKey = "{client_key_history_retention}control"

const (
	StClientKeyHistoryRetentionControlFieldMode                = "mode"
	StClientKeyHistoryRetentionControlFieldMaxClientsPerSecond = "maxClientsPerSecond"
	StClientKeyHistoryRetentionControlFieldHorizonDays         = "horizonDays"
)

// The effective brake: the code settings as the Redis hash narrowed them.
type StClientKeyHistoryRetentionControl struct {
	// the hash was present
	Present bool
	// deletes run
	On bool
	// gone records examined, and so clients deleted, per second at most, while
	// On
	MaxClientsPerSecond float64
	// the retention's own horizon, from `horizonDays`; nil keeps the code's
	// Retention (StClientKeyHistoryRetentionSettings.horizon)
	Horizon *time.Duration
}

// The brake the fields narrow the settings to. Pure: fields is the hash as
// read, empty when the key is absent. A field narrows as far as it can: a
// mode other than on is off, a rate that does not parse is zero, off, and a
// horizon that does not parse is the longest.
func NarrowStClientKeyHistoryRetentionControl(fields map[string]string, settings *StClientKeyHistoryRetentionSettings) *StClientKeyHistoryRetentionControl {
	control := &StClientKeyHistoryRetentionControl{
		Present:             0 < len(fields),
		On:                  true,
		MaxClientsPerSecond: min(settings.DefaultDeleteClientsPerSecond, settings.MaxDeleteClientsPerSecond),
	}
	if value, ok := fields[StClientKeyHistoryRetentionControlFieldMode]; ok {
		control.On = strings.TrimSpace(value) == "on"
	}
	if value, ok := fields[StClientKeyHistoryRetentionControlFieldMaxClientsPerSecond]; ok {
		rate, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if err == nil && !math.IsNaN(rate) {
			control.MaxClientsPerSecond = min(settings.MaxDeleteClientsPerSecond, rate)
		} else {
			control.MaxClientsPerSecond = 0
		}
	}
	if value, ok := fields[StClientKeyHistoryRetentionControlFieldHorizonDays]; ok {
		horizon := max(settings.Retention, server.ClientKeyHistoryRetentionFloor)
		if days, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64); err == nil {
			horizon = stClientKeyHistoryHorizonFromDays(days, settings.Retention)
		}
		control.Horizon = &horizon
	}
	if !(0 < control.MaxClientsPerSecond) {
		control.On = false
	}
	return control
}

// days as a horizon between the guard's floor and ceiling, the code's
// Retention. Clamped while still in days, so that no count of days overflows
// into a short duration. Pure.
func stClientKeyHistoryHorizonFromDays(days int64, ceiling time.Duration) time.Duration {
	const day = 24 * time.Hour
	floor := server.ClientKeyHistoryRetentionFloor
	ceiling = max(ceiling, floor)
	switch {
	case days <= int64(floor/day):
		return floor
	case int64(ceiling/day) < days:
		return ceiling
	default:
		return time.Duration(days) * day
	}
}

// Reads the brake and narrows the settings with it. One bounded Redis read,
// never retried; on an error the caller deletes nothing more.
func GetStClientKeyHistoryRetentionControl(ctx context.Context, settings *StClientKeyHistoryRetentionSettings) (control *StClientKeyHistoryRetentionControl, returnErr error) {
	readCtx, cancel := context.WithTimeout(ctx, settings.ControlTimeout)
	defer cancel()
	var fields map[string]string
	returnErr = server.RedisWithDeadline(readCtx, func(r server.RedisClient) error {
		var err error
		fields, err = r.HGetAll(readCtx, stClientKeyHistoryRetentionControlKey).Result()
		return err
	}, server.OptRedisNoPreflight())
	if returnErr != nil {
		return nil, returnErr
	}
	return NarrowStClientKeyHistoryRetentionControl(fields, settings), nil
}

// Sets brake fields, keeping the others. For operator tools and tests. ctx
// must carry a deadline.
func SetStClientKeyHistoryRetentionControl(ctx context.Context, fields map[string]string) error {
	if len(fields) == 0 {
		return nil
	}
	values := make([]any, 0, 2*len(fields))
	for field, value := range fields {
		values = append(values, field, value)
	}
	return server.RedisWithDeadline(ctx, func(r server.RedisClient) error {
		return r.HSet(ctx, stClientKeyHistoryRetentionControlKey, values...).Err()
	}, server.OptRedisNoPreflight())
}

// Deletes the brake, so the code settings apply. ctx must carry a deadline.
func RemoveStClientKeyHistoryRetentionControl(ctx context.Context) error {
	return server.RedisWithDeadline(ctx, func(r server.RedisClient) error {
		return r.Del(ctx, stClientKeyHistoryRetentionControlKey).Err()
	}, server.OptRedisNoPreflight())
}
