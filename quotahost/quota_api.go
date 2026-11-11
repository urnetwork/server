package quotahost

import (
	"context"
	"errors"

	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The Quota of a host with no Postgres (§19.4: evs, gkm, sockets, tlsedge, mxedge), by plan-QUOTA-host-reach's
// answer (QD-055): limits, the over-limit list, usage batches, a payer's draw order and the spent mark over
// api's internal surface, the transport; CountRate and the lease reservations straight to redis, the host's
// ServiceHost.Redis. It runs until ctx ends or Drain.
//
// api's internal quota routes and their per-caller bearer (plan-TLS-internal-credentials' scheme) are not
// served yet, so no transport reaches api: a host passes nil, every call that needs api answers
// ErrQuotaApiUnavailable, and the Quota is never Ready. That is the interim the resolution gives: a real
// evs, gkm, sockets, tlsedge or mxedge host stays not ready on these calls until the route and its
// credential exist. Tests pass a fake transport.
func NewApiQuota(ctx context.Context, transport QuotaApiTransport, redis model.QuotaRateRedis, clock sidecar.Clock, settings *QuotaSettings) (*HostQuota, error) {
	if transport == nil {
		transport = unavailableQuotaApi{}
	}
	if redis == nil {
		return nil, errors.New("quotahost: an api-backed Quota needs the host's Redis")
	}
	return newHostQuota(ctx, transport, redis, clock, settings)
}

// api's internal quota surface as a host reaches it: one call a read or write, each on the caller's
// credential. The calls are QuotaSource's.
type QuotaApiTransport interface {
	QuotaSource
}

// What a host with no transport answers.
var ErrQuotaApiUnavailable = errors.New("quotahost: api's internal quota surface is not reachable from this host")

// The transport of a host that has none: every call fails closed.
type unavailableQuotaApi struct{}

func (unavailableQuotaApi) DrawOrder(ctx context.Context, networkId server.Id) ([]*LeaseBalance, error) {
	return nil, ErrQuotaApiUnavailable
}

func (unavailableQuotaApi) MarkSpent(ctx context.Context, networkId server.Id) error {
	return ErrQuotaApiUnavailable
}

func (unavailableQuotaApi) LoadLimits(ctx context.Context, networkId server.Id) (*sidecar.QuotaLimits, error) {
	return nil, ErrQuotaApiUnavailable
}

func (unavailableQuotaApi) LoadOverLimit(ctx context.Context) (*sidecar.QuotaOverLimit, error) {
	return nil, ErrQuotaApiUnavailable
}

func (unavailableQuotaApi) ApplyUsageBatch(ctx context.Context, batch *sidecar.UsageBatch, carves []*model.UsageCarve) error {
	return ErrQuotaApiUnavailable
}
