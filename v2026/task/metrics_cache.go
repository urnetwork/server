// One non-waiting Redis admission owns each queue sampling interval. All
// taskworkers publish the completed observation with its original source time;
// unavailable coordination never falls back to a per-process database scan.
package task

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"strings"
	"time"

	"github.com/urnetwork/server/v2026"
)

const (
	taskMetricsRedisTimeout = 2 * time.Second
	// Matches the dashboard freshness gate. Retention does not renew source time.
	taskMetricsRetention = 45 * time.Second
	taskMetricsClockSkew = 5 * time.Second
	taskMetricsWireBytes = 512
	// Version both the schema and the aggregate semantics, including availability.
	taskMetricsCacheKey = "task_queue_metrics:v1:{available_block}:snapshot"
	taskMetricsOwnerKey = "task_queue_metrics:v1:{available_block}:owner"
)

var errTaskQueueMetricsUnavailable = errors.New("task queue metrics snapshot unavailable")

// Admission returns the prior completed generation while a new owner fills it.
// Ownership is retained through success or failure until the interval expires.
type taskQueueMetricsStore interface {
	acquire(context.Context, string) (value string, acquired bool, err error)
	publish(context.Context, string, string, time.Duration) (bool, error)
}

// Uses the existing deadline-bound, non-retrying Redis pool, with same-slot keys.
type redisTaskQueueMetricsStore struct{}

// Reads and admission are atomic, closing the miss/publication/acquisition race.
func (redisTaskQueueMetricsStore) acquire(ctx context.Context, token string) (value string, acquired bool, returnErr error) {
	const script = `local value = redis.call('GET',KEYS[1])
local acquired = redis.call('SET',KEYS[2],ARGV[1],'NX','PX',ARGV[2])
return {value or '', acquired and 1 or 0}`
	operationCtx, cancel := context.WithTimeout(ctx, taskMetricsRedisTimeout)
	defer cancel()
	returnErr = server.RedisWithDeadline(operationCtx, func(client server.RedisClient) error {
		values, err := client.Eval(operationCtx, script,
			[]string{taskMetricsCacheKey, taskMetricsOwnerKey}, token, taskMetricsRefreshInterval.Milliseconds()).Slice()
		if err != nil || len(values) != 2 {
			return errTaskQueueMetricsUnavailable
		}
		stored, valueOk := values[0].(string)
		admitted, admittedOk := values[1].(int64)
		if !valueOk || !admittedOk || admitted < 0 || admitted > 1 {
			return errTaskQueueMetricsUnavailable
		}
		value, acquired = stored, admitted == 1
		return nil
	})
	return
}

// An expired owner cannot overwrite a newer generation. Keeping the admission
// key also bounds attempts after a failed query or an ambiguous publication.
func (redisTaskQueueMetricsStore) publish(ctx context.Context, token, value string, ttl time.Duration) (published bool, returnErr error) {
	const script = `if redis.call('GET',KEYS[2]) ~= ARGV[1] then return 0 end
redis.call('SET',KEYS[1],ARGV[2],'PX',ARGV[3]); return 1`
	operationCtx, cancel := context.WithTimeout(ctx, taskMetricsRedisTimeout)
	defer cancel()
	returnErr = server.RedisWithDeadline(operationCtx, func(client server.RedisClient) error {
		result, err := client.Eval(operationCtx, script,
			[]string{taskMetricsCacheKey, taskMetricsOwnerKey}, token, value, ttl.Milliseconds()).Int64()
		if err != nil || result < 0 || result > 1 {
			return errTaskQueueMetricsUnavailable
		}
		published = result == 1
		return nil
	})
	return
}

// A count list of exactly four entries and a required age preserve real zeros
// while rejecting absent fields; all clocks retain nanosecond source precision.
type taskQueueMetricsWire struct {
	Version             int      `json:"v"`
	ObservedAt          int64    `json:"t"`
	Counts              []*int64 `json:"n"`
	OldestOverdueSecond *float64 `json:"o"`
}

// Rejects partial or impossible aggregates without changing overlapping states.
func validTaskQueueMetricsSample(sample *taskQueueMetricsSample, now time.Time) bool {
	if sample == nil || sample.observedAt.UnixNano() <= 0 || sample.observedAt.After(now.Add(taskMetricsClockSkew)) {
		return false
	}
	snapshot := sample.snapshot
	return snapshot.Total >= 0 && snapshot.Available >= 0 && snapshot.Available <= snapshot.Total &&
		snapshot.Claimed >= 0 && snapshot.Claimed <= snapshot.Total &&
		snapshot.RescheduleError >= 0 && snapshot.RescheduleError <= snapshot.Total &&
		snapshot.OldestOverdueSecond >= 0 && !math.IsNaN(snapshot.OldestOverdueSecond) &&
		!math.IsInf(snapshot.OldestOverdueSecond, 0) &&
		(snapshot.Available != 0 || snapshot.OldestOverdueSecond == 0)
}

