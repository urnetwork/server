package connect

import (
	"context"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

type connectAuthFinalReadTestKey struct{}

// The cause an H1 handler context ends with when its request context ends:
// the client hung up (or its proxy closed the upstream) before admission.
// net/http ends that context only when the connection's read fails, never
// for its read timeout while the handler runs, and not after a hijack.
// Before the 101, H1+ authorizes under that context, so the departure cancels
// the step in flight, and the step reports it as an unavailable dependency.
var errConnectRequestCanceled = errors.New("connect request canceled")

// Whether ctx ended because its client left. A deadline or a server-side
// cancel that ended ctx first keeps its own cause, so only a departure that
// actually ended the work is reported.
func connectRequestCanceled(ctx context.Context) bool {
	return errors.Is(context.Cause(ctx), errConnectRequestCanceled)
}

// A lag.dev guest's token is refused by the connect transport.
var errLagGuestConnect = errors.New("A lag.dev guest token cannot connect.")

// Parse the instance without publishing an admission, then perform the full
// live authorization at the final read boundary. That query already binds the
// active client, device and network to the current administrator and credential
// epoch; a second weaker membership query would add no authority. Preserve
// credential-error precedence when the instance is malformed too.
func connectClientAuthentication(ctx context.Context, byJwt *session.ByJwt, instanceBytes []byte) (server.Id, int, error) {
	if byJwt.LagGuest {
		// a lag.dev guest's token serves the lag routes alone and never
		// connects (lag.dev/PLAN.md A1, session/AUTH.md §1); refused before
		// any state is read
		return server.Id{}, http.StatusForbidden, errLagGuestConnect
	}
	instanceId, instanceErr := server.IdFromBytes(instanceBytes)
	if before, ok := ctx.Value(connectAuthFinalReadTestKey{}).(func()); ok {
		before()
	}
	if err := session.ValidateByJwtState(ctx, byJwt, true); err != nil {
		if connectAuthDependencyUnavailable(err) {
			return server.Id{}, http.StatusServiceUnavailable, err
		}
		return server.Id{}, http.StatusUnauthorized, err
	}
	if instanceErr != nil {
		return server.Id{}, http.StatusBadRequest, instanceErr
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
	if errors.Is(err, session.ErrAuthUnavailable) || errors.Is(err, session.ErrSessionStoreUnavailable) {
		return true
	}
	if errors.Is(err, server.DbContextDoneError) ||
		errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || pgconn.Timeout(err) {
		return true
	}
	var connectError *pgconn.ConnectError
	return errors.As(err, &connectError)
}
