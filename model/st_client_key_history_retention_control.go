package model

import (
	"context"
	"math"
	"strconv"
	"strings"

	"github.com/urnetwork/server"
)

// The key history retention's runtime brake: the Redis hash
// `{client_key_history_retention}control`, apart from the contract horizon
// reaper's control. It stops or slows the deletes without a deploy and is read
// before every batch, so a change applies from the next batch. Its fields, all
// optional:
//   - `mode`: on or off. Off, or any other value, deletes nothing.
//   - `maxClientsPerSecond`: gone clients deleted per second, at most the
//     code's MaxDeleteClientsPerSecond; zero, or a value that does not parse,
//     is off.
//
// Without the hash the retention deletes at DefaultDeleteClientsPerSecond. A
// run that cannot read the hash deletes nothing more in that run. The weekly
// discovery of clients gone without a record only records them, and runs
// either way.
const stClientKeyHistoryRetentionControlKey = "{client_key_history_retention}control"

const (
	StClientKeyHistoryRetentionControlFieldMode                = "mode"
	StClientKeyHistoryRetentionControlFieldMaxClientsPerSecond = "maxClientsPerSecond"
)

// The effective brake: the code settings as the Redis hash narrowed them.
type StClientKeyHistoryRetentionControl struct {
	// the hash was present
	Present bool
	// deletes run
	On bool
	// gone clients deleted per second at most, while On
	MaxClientsPerSecond float64
}

// The brake the fields narrow the settings to. Pure: fields is the hash as
// read, empty when the key is absent. A field narrows as far as it can: a
// mode other than on is off, and a rate that does not parse is zero, off.
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
	if !(0 < control.MaxClientsPerSecond) {
		control.On = false
	}
	return control
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
