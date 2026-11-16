// Registration and SQL association are inseparable from production signing.
package session

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/urnetwork/server"
)

var sessionCreationOverride atomic.Pointer[bool]

func SessionCreationEnabled() bool {
	if value := sessionCreationOverride.Load(); value != nil {
		return *value
	}
	return loadAuthSettingBool("session_creation_enabled")
}
func Testing_SetSessionCreationEnabled(value bool) func() {
	old := sessionCreationOverride.Swap(&value)
	return func() { sessionCreationOverride.Store(old) }
}

// Allocate outside retryable callbacks. A zero lineage time gets one stable
// operation identity; tagged parents always extend registration even when dark.
func PrepareSessionMint(credential *ByJwt, independent bool) {
	if credential.SessionId != nil || !SessionCreationEnabled() {
		return
	}
	sid := server.NewId()
	if !independent {
		sid = LegacySessionId(credential, sid)
	}
	credential.SessionId = &sid
}

// Callers acquire the shared lifecycle lock before touching any client/code row.
// Explicit network re-auth may replace association; refresh/replay never does.
func AssociateSessionClientInTx(ctx context.Context, tx server.PgTx, credential *ByJwt, rebind bool) error {
	if credential.ClientId == nil {
		return nil
	}
	var associated *server.Id
	var created *time.Time
	if err := tx.QueryRow(ctx, `SELECT session_id,session_create_time FROM network_client WHERE client_id=$1 AND network_id=$2 AND active AND device_id=$3 FOR UPDATE`, *credential.ClientId, credential.NetworkId, credential.DeviceId).Scan(&associated, &created); err != nil {
		return err
	}
	root, err := ResolveClientRootInTx(ctx, tx, credential.NetworkId, *credential.ClientId)
	if err != nil {
		return err
	}
	credential.RootClientId = &root
	if credential.SessionId == nil {
		return nil
	}
	if associated != nil && *associated != *credential.SessionId && !rebind {
		return sessionError("client_session_changed")
	}
	_, err = tx.Exec(ctx, `UPDATE network_client SET session_id=$3,session_create_time=$4 WHERE client_id=$1 AND network_id=$2`, *credential.ClientId, credential.NetworkId, credential.SessionId, credential.CreateTime)
	return err
}

func RegisterAndSignInTx(ctx context.Context, tx server.PgTx, credential *ByJwt, kind string, origin *server.Id, fenceOrigin bool) (string, error) {
	if credential.ClientId != nil && credential.RootClientId == nil {
		return "", errors.New("client mint requires resolved root")
	}
	if credential.SessionId != nil {
		if err := verifySessionMintClock(ctx); err != nil {
			return "", err
		}
		registered, err := RegisterNetworkSession(ctx, credential, kind, origin, fenceOrigin, server.NowUtc())
		if err != nil {
			return "", err
		}
		// in UTC, as next_review_time holds it
		if err = QueueSessionIndexInTx(ctx, tx, credential.NetworkId, time.UnixMilli(registered.AcceptUntil).UTC()); err != nil {
			return "", err
		}
	}
	return sign(credential), nil
}

// Independent successful password/SSO/wallet/verification/seedphrase sign-ins.
func MintNetworkSession(ctx context.Context, credential *ByJwt, kind string) (signed string, err error) {
	PrepareSessionMint(credential, true)
	err = sessionTx(ctx, func(tx server.PgTx) error {
		if err := LockSessionLifecycle(ctx, tx, credential.NetworkId, false); err != nil {
			return err
		}
		if err := ValidateByJwtStateInTx(ctx, tx, credential, false); err != nil {
			return err
		}
		var err error
		signed, err = RegisterAndSignInTx(ctx, tx, credential, kind, nil, false)
		return err
	})
	if err == nil {
		if observation, ok := ctx.Value(clientObservationContextKey{}).(*ClientSession); ok {
			observation.WithByJwt(credential).ObserveAuthenticatedUse()
		}
	}
	return
}

