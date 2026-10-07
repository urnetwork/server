package controller

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

const (
	statsContractHourCacheTTL  = 5 * time.Minute
	statsContractHourFillTime  = 2 * time.Minute
	statsContractHourLeaseTTL  = 3 * time.Minute
	statsContractHourClockSkew = 5 * time.Second
)

var (
	errStatsContractHourUnavailable = errors.New("contract hourly stats unavailable")
	errStatsContractHourBusy        = errors.New("contract hourly stats refresh in progress")
	errStatsContractHourInvalid     = errors.New("contract hourly stats snapshot invalid")
)

// WindowEnd is also the source-read start: hits never advance the window.
// This cache belongs only to the periodic collector. Explicit model callers
// continue to count the exact window they request.
type statsContractHourSnapshot struct {
	Counts      model.ContractHourCounts
	WindowEnd   time.Time
	CompletedAt time.Time
}

func statsContractHourKeys(scope string) statsProviderEgressCacheKeys {
	prefix := "stats_contract_hour_window:v1:{" + scope + "}"
	return statsProviderEgressCacheKeys{prefix + ":value", prefix + ":fill"}
}

// Reuse the bounded Redis operations and token-fenced publication primitive;
// contract snapshots have their own keys, codec, freshness and source read.
type statsContractHourCache struct {
	store    statsProviderEgressCacheStore
	keys     statsProviderEgressCacheKeys
	now      func() time.Time
	newToken func() string
	fill     func(context.Context, time.Time) (model.ContractHourCounts, error)
}

type statsContractHourWire struct {
	Version   int                      `json:"v"`
	WindowEnd int64                    `json:"s"`
	Completed int64                    `json:"c"`
	Counts    model.ContractHourCounts `json:"n"`
}

func statsContractHourSnapshotFresh(snapshot *statsContractHourSnapshot, now time.Time) bool {
	return snapshot != nil && snapshot.Counts.Contracts >= 0 && snapshot.Counts.Disputes >= 0 && snapshot.Counts.WithExtender >= 0 &&
		snapshot.WindowEnd.UnixMilli() > 0 && !snapshot.CompletedAt.Before(snapshot.WindowEnd) &&
		snapshot.CompletedAt.Sub(snapshot.WindowEnd) <= statsContractHourFillTime &&
		!snapshot.CompletedAt.After(now.Add(statsContractHourClockSkew)) &&
		now.Sub(snapshot.WindowEnd) < statsContractHourCacheTTL
}

func encodeStatsContractHourSnapshot(snapshot *statsContractHourSnapshot, now time.Time) (string, error) {
	if !statsContractHourSnapshotFresh(snapshot, now) {
		return "", errStatsContractHourInvalid
	}
	wire, err := json.Marshal(statsContractHourWire{1, snapshot.WindowEnd.UnixMilli(), snapshot.CompletedAt.UnixMilli(), snapshot.Counts})
	if err != nil {
		return "", errStatsContractHourInvalid
	}
	return string(wire), nil
}

func decodeStatsContractHourSnapshot(value string, now time.Time) (*statsContractHourSnapshot, bool, error) {
	if len(value) == 0 || len(value) > 512 {
		return nil, false, errStatsContractHourInvalid
	}
	var wire statsContractHourWire
	if json.Unmarshal([]byte(value), &wire) != nil || wire.Version != 1 {
		return nil, false, errStatsContractHourInvalid
	}
	// Only our canonical closed schema is accepted, including all zero counts.
	// Round-trip equality also rejects duplicate, missing and unknown fields.
	canonical, err := json.Marshal(wire)
	if err != nil || string(canonical) != value {
		return nil, false, errStatsContractHourInvalid
	}
	snapshot := &statsContractHourSnapshot{wire.Counts, time.UnixMilli(wire.WindowEnd), time.UnixMilli(wire.Completed)}
	// An expired, otherwise valid value is a miss; an invalid one cannot trigger
	// an unfenced database fallback.
	if !statsContractHourSnapshotFresh(snapshot, snapshot.CompletedAt) || snapshot.CompletedAt.After(now.Add(statsContractHourClockSkew)) {
		return nil, false, errStatsContractHourInvalid
	}
	return snapshot, statsContractHourSnapshotFresh(snapshot, now), nil
}

func (cache statsContractHourCache) read(ctx context.Context) (*statsContractHourSnapshot, bool, error) {
	value, exists, err := cache.store.get(ctx, cache.keys.value)
	if err != nil || ctx.Err() != nil {
		return nil, false, errStatsContractHourUnavailable
	}
	if !exists {
		return nil, false, nil
	}
	return decodeStatsContractHourSnapshot(value, cache.now())
}

