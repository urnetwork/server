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
	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
)

var sessionCreationOverride atomic.Pointer[bool]

// vault auth.yml session_creation_enabled is read at most this often. A
// delivery of the file turns creation on or off within this interval, with no
// restart; between reads every mint answers from the last read.
const sessionCreationSettingInterval = 5 * time.Second

var sessionCreationSetting struct {
	stateLock sync.Mutex
	readTime  time.Time
	value     bool
}

// the setting's clock, for tests
var sessionCreationSettingNow = time.Now

// Every mint asks. The setting used to be read and parsed from the vault file
// on every call, and a file that failed to parse panicked the mint (S1 W10).
// Now a read that fails keeps the last value read.
func SessionCreationEnabled() bool {
	if value := sessionCreationOverride.Load(); value != nil {
		return *value
	}
	setting := &sessionCreationSetting
	setting.stateLock.Lock()
	defer setting.stateLock.Unlock()
	now := sessionCreationSettingNow()
	if !setting.readTime.IsZero() && now.Sub(setting.readTime) < sessionCreationSettingInterval {
		return setting.value
	}
	setting.readTime = now
	if value, err := readAuthSettingBool("session_creation_enabled"); err == nil {
		setting.value = value
	} else {
		glog.Errorf("[session]vault auth.yml could not be read (%s); session_creation_enabled stays %t\n", err, setting.value)
	}
	return setting.value
}

// A bool key of vault auth.yml. A missing file or key is false; a file that
// does not parse is an error, not a panic.
func readAuthSettingBool(key string) (bool, error) {
	resource, err := server.Vault.SimpleResource("auth.yml")
	if err != nil {
		return false, nil
	}
	values, err := resource.ParseE()
	if err != nil {
		return false, err
	}
	value, ok := values[key].(bool)
	return ok && value, nil
}

// Testing_ResetSessionCreationSetting forgets the last read of the setting.
func Testing_ResetSessionCreationSetting() {
	sessionCreationSetting.stateLock.Lock()
	defer sessionCreationSetting.stateLock.Unlock()
	sessionCreationSetting.readTime = time.Time{}
	sessionCreationSetting.value = false
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
	credential.mintStart = &sessionMintStart{sessionId: sid, fresh: fresh}
}

// StartSessionMint gives credential a new session that starts at this mint,
// for a producer that allocates its own candidate id outside its retryable
// callback (device adoption).
func StartSessionMint(credential *ByJwt, sessionId server.Id) {
	credential.SessionId = &sessionId
	credential.mintStart = &sessionMintStart{sessionId: sessionId, fresh: true}
}

// The session a mint starts, when the mint chose its id: a new id, or the
// legacy lineage derived from an untagged credential. Neither is carried from
// a presented tagged credential or a stored association, so a token without it
// grants nothing its caller does not already hold (R-mint-redis).
type sessionMintStart struct {
	sessionId server.Id
	// a new random id, which no client row is bound to before this mint
	fresh bool
}

// The session credential starts at this mint, or nil.
func (self *ByJwt) startsSession() *sessionMintStart {
	if self.mintStart == nil || self.SessionId == nil || *self.SessionId != self.mintStart.sessionId {
		return nil
	}
	return self.mintStart
}

// A mint waits at most this long for each Redis step of its registration, the
// clock sample and the mint script, inside its SQL transaction. A Redis stall
// then holds a mint's PostgreSQL connection for at most two budgets, where
// RedisAuth alone allows two seconds each: at the 380 tagged mints a second
// S4 measured, two-second waits would hold more connections than PgBouncer
// has.
const sessionMintRedisBudget = RevocationCheckBudget

// The service label comes from the stats push grouping (StartStatsPusher).
var sessionMintUntaggedCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "urnetwork_session_mint_untagged_total",
	Help: "Mints that started a session but signed an untagged token because Redis did not register the session within the mint budget (R-mint-redis, decision 3), by session kind: signin, code or client. The token gets its session at its next refresh.",
}, []string{"kind"})

func init() {
	prometheus.MustRegister(sessionMintUntaggedCounter)
	for _, kind := range []string{SessionKindSignin, SessionKindCode, SessionKindClient} {
		sessionMintUntaggedCounter.WithLabelValues(kind)
	}
}

