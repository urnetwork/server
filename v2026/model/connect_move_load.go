// The connect load table (connect/MOVE.md §5.4): one field per connect process
// in the hash `{connect_load}`, which the mover in every process reads to
// decide where clients belong.
//
// Each process writes its field `<host>/<block>/<exchangeId>` every
// MoveLoadPublishInterval with a JSON record of its load. The publish script
// stamps the record's `time` with the Redis node's clock (TIME), and the read
// script returns that clock with the table, so staleness is judged on one
// clock and no process clock decides it (MOVE.md §6). The braces are the whole
// key, so the table is one key on one cluster slot: about 30 fields today,
// read with one HGETALL per mover tick.
//
// Readers parse leniently, since a newer build may add fields. A field whose
// name or value does not parse is skipped and never deleted: a newer build may
// write a shape this one does not know. The movers delete fields that have
// been stale past the cleanup timeout with a compare-and-delete, so a field
// its process refreshed after the read survives.
//
// Every call is one bounded Redis operation on the deadline pool, never
// retried: ctx must carry a deadline. A failed publish is repaired by the next
// one, and a failed read leaves the mover idle for its tick.
package model

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/server/v2026"
)

// The whole table. The braces are the whole key: one cluster slot.
const connectMoveLoadKey = "{connect_load}"

// The host CPU busy fraction of a record whose publisher could not measure it
// (no /proc/stat, or no interval yet).
const ConnectMoveLoadCpuUnknown = float64(-1)

// One process's load record.
type ConnectMoveLoad struct {
	// The field: `<host>/<block>/<exchangeId>`.
	Host       string
	Block      string
	ExchangeId server.Id

	// The Redis node's clock when the record was written. Set by the publish
	// script; ignored on publish.
	Time time.Time
	// Distinct residents of the process.
	Residents int
	// Residents whose client provides.
	ProviderResidents int
	// Client transports of the process, for visibility (MOVE.md §5.4).
	Transports int
	// Cores the process used over its last publish interval.
	ProcessCpu float64
	// The busy fraction, 0 to 1, of all host CPUs over the publisher's last
	// interval, from /proc/stat, which is host-wide inside the container.
	// ConnectMoveLoadCpuUnknown when not measured.
	HostCpuBusy float64
	// runtime.NumCPU() of the process.
	HostCores int
	// The process is draining: its residents are leaving it.
	Draining bool

	// The stored value as read, which the stale cleanup compares before it
	// deletes. Empty for a record that was not read.
	value string
}

// The JSON record without its time, which the publish script adds.
type connectMoveLoadBody struct {
	Residents         int     `json:"residents"`
	ProviderResidents int     `json:"providerResidents"`
	Transports        int     `json:"transports"`
	ProcessCpu        float64 `json:"processCpu"`
	HostCpuBusy       float64 `json:"hostCpuBusy"`
	HostCores         int     `json:"hostCores"`
	Draining          bool    `json:"draining"`
}

// The stored JSON record: {time, residents, providerResidents, transports,
// processCpu, hostCpuBusy, hostCores, draining}, `time` in unix milliseconds
// of the Redis node's clock.
type connectMoveLoadValue struct {
	Time int64 `json:"time"`
	connectMoveLoadBody
}

// The field of one process.
func connectMoveLoadField(host string, block string, exchangeId server.Id) (string, error) {
	if host == "" || block == "" || strings.Contains(host, "/") || strings.Contains(block, "/") {
		return "", fmt.Errorf("connect load identity %q/%q needs a host and block without /", host, block)
	}
	return fmt.Sprintf("%s/%s/%s", host, block, exchangeId), nil
}

// The identity of a field, or false for a field this build does not parse.
func parseConnectMoveLoadField(field string) (host string, block string, exchangeId server.Id, ok bool) {
	parts := strings.Split(field, "/")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" {
		return "", "", server.Id{}, false
	}
	exchangeId, err := server.ParseId(parts[2])
	if err != nil {
		return "", "", server.Id{}, false
	}
	return parts[0], parts[1], exchangeId, true
}

// A measurement that JSON can carry: a NaN or infinite value is not one.
func finiteConnectMoveLoadValue(value float64, unknown float64) float64 {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return unknown
	}
	return value
}

// Writes ARGV[2], a JSON object, with `time` first, at ARGV[1], and returns
// that time: the node's clock in unix milliseconds. One key, so one slot.
var connectMoveLoadPublishScript = redis.NewScript(`
local time = redis.call('TIME')
local now = tonumber(time[1]) * 1000 + math.floor(tonumber(time[2]) / 1000)
redis.call('HSET', KEYS[1], ARGV[1], '{"time":' .. string.format('%.0f', now) .. ',' .. string.sub(ARGV[2], 2))
return now
`)

// Returns the node's clock in unix milliseconds and the whole table.
var connectMoveLoadReadScript = redis.NewScript(`
local time = redis.call('TIME')
local now = tonumber(time[1]) * 1000 + math.floor(tonumber(time[2]) / 1000)
return {now, redis.call('HGETALL', KEYS[1])}
`)

// Deletes each field of the ARGV pairs (field, value) that still holds the
// value, and returns how many it deleted.
var connectMoveLoadRemoveIfEqualScript = redis.NewScript(`
local removed = 0
for i = 1, #ARGV, 2 do
  if redis.call('HGET', KEYS[1], ARGV[i]) == ARGV[i + 1] then
    redis.call('HDEL', KEYS[1], ARGV[i])
    removed = removed + 1
  end
end
return removed
`)