func statsContractHourFill(ctx context.Context, now time.Time, fill func(context.Context, time.Time) (model.ContractHourCounts, error)) (counts model.ContractHourCounts, err error) {
	defer func() {
		if recover() != nil {
			counts, err = model.ContractHourCounts{}, errStatsContractHourUnavailable
		}
	}()
	return fill(ctx, now)
}

func (cache statsContractHourCache) get(ctx context.Context) (*statsContractHourSnapshot, error) {
	if ctx.Err() != nil {
		return nil, errStatsContractHourUnavailable
	}
	if snapshot, fresh, err := cache.read(ctx); err != nil {
		return nil, err
	} else if fresh {
		return snapshot, nil
	}
	token, leaseStarted := cache.newToken(), cache.now()
	acquired, acquireErr := cache.store.acquire(ctx, cache.keys.fill, token, statsContractHourLeaseTTL)
	if acquireErr != nil {
		owner, exists, err := cache.store.get(ctx, cache.keys.fill)
		if err != nil || !exists || owner != token || ctx.Err() != nil {
			return nil, errStatsContractHourUnavailable
		}
		acquired = true
	}
	// Another owner may have published between our miss and lease acquisition.
	if snapshot, fresh, err := cache.read(ctx); err != nil {
		return nil, err
	} else if fresh {
		if acquired && cache.store.release(ctx, cache.keys.fill, token) != nil || ctx.Err() != nil {
			return nil, errStatsContractHourUnavailable
		}
		return snapshot, nil
	}
	if !acquired {
		return nil, errStatsContractHourBusy
	}
	if cache.now().Sub(leaseStarted) >= statsContractHourLeaseTTL-statsContractHourFillTime {
		return nil, errStatsContractHourUnavailable
	}
	fillCtx, cancel := context.WithTimeout(ctx, statsContractHourFillTime)
	defer cancel()
	fillStarted := cache.now()
	started := fillStarted.Truncate(time.Millisecond)
	counts, err := statsContractHourFill(fillCtx, started, cache.fill)
	fillCompleted := cache.now()
	completed := fillCompleted.Truncate(time.Millisecond)
	deadline, _ := fillCtx.Deadline()
	if err != nil || fillCtx.Err() != nil || !time.Now().Before(deadline) || fillCompleted.Sub(fillStarted) > statsContractHourFillTime || completed.Before(started) || fillCompleted.Sub(leaseStarted) >= statsContractHourLeaseTTL {
		// Keep the failed owner's lease as backoff; followers must not fan out.
		return nil, errStatsContractHourUnavailable
	}
	snapshot := &statsContractHourSnapshot{counts, started, completed}
	value, err := encodeStatsContractHourSnapshot(snapshot, completed)
	if err != nil {
		return nil, err
	}
	publicationTime := cache.now()
	remaining := min(statsContractHourCacheTTL, statsContractHourCacheTTL-publicationTime.Sub(started))
	if remaining <= 0 || fillCtx.Err() != nil || !time.Now().Before(deadline) || publicationTime.Sub(leaseStarted) >= statsContractHourLeaseTTL {
		return nil, errStatsContractHourUnavailable
	}
	published, publishErr := cache.store.publish(fillCtx, cache.keys, token, value, remaining)
	if fillCtx.Err() != nil || !time.Now().Before(deadline) {
		return nil, errStatsContractHourUnavailable
	}
	if publishErr == nil && published && statsContractHourSnapshotFresh(snapshot, cache.now()) {
		return snapshot, nil
	}
	// Lost replies or replaced leases recover only a validated shared value.
	if retained, fresh, err := cache.read(fillCtx); err == nil && fresh {
		return retained, nil
	}
	return nil, errStatsContractHourUnavailable
}

func getStatsContractHourSnapshot(ctx context.Context) (snapshot *statsContractHourSnapshot, err error) {
	defer func() {
		if recover() != nil {
			snapshot, err = nil, errStatsContractHourUnavailable
		}
	}()
	cache := statsContractHourCache{
		store: redisStatsProviderEgressCacheStore{}, keys: statsContractHourKeys("network"), now: time.Now,
		newToken: func() string { return server.NewId().String() },
		fill: func(ctx context.Context, now time.Time) (model.ContractHourCounts, error) {
			return model.CountContractHourWindow(ctx, now), nil
		},
	}
	return cache.get(ctx)
}
