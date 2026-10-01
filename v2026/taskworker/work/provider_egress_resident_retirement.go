package work

import (
	"context"
	"errors"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"
)

var egressProbeResidentRetirementTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork", Subsystem: "egress_probe", Name: "resident_retirement_total",
	Help: "Owned probe resident capture and conditional removal; not measured provider coverage",
}, []string{"operation", "result"})

func init() {
	for _, operation := range []string{"capture", "commit"} {
		for _, result := range []string{"ok", "noop", "error", "timeout", "canceled"} {
			egressProbeResidentRetirementTotal.WithLabelValues(operation, result)
		}
	}
	prometheus.MustRegister(egressProbeResidentRetirementTotal)
}

func recordProviderEgressResidentRetirement(operation string, changed bool, err error) {
	if operation != "capture" && operation != "commit" {
		return
	}
	result := "noop"
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		result = "timeout"
	case errors.Is(err, context.Canceled):
		result = "canceled"
	case err != nil:
		result = "error"
	case changed:
		result = "ok"
	}
	egressProbeResidentRetirementTotal.WithLabelValues(operation, result).Inc()
}

var _ connect.NetworkClientResidentRetirement = (*providerEgressCredentials)(nil)

// Only this immutable authority's minted children are eligible. The model token
// binds both the original client instance and exact resident bytes. SQL removal
// still revalidates the parent JWT and applies its transactional network fence.
func (self *providerEgressCredentials) PrepareResidentRetirement(ctx context.Context, clientId, instanceId connect.Id) (commit func(context.Context) error, returnErr error) {
	defer func() { recordProviderEgressResidentRetirement("capture", commit != nil, returnErr) }()
	return server.HandleError2(func() (func(context.Context) error, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		child := server.Id(clientId)
		if child == (server.Id{}) || child == self.clientId || instanceId == (connect.Id{}) {
			return nil, errors.New("resident retirement requires an owned derived instance")
		}
		if _, owned := self.children.Load(child); !owned {
			return nil, nil
		}
		token, err := self.captureResident(ctx, child, server.Id(instanceId))
		if err != nil || token == nil {
			return nil, err
		}
		return func(commitCtx context.Context) (commitErr error) {
			removed := false
			defer func() { recordProviderEgressResidentRetirement("commit", removed, commitErr) }()
			return server.HandleError1(func() error {
				// Successful RemoveNetworkClient is the only operation that
				// releases this minted child from the owner's set. A premature
				// callback cannot remove residency while SQL is still active.
				if _, active := self.children.Load(child); active {
					return errors.New("resident retirement preceded identity retirement")
				}
				var err error
				removed, err = self.removeResident(commitCtx, token)
				return err
			}, func(err error) error {
				if commitCtx.Err() != nil {
					err = commitCtx.Err()
				}
				return err
			})
		}, nil
	}, func(err error) (func(context.Context) error, error) {
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return nil, err
	})
}
