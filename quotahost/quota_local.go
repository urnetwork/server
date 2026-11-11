package quotahost

import (
	"context"

	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The Quota of a process that holds Postgres (api, connect, taskworker), over model directly, its rates and
// reservations on redis, nil being this process's own Redis, at clock, nil being real time. It runs until
// ctx ends or Drain.
func NewLocalQuota(ctx context.Context, redis model.QuotaRateRedis, clock sidecar.Clock, settings *QuotaSettings) (*HostQuota, error) {
	if settings == nil {
		settings = DefaultQuotaSettings()
	}
	if clock == nil {
		clock = sidecar.RealClock{}
	}
	if redis == nil {
		rateSettings := settings.Rate
		if rateSettings == nil {
			rateSettings = model.DefaultQuotaRateSettings()
		}
		redis = model.NewQuotaRateServerRedis(rateSettings.CallTimeout)
	}
	return newHostQuota(ctx, NewLocalQuotaSource(clock), redis, clock, settings)
}

// model's reads and writes as a QuotaSource, at clock (nil being real time).
func NewLocalQuotaSource(clock sidecar.Clock) QuotaSource {
	if clock == nil {
		clock = sidecar.RealClock{}
	}
	return &localQuotaSource{
		BalanceSource: NewLocalBalanceSource(clock),
		clock:         clock,
	}
}

type localQuotaSource struct {
	BalanceSource
	clock sidecar.Clock
}

func (self *localQuotaSource) LoadLimits(ctx context.Context, networkId server.Id) (limits *sidecar.QuotaLimits, returnErr error) {
	server.HandleError(func() {
		limits = model.NetworkQuotaLimits(ctx, networkId)
	}, func(err error) {
		returnErr = err
	})
	return
}

func (self *localQuotaSource) LoadOverLimit(ctx context.Context) (overLimit *sidecar.QuotaOverLimit, returnErr error) {
	server.HandleError(func() {
		overLimit, returnErr = model.GetQuotaOverLimit(ctx)
	}, func(err error) {
		returnErr = err
	})
	return
}

func (self *localQuotaSource) ApplyUsageBatch(ctx context.Context, batch *sidecar.UsageBatch, carves []*model.UsageCarve) (returnErr error) {
	server.HandleError(func() {
		_, returnErr = model.ApplyUsageBatch(ctx, batch, carves, self.clock.Now())
	}, func(err error) {
		returnErr = err
	})
	return
}
