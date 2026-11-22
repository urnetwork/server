// Registration and SQL association are inseparable from production signing.
package session

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
)

var sessionCreationOverride atomic.Pointer[bool]

// How long a read of vault auth.yml's session_creation_enabled answers every
// mint: a flip delivered to a host applies within this, without a restart.
const sessionCreationReadInterval = 5 * time.Second

type sessionCreationRead struct {
	value bool
	until time.Time
}

var sessionCreationCache atomic.Pointer[sessionCreationRead]
var sessionCreationReadLock sync.Mutex

// The vault read; a seam for tests.
var readSessionCreationSetting = func() bool { return loadAuthSettingBool("session_creation_enabled") }

// SessionCreationEnabled is vault auth.yml's session_creation_enabled, read at
// most once per sessionCreationReadInterval (S1 W10: every mint read and
// parsed the file, and a file that failed to parse panicked the mint). A read
// that fails keeps the last value read, false before any.
func SessionCreationEnabled() bool {
	if value := sessionCreationOverride.Load(); value != nil {
		return *value
	}
	now := time.Now()
	if cached := sessionCreationCache.Load(); cached != nil && now.Before(cached.until) {
		return cached.value
	}
	sessionCreationReadLock.Lock()
	defer sessionCreationReadLock.Unlock()
	if cached := sessionCreationCache.Load(); cached != nil && now.Before(cached.until) {
		return cached.value
	}
	value := false
	if cached := sessionCreationCache.Load(); cached != nil {
		value = cached.value
	}
	func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				glog.Infof("[session]session_creation_enabled could not be read; keeping %t: %v\n", value, recovered)
			}
		}()
		value = readSessionCreationSetting()
	}()
	sessionCreationCache.Store(&sessionCreationRead{value: value, until: now.Add(sessionCreationReadInterval)})
	return value
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
	fresh := true
	if !independent {
		derived := LegacySessionId(credential, sid)
		fresh = derived == sid
		sid = derived
	}
	credential.SessionId = &sid
	// R-mint-redis (session_mint_untagged.go)
	credential.mintStart = &sessionMintStart{sessionId: sid, fresh: fresh}
}

// PrepareOwnSessionMint gives the client a mint creates a session of its own,
// whatever the creating credential carries (owner decision 1: every hosted
// proxy keeps its own listed, revocable session). Its lineage starts at the
// mint: the creator's sign-in is no parent of it, so revoking that sign-in, a
// password reset or a sign-out-all does not end it. Allocate outside retryable
// callbacks, like PrepareSessionMint.
func PrepareOwnSessionMint(credential *ByJwt) {
	if !SessionCreationEnabled() {
		return
	}
	sid := server.NewId()
	credential.SessionId = &sid
	credential.CreateTime = server.CodecTime(server.NowUtc())
	// R-mint-redis (session_mint_untagged.go)
	credential.mintStart = &sessionMintStart{sessionId: sid, fresh: true}
}

// A client session that can carry a reissued client's credential: live,
// unrevoked, of a client class, never carried by a network credential.
const sessionReusableLua = `
local accept=redis.call('ZSCORE',KEYS[1],ARGV[2])
if not accept or tonumber(accept)<=tonumber(ARGV[1]) or redis.call('EXISTS',KEYS[3])==1 then return 0 end
local class=redis.call('HGET',KEYS[2],'class')
if (class=='hosted_proxy' or class=='api_key' or class=='device' or class=='legacy') and redis.call('HGET',KEYS[2],'network')~='1' then return 1 end
return 0
`