// Writes the process's record, stamped with the Redis node's clock, and
// returns that time. The record's Time is ignored. A measurement that is NaN
// or infinite is written as unknown (CPU) or zero.
func PublishConnectMoveLoad(ctx context.Context, load *ConnectMoveLoad) (publishTime time.Time, returnErr error) {
	field, err := connectMoveLoadField(load.Host, load.Block, load.ExchangeId)
	if err != nil {
		return time.Time{}, err
	}
	body, err := json.Marshal(&connectMoveLoadBody{
		Residents:         load.Residents,
		ProviderResidents: load.ProviderResidents,
		Transports:        load.Transports,
		ProcessCpu:        finiteConnectMoveLoadValue(load.ProcessCpu, 0),
		HostCpuBusy:       finiteConnectMoveLoadValue(load.HostCpuBusy, ConnectMoveLoadCpuUnknown),
		HostCores:         load.HostCores,
		Draining:          load.Draining,
	})
	if err != nil {
		return time.Time{}, err
	}
	returnErr = server.RedisWithDeadline(ctx, func(r server.RedisClient) error {
		now, err := connectMoveLoadPublishScript.Run(ctx, r, []string{connectMoveLoadKey}, field, string(body)).Int64()
		if err != nil {
			return err
		}
		publishTime = time.UnixMilli(now)
		return nil
	}, server.OptRedisNoPreflight())
	return
}

// Reads the whole table and the Redis node's clock at the read. Fields that
// do not parse are left out. Every record is returned, fresh or not; the
// reader judges freshness against nodeTime.
func GetConnectMoveLoads(ctx context.Context) (loads []*ConnectMoveLoad, nodeTime time.Time, returnErr error) {
	returnErr = server.RedisWithDeadline(ctx, func(r server.RedisClient) error {
		result, err := connectMoveLoadReadScript.Run(ctx, r, []string{connectMoveLoadKey}).Slice()
		if err != nil {
			return err
		}
		if len(result) != 2 {
			return fmt.Errorf("connect load read answered %d values", len(result))
		}
		now, ok := result[0].(int64)
		if !ok {
			return fmt.Errorf("connect load read answered a clock of %T", result[0])
		}
		nodeTime = time.UnixMilli(now)
		fieldValues, ok := result[1].([]any)
		if !ok {
			return fmt.Errorf("connect load read answered a table of %T", result[1])
		}
		loads = make([]*ConnectMoveLoad, 0, len(fieldValues)/2)
		for i := 0; i+1 < len(fieldValues); i += 2 {
			field, fieldOk := fieldValues[i].(string)
			value, valueOk := fieldValues[i+1].(string)
			if !fieldOk || !valueOk {
				continue
			}
			if load, ok := parseConnectMoveLoad(field, value); ok {
				loads = append(loads, load)
			}
		}
		return nil
	}, server.OptRedisNoPreflight())
	if returnErr != nil {
		return nil, time.Time{}, returnErr
	}
	return
}

// One stored record, or false for one this build does not parse. Unknown JSON
// fields are allowed: a newer build may add them.
func parseConnectMoveLoad(field string, value string) (*ConnectMoveLoad, bool) {
	host, block, exchangeId, ok := parseConnectMoveLoadField(field)
	if !ok {
		return nil, false
	}
	var stored connectMoveLoadValue
	if err := json.Unmarshal([]byte(value), &stored); err != nil {
		return nil, false
	}
	return &ConnectMoveLoad{
		Host:              host,
		Block:             block,
		ExchangeId:        exchangeId,
		Time:              time.UnixMilli(stored.Time),
		Residents:         stored.Residents,
		ProviderResidents: stored.ProviderResidents,
		Transports:        stored.Transports,
		ProcessCpu:        stored.ProcessCpu,
		HostCpuBusy:       stored.HostCpuBusy,
		HostCores:         stored.HostCores,
		Draining:          stored.Draining,
		value:             value,
	}, true
}

// Deletes the process's field, for a publisher that stops.
func RemoveConnectMoveLoad(ctx context.Context, host string, block string, exchangeId server.Id) error {
	field, err := connectMoveLoadField(host, block, exchangeId)
	if err != nil {
		return err
	}
	return server.RedisWithDeadline(ctx, func(r server.RedisClient) error {
		return r.HDel(ctx, connectMoveLoadKey, field).Err()
	}, server.OptRedisNoPreflight())
}

// Deletes the fields of loads, as read with nodeTime, that were older than
// cleanupTimeout at that read and still hold the value read. A field its
// process wrote again after the read is kept. Non-positive cleanupTimeout
// deletes nothing.
func RemoveStaleConnectMoveLoads(
	ctx context.Context,
	loads []*ConnectMoveLoad,
	nodeTime time.Time,
	cleanupTimeout time.Duration,
) (removedCount int, returnErr error) {
	if cleanupTimeout <= 0 {
		return 0, nil
	}
	args := []any{}
	for _, load := range loads {
		if load.value == "" || nodeTime.Sub(load.Time) <= cleanupTimeout {
			continue
		}
		field, err := connectMoveLoadField(load.Host, load.Block, load.ExchangeId)
		if err != nil {
			continue
		}
		args = append(args, field, load.value)
	}
	if len(args) == 0 {
		return 0, nil
	}
	returnErr = server.RedisWithDeadline(ctx, func(r server.RedisClient) error {
		removed, err := connectMoveLoadRemoveIfEqualScript.Run(ctx, r, []string{connectMoveLoadKey}, args...).Int64()
		if err != nil {
			return err
		}
		removedCount = int(removed)
		return nil
	}, server.OptRedisNoPreflight())
	return
}