// Renewal preserves the presented authority, actual topology, and credential
// lineage time. Rebinding is exclusively an explicit network auth-client action.
//
// The presented credential is checked again in the renewal's transaction,
// under the shared lifecycle lock that every credential rotation (password
// set, sign-out-all) takes exclusively. A rotation or revocation that commits
// after the router's check refuses the renewal here, and the refusal gets
// authentication's answer (answerRenewalRefusal).
//
// A non-nil clientLimitUpgrade replaces the presented token's
// client_limit_upgrade claim (ByJwt.ClientLimitUpgrade); nil keeps it.
func RenewSessionCredential(clientSession *ClientSession, networkName string, pro bool, clientLimitUpgrade *bool) (signed string, err error) {
	if clientSession.ApiKeyAuthenticated || clientSession.UnsignedIdentity {
		return "", sessionError("current_session_required")
	}
	credential := clientSession.ByJwt.Renew()
	credential.NetworkName = networkName
	credential.Pro = pro
	if clientLimitUpgrade != nil {
		credential.ClientLimitUpgrade = cloneClientLimitUpgrade(clientLimitUpgrade)
	}
	PrepareSessionMint(credential, false)
	err = sessionTx(clientSession.Ctx, func(tx server.PgTx) error {
		if err := LockSessionLifecycle(clientSession.Ctx, tx, credential.NetworkId, false); err != nil {
			return err
		}
		if err := ValidateByJwtStateInTx(clientSession.Ctx, tx, clientSession.ByJwt, credential.ClientId != nil); err != nil {
			return err
		}
		if err := AssociateSessionClientInTx(clientSession.Ctx, tx, credential, false); err != nil {
			return err
		}
		var err error
		signed, err = RegisterAndSignInTx(clientSession.Ctx, tx, credential, "legacy", nil, false)
		return err
	})
	if err != nil {
		return "", answerRenewalRefusal(err)
	}
	return
}

// RenewalRefused reports whether a renewal's own check refused the presented
// credential: it was rotated, removed or revoked after the router's check.
func RenewalRefused(err error) bool {
	var refusal *renewalRefusalError
	return errors.As(err, &refusal)
}

// Authentication's answer to a credential the renewal refused.
type renewalRefusalError struct {
	answer error
}

func (self *renewalRefusalError) Error() string { return self.answer.Error() }
func (self *renewalRefusalError) Unwrap() error { return self.answer }

// A definitive refusal of the presented credential gets the answer
// authentication gives the same refusal (router.authenticationHttpError): a
// revoked session the structured 401 session_revoked, any other refusal 401.
// It was an undeclared error, a 500 the client retries, while the router's
// check answers the same credential 401 and the client signs out. A store that
// could not answer is no refusal and stays ErrAuthUnavailable, a 503 that never
// signs the client out; every other failure is returned as it is.
func answerRenewalRefusal(err error) error {
	switch {
	case errors.Is(err, ErrSessionRevoked):
		return &renewalRefusalError{answer: sessionError("session_revoked")}
	case errors.Is(err, ErrAuthUnavailable):
		return err
	case authorizationRejectionDefinitive(err):
		return &renewalRefusalError{answer: fmt.Errorf("%d Not authorized.", http.StatusUnauthorized)}
	default:
		return err
	}
}

// Internal probes are explicitly exempt; no request-selected flag reaches here.
// They still carry their actual database root and the ordinary bounded lifetime.
func SignInternalProbeInTx(ctx context.Context, tx server.PgTx, credential *ByJwt) (string, error) {
	if credential.SessionId != nil {
		return "", errors.New("internal probe cannot inherit a session")
	}
	if credential.ClientId == nil {
		return "", errors.New("internal probe requires client")
	}
	root, err := ResolveClientRootInTx(ctx, tx, credential.NetworkId, *credential.ClientId)
	if err != nil {
		return "", err
	}
	credential.RootClientId = &root
	return sign(credential), nil
}