// The §15 kind of a mint's session, as the session cap counts it
// (sessionClass): a network credential's legacy lineage is a sign-in, a
// client credential's is the client's own.
func sessionKindClass(kind string, credential *ByJwt) string {
	return sessionKindLabel(sessionClass(kind, credential.ClientId == nil))
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

// Registers a tagged credential's session, then signs it.
//
// R-mint-redis (decision 3, 2026-11-18): when Redis does not answer the
// registration within sessionMintRedisBudget and the mint starts the session
// itself, the token is signed untagged, as tokens were before sessions, and
// counted. No tagged token then carries a session Redis does not list, and the
// token gets its session at its next refresh. A fresh session's client
// association, written earlier in this transaction, is cleared with it. A mint
// that carries a session from its presented credential or a stored association
// still needs its registration (the check-and-extend of REVOKE-FINAL.md §12):
// signing it untagged would let the new token outlive the revocation of its
// session, so it stays unavailable, and its caller keeps the credential it
// holds.
func RegisterAndSignInTx(ctx context.Context, tx server.PgTx, credential *ByJwt, kind string, origin *server.Id, fenceOrigin bool) (string, error) {
	if credential.ClientId != nil && credential.RootClientId == nil {
		return "", errors.New("client mint requires resolved root")
	}
	if credential.SessionId != nil {
		exempt, err := sessionMintExemption(ctx, tx, credential)
		if err != nil {
			return "", err
		}
		registered, err := registerSessionMint(ctx, credential, kind, origin, fenceOrigin, exempt)
		if err != nil {
			start := credential.startsSession()
			if start == nil || !errors.Is(err, ErrSessionStoreUnavailable) || ctx.Err() != nil {
				return "", err
			}
			if err := untagSessionMintInTx(ctx, tx, credential, start); err != nil {
				return "", err
			}
			sessionMintUntaggedCounter.WithLabelValues(sessionKindClass(kind, credential)).Inc()
		} else if registered.Created {
			// Only a session the mint added can be the network's earliest
			// review: a refresh only extends a member, and the network's index
			// entry already reviews at or before it. The new member is
			// published at once, within the mint budget; only a failed
			// publication goes to the outbox, whose single row per network
			// every mint used to upsert (S1 W7). An index entry published by a
			// transaction that then rolls back only reviews early.
			due := time.UnixMilli(registered.AcceptUntil).UTC()
			publishCtx, cancelPublish := context.WithTimeout(ctx, sessionMintRedisBudget)
			publishErr := publishSessionIndexOnMint(publishCtx, credential.NetworkId, due, server.NewId().String())
			cancelPublish()
			if publishErr != nil {
				// in UTC, as next_review_time holds it
				if err = QueueSessionIndexInTx(ctx, tx, credential.NetworkId, due); err != nil {
					return "", err
				}
			}
		}
	}
	return sign(credential), nil
}

// The clock sample and the mint script, each within the mint budget. exempt
// is the provider exemption the mint sets (sessionMintExemption).
func registerSessionMint(ctx context.Context, credential *ByJwt, kind string, origin *server.Id, fenceOrigin bool, exempt string) (*registeredSession, error) {
	clockCtx, cancelClock := context.WithTimeout(ctx, sessionMintRedisBudget)
	err := verifySessionMintClock(clockCtx)
	cancelClock()
	if err != nil {
		return nil, err
	}
	registerCtx, cancelRegister := context.WithTimeout(ctx, sessionMintRedisBudget)
	defer cancelRegister()
	return registerNetworkSession(registerCtx, credential, kind, origin, fenceOrigin, server.NowUtc(), exempt)
}

// Leaves the session out of a mint that started it. Only this mint can have
// bound a client row to a fresh id; a derived lineage's binding stays, since
// the token's next refresh derives the same id.
func untagSessionMintInTx(ctx context.Context, tx server.PgTx, credential *ByJwt, start *sessionMintStart) error {
	if start.fresh && credential.ClientId != nil {
		if _, err := tx.Exec(ctx, `UPDATE network_client SET session_id=NULL,session_create_time=NULL WHERE network_id=$1 AND client_id=$2 AND session_id=$3`, credential.NetworkId, *credential.ClientId, start.sessionId); err != nil {
			return err
		}
	}
	credential.SessionId = nil
	credential.mintStart = nil
	return nil
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