// ReuseClientSessionInTx keeps one session per client (owner decision 1) when
// an API key or an MCP identity reissues an existing client: the credential
// carries the client's own live session instead of the fresh one the
// independent mint prepared, which would rebind the client and leave its own
// session listed and unowned. A client bound to a sign-in session, or to none
// that can be reused, keeps the prepared session, as before. Call it under the
// shared lifecycle lock, before AssociateSessionClientInTx.
func ReuseClientSessionInTx(ctx context.Context, tx server.PgTx, credential *ByJwt) error {
	if credential.ClientId == nil || credential.SessionId == nil {
		return nil
	}
	var associated *server.Id
	err := tx.QueryRow(ctx, `SELECT session_id FROM network_client WHERE client_id=$1 AND network_id=$2 AND active FOR UPDATE`, *credential.ClientId, credential.NetworkId).Scan(&associated)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if associated == nil || *associated == *credential.SessionId {
		return nil
	}
	var reusable int64
	err = server.RedisAuth(ctx, func(ctx context.Context, r server.RedisClient) error {
		var err error
		reusable, err = r.Eval(ctx, sessionReusableLua, []string{SessionKey(credential.NetworkId, "z"), SessionKey(credential.NetworkId, "s:"+associated.String()), SessionMarkerKey(credential.NetworkId, *associated)}, server.NowUtc().UnixMilli(), associated.String()).Int64()
		return err
	})
	if err != nil {
		return errors.Join(ErrAuthUnavailable, ErrSessionStoreUnavailable)
	}
	if reusable == 1 {
		sid := *associated
		credential.SessionId = &sid
	}
	return nil
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
		// The exemption read and the registration each within the mint
		// budget. When Redis answers neither, a mint that starts its session
		// signs it untagged, and one that carries its presented credential's
		// session signs it unextended (session_mint_untagged.go): a top-level
		// client's exemption is read from Redis too, and a read that waited out
		// the caller failed the whole mint.
		exempt, err := sessionMintExemption(ctx, tx, credential)
		var registered *registeredSession
		if err == nil {
			registered, err = registerSessionMint(ctx, credential, kind, origin, fenceOrigin, exempt)
		}
		if err != nil {
			if untagErr := untagUnregisteredSessionMintInTx(ctx, tx, credential, kind, err); untagErr != nil {
				return "", untagErr
			}
			return sign(credential), nil
		}
		// Only a session the mint added can be the network's earliest review: a
		// refresh only extends a member, and the network's index entry already
		// reviews at or before it. The new member is published at once; only a
		// failed publication goes to the outbox, whose single row per network
		// every mint used to upsert (S1 W7). An index entry published by a
		// transaction that then rolls back only reviews early.
		if err := publishRegisteredSessionInTx(ctx, tx, credential.NetworkId, registered); err != nil {
			return "", err
		}
	}
	return sign(credential), nil
}

// Publishes a registration that added its session to the live set to the
// network's maintenance index, or queues the outbox when the publication
// fails. A refresh adds nothing to publish (RegisterAndSignInTx).
func publishRegisteredSessionInTx(ctx context.Context, tx server.PgTx, networkId server.Id, registered *registeredSession) error {
	if !registered.Created {
		return nil
	}
	due := time.UnixMilli(registered.AcceptUntil).UTC()
	// the Redis step within the mint budget (session_mint_untagged.go); the
	// outbox write keeps the transaction's context
	publishCtx, cancelPublish := context.WithTimeout(ctx, SessionMintRedisBudgetFor(ctx))
	publishErr := publishSessionIndexOnMint(publishCtx, networkId, due, server.NewId().String())
	cancelPublish()
	if publishErr != nil {
		// in UTC, as next_review_time holds it
		return QueueSessionIndexInTx(ctx, tx, networkId, due)
	}
	return nil
}