// Hosted migration allocates under the row lock and reuses the durable winner.
func MintHostedSession(ctx context.Context, credential *ByJwt) (signed string, err error) {
	candidate := server.NewId()
	created := server.CodecTime(server.NowUtc())
	err = sessionTx(ctx, func(tx server.PgTx) error {
		if err := LockSessionLifecycle(ctx, tx, credential.NetworkId, false); err != nil {
			return err
		}
		var sid *server.Id
		var lineage *time.Time
		if err := tx.QueryRow(ctx, `SELECT session_id,session_create_time FROM network_client WHERE network_id=$1 AND client_id=$2 AND active FOR UPDATE`, credential.NetworkId, credential.ClientId).Scan(&sid, &lineage); err != nil {
			return err
		}
		if sid != nil {
			// A hosted loader has no old JWT expiry to bound a newly fabricated
			// token. Unfinished cleanup must therefore fence that durable binding
			// even after its finite Redis marker/receipt has naturally expired.
			if err := validateHostedSessionJournalInTx(ctx, tx, credential.NetworkId, *sid, lineage); err != nil {
				return err
			}
		}
		if sid == nil && SessionCreationEnabled() {
			sid = &candidate
			lineage = &created
		}
		credential.SessionId = sid
		if lineage != nil {
			credential.CreateTime = *lineage
		}
		if err := ValidateByJwtStateInTx(ctx, tx, credential, true); err != nil {
			return err
		}
		if err := AssociateSessionClientInTx(ctx, tx, credential, false); err != nil {
			return err
		}
		var err error
		signed, err = RegisterAndSignInTx(ctx, tx, credential, "legacy_proxy", nil, false)
		return err
	})
	return
}

// Replay has no presented original token to bound expired Redis markers. The
// caller proves the original binding and horizon under its existing row locks;
// this fence also refuses retained revocation journals and current state loss.
func ValidateSessionReplayInTx(ctx context.Context, tx server.PgTx, credential *ByJwt) error {
	if credential.SessionId != nil {
		if err := validateHostedSessionJournalInTx(ctx, tx, credential.NetworkId, *credential.SessionId, &credential.CreateTime); err != nil {
			return err
		}
	}
	return ValidateByJwtStateInTx(ctx, tx, credential, true)
}

// Durable terminal operations outlive their finite authentication projections.
func validateHostedSessionJournalInTx(ctx context.Context, tx server.PgTx, networkId, sessionId server.Id, lineage *time.Time) error {
	return sessionJournalFence(ctx, tx, networkId, sessionId, lineage)
}

// The journal's verdict on one session: refused once an enforced operation
// retired it, unavailable while a prepared one may apply to it. The revocation
// record's SQL fallback reads it too, so lost Redis state cannot revive a
// session the journal still retains.
func sessionJournalFence(ctx context.Context, conn server.PgCanQuery, networkId, sessionId server.Id, lineage *time.Time) error {
	var retired, pending bool
	rows, err := conn.Query(ctx, `SELECT
		EXISTS(SELECT 1 FROM network_session_operation WHERE network_id=$1
			AND status IN ('enforced','complete') AND target_session_ids ? $2),
		EXISTS(SELECT 1 FROM network_session_operation WHERE network_id=$1 AND status='prepared'
			AND ((action='single' AND target_session_id=$2::uuid)
				OR (action='others' AND kept_session_id IS DISTINCT FROM $2::uuid)
				OR action='delete' OR (action='reset' AND ($3::timestamp IS NULL OR credential_epoch>$3))))`,
		networkId, sessionId.String(), lineage)
	if err == nil {
		if rows.Next() {
			err = rows.Scan(&retired, &pending)
		}
		rows.Close()
		if err == nil {
			err = rows.Err()
		}
	}
	if err != nil {
		return errors.Join(ErrAuthUnavailable, err)
	}
	if retired {
		return ErrSessionRevoked
	}
	if pending {
		// A prepared journal may have applied Redis before its SQL commit was
		// interrupted. Only recovery may establish its authoritative outcome.
		return ErrAuthUnavailable
	}
	return nil
}

func SignInternalProbe(ctx context.Context, credential *ByJwt) (signed string, err error) {
	err = sessionTx(ctx, func(tx server.PgTx) error {
		var err error
		signed, err = SignInternalProbeInTx(ctx, tx, credential)
		return err
	})
	return
}
