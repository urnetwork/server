package session

import (
	"context"
	"errors"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/urnetwork/server/v2026"
)

var sessionAuthorityLoss = prometheus.NewCounter(prometheus.CounterOpts{Name: "urnetwork_session_authority_loss_total", Help: "Journalled revocations whose expected Redis receipt and markers are missing"})

func init() { prometheus.MustRegister(sessionAuthorityLoss) }

// The journal is not an allowlist. If Redis loses an acknowledged denylist, do
// not turn a historical SQL result into a new claim of authoritative success.
func verifySessionOperationAuthority(ctx context.Context, op *storedSessionOperation) error {
	if (op.Status != "enforced" && op.Status != "complete") || len(op.Targets) == 0 || !server.NowUtc().Before(op.RetainUntil) {
		return nil
	}
	receipt, err := readSessionReceipt(ctx, op.NetworkId, op.OperationId)
	if err != nil || receipt != nil {
		return err
	}
	keys := make([]string, 0, len(op.Targets))
	for _, sid := range op.Targets {
		keys = append(keys, SessionMarkerKey(op.NetworkId, sid))
	}
	var present int64
	err = server.RedisAuth(ctx, func(ctx context.Context, r server.RedisClient) error {
		var err error
		present, err = r.Exists(ctx, keys...).Result()
		return err
	})
	if err != nil {
		return errors.Join(ErrAuthUnavailable, ErrSessionStoreUnavailable)
	}
	if present != int64(len(keys)) {
		sessionAuthorityLoss.Inc()
		return errors.Join(ErrAuthUnavailable, ErrSessionStoreUnavailable)
	}
	return nil
}
