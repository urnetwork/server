package connect

import (
	"context"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/model"
)

type connectAuthFinalReadTestKey struct{}

// Both handshake carriers use the same live authorization and instance checks.
// The private context hook gives native tests a precise last-read barrier.
func connectClientAuthentication(ctx context.Context, byJwt *jwt.ByJwt, instanceBytes []byte) (server.Id, int, error) {
	if err := jwt.ValidateByJwtState(ctx, byJwt, true); err != nil {
		return server.Id{}, http.StatusUnauthorized, err
	}
	instanceId, err := server.IdFromBytes(instanceBytes)
	if err != nil {
		return server.Id{}, http.StatusBadRequest, err
	}
	if before, ok := ctx.Value(connectAuthFinalReadTestKey{}).(func()); ok {
		before()
	}
	networkId := model.GetNetworkClientNetwork(ctx, *byJwt.ClientId)
	if networkId == nil || *networkId != byJwt.NetworkId {
		return server.Id{}, http.StatusForbidden, errors.New("Client id is not part of network.")
	}
	return instanceId, 0, nil
}

// Only the pre-admission checks cross this boundary. Db reports exhausted
// dependency work by panic, but H1+ must reject it before writing its 101.
// Unexpected panics still belong to the router's internal-error handling,
// even when the authentication deadline happens to expire at the same time.
func connectH1AuthenticationStatus(ctx context.Context, authenticate func() (int, error)) (status int) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err, ok := recovered.(error)
			if !ok || !connectAuthDependencyUnavailable(err) {
				panic(recovered)
			}
			status = http.StatusServiceUnavailable
		}
		if ctx.Err() != nil {
			status = http.StatusServiceUnavailable
		}
	}()
	if ctx.Err() != nil {
		return http.StatusServiceUnavailable
	}
	status, err := authenticate()
	if connectAuthDependencyUnavailable(err) {
		return http.StatusServiceUnavailable
	}
	if err != nil && status == 0 {
		// A callback must never publish successful admission with an error.
		// Preserve an unchecked program failure for the router, not a retry.
		panic(err)
	}
	return status
}

// Match session authentication's selective dependency classification. A bad
// credential or an unrelated program error must not become a retryable outage.
func connectAuthDependencyUnavailable(err error) bool {
	if errors.Is(err, server.DbContextDoneError) ||
		errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || pgconn.Timeout(err) {
		return true
	}
	var connectError *pgconn.ConnectError
	return errors.As(err, &connectError)
}