// Reads never advance the observation time or renew the retained value.
func decodeTaskQueueMetricsSample(value string, now time.Time) (*taskQueueMetricsSample, error) {
	if len(value) == 0 || len(value) > taskMetricsWireBytes {
		return nil, errTaskQueueMetricsUnavailable
	}
	wire := taskQueueMetricsWire{}
	decoder := json.NewDecoder(strings.NewReader(value))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&wire) != nil || decoder.Decode(new(any)) != io.EOF ||
		wire.Version != 1 || len(wire.Counts) != 4 || wire.OldestOverdueSecond == nil {
		return nil, errTaskQueueMetricsUnavailable
	}
	for _, count := range wire.Counts {
		if count == nil {
			return nil, errTaskQueueMetricsUnavailable
		}
	}
	sample := &taskQueueMetricsSample{
		observedAt: time.Unix(0, wire.ObservedAt).UTC(),
		snapshot: taskQueueMetricsSnapshot{
			Total: *wire.Counts[0], Available: *wire.Counts[1], Claimed: *wire.Counts[2], RescheduleError: *wire.Counts[3],
			OldestOverdueSecond: *wire.OldestOverdueSecond,
		},
	}
	if !validTaskQueueMetricsSample(sample, now) {
		return nil, errTaskQueueMetricsUnavailable
	}
	return sample, nil
}

// Each observer has its own dependencies; only the Redis store is shared.
type taskQueueMetricsCache struct {
	store    taskQueueMetricsStore
	now      func() time.Time
	newToken func() string
	load     func(context.Context, time.Time) (taskQueueMetricsSnapshot, error)
}

// A cold follower returns no data immediately. A warm follower keeps the prior
// source timestamp even while its replacement is running or fails. No retry or
// database fallback is allowed when coordination is uncertain.
func (self taskQueueMetricsCache) get(ctx context.Context) (retained *taskQueueMetricsSample, returnErr error) {
	defer func() {
		if recover() != nil {
			returnErr = errTaskQueueMetricsUnavailable
		}
	}()
	if ctx.Err() != nil {
		return nil, errTaskQueueMetricsUnavailable
	}
	token, admittedAt := self.newToken(), self.now()
	value, acquired, err := self.store.acquire(ctx, token)
	if err != nil || ctx.Err() != nil {
		return nil, errTaskQueueMetricsUnavailable
	}
	if value != "" {
		retained, err = decodeTaskQueueMetricsSample(value, self.now())
		if err != nil {
			return nil, err
		}
	}
	if !acquired {
		return retained, nil
	}
	observedAt := self.now()
	if elapsed := observedAt.Sub(admittedAt); elapsed < 0 || elapsed >= taskMetricsRefreshInterval-taskMetricsQueryTimeout {
		return retained, errTaskQueueMetricsUnavailable
	}
	queryCtx, cancel := context.WithTimeout(ctx, taskMetricsQueryTimeout)
	defer cancel()
	snapshot, err := self.load(queryCtx, observedAt)
	completedAt := self.now()
	deadline, _ := queryCtx.Deadline()
	if err != nil || queryCtx.Err() != nil || !time.Now().Before(deadline) ||
		completedAt.Before(observedAt) || completedAt.Sub(observedAt) > taskMetricsQueryTimeout ||
		completedAt.Sub(admittedAt) >= taskMetricsRefreshInterval {
		return retained, errTaskQueueMetricsUnavailable
	}
	sample := &taskQueueMetricsSample{snapshot: snapshot, observedAt: observedAt}
	if !validTaskQueueMetricsSample(sample, completedAt) {
		return retained, errTaskQueueMetricsUnavailable
	}
	wire, err := json.Marshal(taskQueueMetricsWire{
		Version: 1, ObservedAt: observedAt.UnixNano(),
		Counts:              []*int64{&snapshot.Total, &snapshot.Available, &snapshot.Claimed, &snapshot.RescheduleError},
		OldestOverdueSecond: &snapshot.OldestOverdueSecond,
	})
	if err != nil || len(wire) > taskMetricsWireBytes {
		return retained, errTaskQueueMetricsUnavailable
	}
	// Redis publication occurs after the query released its PostgreSQL connection.
	// The publication's own deadline cannot extend the query's original timestamp.
	ttl := min(taskMetricsRetention, taskMetricsRetention-self.now().Sub(observedAt))
	if ttl <= 0 {
		return retained, errTaskQueueMetricsUnavailable
	}
	published, err := self.store.publish(ctx, token, string(wire), ttl)
	if err != nil || !published || ctx.Err() != nil {
		return retained, errTaskQueueMetricsUnavailable
	}
	return sample, nil
}