// RegisterRestoredSessionInTx re-registers the presented credential's own
// session before it authorizes more of that session (auth-code creation):
// the clock check and the index publication of every registered mint, with
// kind restored for a member the store no longer holds.
func RegisterRestoredSessionInTx(ctx context.Context, tx server.PgTx, credential *ByJwt, now time.Time) (*registeredSession, error) {
	if err := verifySessionMintClock(ctx); err != nil {
		return nil, err
	}
	registered, err := RegisterNetworkSession(ctx, credential, "restored", nil, false, now)
	if err != nil {
		return nil, err
	}
	if err := publishRegisteredSessionInTx(ctx, tx, credential.NetworkId, registered); err != nil {
		return nil, err
	}
	return registered, nil
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
//
// A hosted proxy survives a password reset and sign-out-all (owner decision
// 4): neither retires its own session (sessionRevokeLua), and a reset that
// moved the network's credential cutoff past the stored lineage moves the
// lineage to the cutoff, so the proxy keeps minting. Its credentials minted
// before the reset are refused by the rotation like every other credential
// of the network, until the proxy's device mints again. The proxy ends when
// it is removed, or when its own session is revoked: the journal refuses the
// revoked binding and the session's cleanup deactivates the client, so it can
// never bootstrap another.
//
// Each Redis step of the mint waits hostedSessionMintRedisBudget, the hosted
// path's check budget, not the mint budget: a stored binding Redis does not
// register within it fails the device's open (session_mint_untagged.go), and
// a bootstrap is signed untagged, while the proxy hosts' in-process Redis
// calls overran the mint budget 5-15% of the time on 2026-11-21.
func MintHostedSession(ctx context.Context, credential *ByJwt) (signed string, err error) {
	ctx = withSessionMintRedisBudget(ctx, hostedSessionMintRedisBudget)
	candidate := server.NewId()
	created := server.CodecTime(server.NowUtc())
	err = sessionTx(ctx, func(tx server.PgTx) error {
		if err := LockSessionLifecycle(ctx, tx, credential.NetworkId, false); err != nil {
			return err
		}
		var sid *server.Id
		var lineage *time.Time
		if err := tx.QueryRow(ctx, `SELECT session_id,session_create_time FROM network_client WHERE network_id=$1 AND client_id=$2 AND active FOR UPDATE`, credential.NetworkId, credential.ClientId).Scan(&sid, &lineage); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				// a removed proxy (its own session revoked and cleaned up, or
				// the client deleted) is a definitive refusal, not a bare
				// "no rows in result set" (session1118 D F3)
				return ErrByJwtInactive
			}
			return err
		}
		if sid != nil {
			// A hosted loader has no old JWT expiry to bound a newly fabricated
			// token. Unfinished cleanup must therefore fence that durable binding
			// even after its finite Redis marker/receipt has naturally expired.
			if err := hostedSessionJournalFence(ctx, tx, credential.NetworkId, *sid); err != nil {
				return err
			}
		}
		bootstrap := sid == nil && SessionCreationEnabled()
		if bootstrap {
			sid = &candidate
			lineage = &created
		}
		credential.SessionId = sid
		// R-mint-redis (session_mint_untagged.go): only the bootstrap starts
		// its session; a stored binding is carried
		credential.mintStart = nil
		if bootstrap {
			credential.mintStart = &sessionMintStart{sessionId: candidate, fresh: true}
		}
		if lineage != nil {
			var cutoff time.Time
			if err := tx.QueryRow(ctx, `SELECT u.credential_change_time FROM network_user u JOIN network n ON n.admin_user_id=u.user_id WHERE n.network_id=$1`, credential.NetworkId).Scan(&cutoff); err != nil {
				return err
			}
			if lineage.Before(cutoff) {
				moved := server.CodecTime(cutoff.UTC())
				if moved.Before(created) {
					moved = created
				}
				lineage = &moved
			}
			credential.CreateTime = *lineage
		}
		if err := ValidateByJwtStateInTx(ctx, tx, credential, true); err != nil {
			return err
		}
		if err := AssociateSessionClientInTx(ctx, tx, credential, false); err != nil {
			return err
		}
		var err error
		signed, err = RegisterAndSignInTx(ctx, tx, credential, MintKindLegacyProxy, nil, false)
		return err
	})
	return
}

// The journal's verdict on a hosted proxy's own session: as
// sessionJournalFence, except that a pending reset or sign out of other
// sessions never applies to it (sessionRevokeLua skips hosted proxy sessions),
// so neither makes its mint wait for recovery.
func hostedSessionJournalFence(ctx context.Context, conn server.PgCanQuery, networkId, sessionId server.Id) error {
	var retired, pending bool
	rows, err := conn.Query(ctx, `SELECT
		EXISTS(SELECT 1 FROM network_session_operation WHERE network_id=$1
			AND status IN ('enforced','complete') AND target_session_ids ? $2),
		EXISTS(SELECT 1 FROM network_session_operation WHERE network_id=$1 AND status='prepared'
			AND ((action='single' AND target_session_id=$2::uuid) OR action='delete'))`,
		networkId, sessionId.String())
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
		return ErrAuthUnavailable
	}
	return nil
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
