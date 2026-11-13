// The Client-class token check sockets runs (plan-SOCK-client-auth, QD-040;
// §9.15, §19.4, §19.10): a by client jwt verified offline with the verify-only
// keyring, then checked against its Redis revocation record alone, never SQL,
// so a block with no Postgres answers it, a Find stays off Postgres (I-12,
// §22.5), and sockets runs through a Postgres outage until the record's
// positive fields lapse, about an hour, after which it answers unavailable.
//
// Three answers, and nothing is accepted on a missing one:
//   - valid: the token verifies, every claim holds, and the record answers
//     valid; or the record misses, nothing it holds refuses the token, and the
//     client checked valid within ValidGrace.
//   - refused, wrapping ErrClientTokenRefused: the signature or a claim fails,
//     the token is past its exp (a strict_exp one with no leeway), it names no
//     network, user, client, device or root (QD-040: no root claim, no
//     answer), or its record holds a tombstone, a deleted network, another
//     admin, or a credential cutoff after the token's creation. The engine
//     answers 401.
//   - unavailable, a *ClientTokenUnavailableError with the wait to ask for: the
//     keyring holds no key or not the token's, the record is not enabled
//     (auth.yml's revocation_record_enabled; its writers must all run first),
//     Redis fails, the record misses outside the grace, or the token is one the
//     record keeps no state for, a team member's or one descending from an API
//     key, which only the SQL state query answers.
//
// The grace. A client's positive record fields lapse 54-60 min after the SQL
// fallback projected them, and only a later fallback, api's or connect's,
// projects them again; connect's 60 s recheck does so for a connected client,
// so a valid client's fields can be missing for about a minute, which would
// outlast a 45 s lease. {sc_<client_id>}ok holds the last time this check found
// the client valid, for ValidGrace, and a miss within that span counts as valid
// without moving it. A revocation in the span shows: a removed client's or
// root's field, or a revoked session's, holds a tombstone for 10 min, a deleted
// network's mark and a rotation's cutoff stay on the record, and each refuses
// whatever else is missing.
//
// Safe for concurrent use.
package session

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/server"
)

// A Client-class token that will not authenticate. The engine answers 401.
var ErrClientTokenRefused = errors.New("client token refused")

// A Client-class check that could not answer, and how long its caller should
// wait before asking again. It says nothing about the token.
type ClientTokenUnavailableError struct {
	RetryAfter time.Duration
	// why it could not answer
	Err error
}

func (self *ClientTokenUnavailableError) Error() string {
	return fmt.Sprintf("client token check unavailable, retry after %s: %s", self.RetryAfter, self.Err)
}

func (self *ClientTokenUnavailableError) Unwrap() error {
	return self.Err
}

// The key holding a client's last valid time, in sock-5's family (§9.8): the
// client's own tag, the slot of its socket set.
func clientTokenValidKey(clientId server.Id) string {
	return fmt.Sprintf("{sc_%s}ok", clientId)
}

// What the check tunes. The defaults are QD-040's.
type ClientTokenVerifierSettings struct {
	Keyring *ByJwtVerifyKeyringSettings
	// How long after a valid check a record miss still counts as valid: 120 s
	// (QD-040), past connect's 60 s recheck, which projects the record again.
	ValidGrace time.Duration
	// How often at most this process writes one client's last valid time: 10 s,
	// so a busy caller, an edge's Finds, writes it a few times a grace rather
	// than once a call. A miss's grace shrinks by at most this. Non-positive
	// writes it at every valid check.
	GraceWriteInterval time.Duration
	// How many clients' last writes this process remembers: 65,536. It bounds
	// memory only, since a client forgotten is written again at its next valid
	// check.
	GraceWriteMemoCount int
	// The wait a record that cannot answer asks for: a miss outside the grace,
	// or a token the record keeps no state for. 5 s, a twelfth of connect's
	// 60 s recheck, which projects a connected client's record again.
	MissRetryAfter time.Duration
	// The wait a failed store, a keyring with no key or not the token's, or a
	// record not yet enabled asks for: 1 s.
	UnavailableRetryAfter time.Duration
	Now                   func() time.Time
}

// QD-040's grace and waits, on the block's mounted jwk set.
func DefaultClientTokenVerifierSettings() *ClientTokenVerifierSettings {
	return &ClientTokenVerifierSettings{
		Keyring:               DefaultByJwtVerifyKeyringSettings(),
		ValidGrace:            120 * time.Second,
		GraceWriteInterval:    10 * time.Second,
		GraceWriteMemoCount:   64 * 1024,
		MissRetryAfter:        5 * time.Second,
		UnavailableRetryAfter: time.Second,
		Now:                   server.NowUtc,
	}
}

// Verifies Client-class tokens with no Postgres. NewClientTokenVerifier makes
// one.
type ClientTokenVerifier struct {
	settings *ClientTokenVerifierSettings
	keyring  *ByJwtVerifyKeyring

	stateLock sync.Mutex
	// when this process last wrote each client's last valid time
	clientGraceWriteTimes map[server.Id]time.Time
}

// A verifier on settings' keyring, which it reads at first use.
func NewClientTokenVerifier(settings *ClientTokenVerifierSettings) *ClientTokenVerifier {
	if settings == nil {
		settings = DefaultClientTokenVerifierSettings()
	}
	return &ClientTokenVerifier{
		settings:              settings,
		keyring:               NewByJwtVerifyKeyring(settings.Keyring),
		clientGraceWriteTimes: map[server.Id]time.Time{},
	}
}

// Why every token answers unavailable now, nil when the verifier can answer:
// its keyring holds a key and the record is enabled. A block says it once at
// start; Verify still answers each token on its own.
func (self *ClientTokenVerifier) Check() error {
	if _, err := self.keyring.heldKeys(); err != nil {
		return err
	}
	if !RevocationRecordEnabled() {
		return errors.New("the revocation record is not enabled (auth.yml revocation_record_enabled)")
	}
	return nil
}

func (self *ClientTokenVerifier) unavailable(retryAfter time.Duration, err error) error {
	return &ClientTokenUnavailableError{RetryAfter: retryAfter, Err: err}
}

func clientTokenRefused(err error) error {
	return fmt.Errorf("%w: %w", ErrClientTokenRefused, err)
}

// The by client jwt jwtSigned, once it is verified and its record answers it
// valid (see the file's answers). Nothing here reads Postgres: ctx is marked so,
// and an acquisition would panic rather than query.
func (self *ClientTokenVerifier) Verify(ctx context.Context, jwtSigned string) (*ByJwt, error) {
	ctx = server.WithoutPostgres(ctx)
	if jwtSigned == "" {
		return nil, clientTokenRefused(errors.New("no token"))
	}
	byJwt, unknownKid, err := self.keyring.parse(jwtSigned)
	switch {
	case errors.Is(err, ErrByJwtVerifyKeysUnavailable):
		return nil, self.unavailable(self.settings.UnavailableRetryAfter, err)
	case err != nil && unknownKid:
		// a key this block was not given yet: its mount lags the issuer's
		return nil, self.unavailable(self.settings.UnavailableRetryAfter, fmt.Errorf("the token's kid is not in %s", ByJwtPublicJwksResourceName))
	case err != nil:
		return nil, clientTokenRefused(rejectByJwt(AuthRejectionSignature, "verify-only parse err = %v", err))
	}
	if err := validateClientTokenClaims(byJwt, self.settings.Now()); err != nil {
		return nil, clientTokenRefused(err)
	}
	if !RevocationRecordEnabled() {
		return nil, self.unavailable(self.settings.UnavailableRetryAfter, errors.New("the revocation record is not enabled"))
	}
	if byJwt.ApiKeyId != nil || byJwt.MemberId != nil {
		// the record keeps no API key and no membership (usesRevocationRecord)
		return nil, self.unavailable(self.settings.MissRetryAfter, errors.New("the record keeps no state for this credential"))
	}
	if err := self.checkRecord(ctx, byJwt); err != nil {
		return nil, err
	}
	return byJwt, nil
}

// Every claim a Client-class token carries, required and checked, with nothing
// repaired, since fixByJwt's repair reads SQL: the registered claims as
// reject_missing_expiration requires them, api's audience, the client's
// network, user, device and root (QD-040), and exp enforced as reject_expired
// enforces it, a strict_exp token's with no leeway (validateSessionHorizon).
// sockets has no legacy client, so neither gate's tolerance applies.
func validateClientTokenClaims(byJwt *ByJwt, now time.Time) error {
	clockLeeway := DefaultByJwtSettings().ClockLeeway
	if byJwt.Issuer != ByJwtIssuer ||
		!slices.Contains(byJwt.Audience, ByJwtAudienceApi) ||
		byJwt.Subject == "" || byJwt.Subject != byJwt.UserId.String() ||
		byJwt.ExpiresAt == nil || byJwt.IssuedAt == nil || byJwt.NotBefore == nil ||
		byJwt.ID == "" || byJwt.CreateTime.IsZero() {
		return rejectByJwtClaims(AuthRejectionMissingClaims, "incomplete registered claims")
	}
	if byJwt.NetworkId == (server.Id{}) || byJwt.UserId == (server.Id{}) {
		return rejectByJwtClaims(AuthRejectionMissingClaims, "no network or user")
	}
	if byJwt.ClientId == nil || byJwt.DeviceId == nil || byJwt.RootClientId == nil {
		return rejectByJwtClaims(AuthRejectionClientRequired, "no client, device or root")
	}
	for _, claimId := range []*server.Id{byJwt.ClientId, byJwt.DeviceId, byJwt.RootClientId, byJwt.SidecarId} {
		if claimId != nil && *claimId == (server.Id{}) {
			return rejectByJwtClaims(AuthRejectionInvalidClaims, "a zero id in a claim")
		}
	}
	if !now.Before(byJwt.ExpiresAt.Time.Add(clockLeeway)) {
		return rejectByJwt(AuthRejectionExpired, "token is expired")
	}
	if now.Before(byJwt.NotBefore.Time.Add(-clockLeeway)) {
		return rejectByJwt(AuthRejectionNotYetValid, "token is not valid yet")
	}
	if now.Before(byJwt.IssuedAt.Time.Add(-clockLeeway)) {
		return rejectByJwt(AuthRejectionUsedBeforeIssued, "token used before issued")
	}
	return validateSessionHorizon(byJwt, now)
}

// The record's reply and the client's last valid time, read in one round trip.
// A test stands in for an unavailable store.
var readClientTokenRecord = func(ctx context.Context, recordKey string, fields []string, validKey string) (values []any, validValue string, validFound bool, returnErr error) {
	returnErr = server.RedisAuth(ctx, func(ctx context.Context, r server.RedisClient) error {
		var record *redis.SliceCmd
		var valid *redis.StringCmd
		_, err := r.Pipelined(ctx, func(pipe redis.Pipeliner) error {
			record = pipe.HMGet(ctx, recordKey, fields...)
			valid = pipe.Get(ctx, validKey)
			return nil
		})
		if err != nil && !errors.Is(err, server.RedisNil) {
			return err
		}
		if values, err = record.Result(); err != nil {
			return err
		}
		validValue, err = valid.Result()
		switch {
		case errors.Is(err, server.RedisNil):
			return nil
		case err != nil:
			return err
		}
		validFound = true
		return nil
	})
	return
}

// The write of the client's last valid time, which expires after the grace.
var writeClientTokenValid = func(ctx context.Context, validKey string, now time.Time, grace time.Duration) error {
	return server.RedisAuth(ctx, func(ctx context.Context, r server.RedisClient) error {
		return r.Set(ctx, validKey, strconv.FormatInt(now.UnixMilli(), 10), grace).Err()
	})
}

// Fails every Client-class record read with err until the returned function
// runs, as a lost Redis would.
func Testing_FailClientTokenRecordReads(err error) func() {
	read := readClientTokenRecord
	readClientTokenRecord = func(context.Context, string, []string, string) ([]any, string, bool, error) {
		return nil, "", false, err
	}
	return func() { readClientTokenRecord = read }
}

// The record's verdict on byJwt, with the grace on a miss.
func (self *ClientTokenVerifier) checkRecord(ctx context.Context, byJwt *ByJwt) error {
	lineage, _ := revocationRecordLineage(byJwt)
	query := newRevocationRecordQuery(byJwt, lineage, nil)
	validKey := clientTokenValidKey(*byJwt.ClientId)
	values, validValue, validFound, err := readClientTokenRecord(ctx, query.key, query.fields, validKey)
	if err != nil {
		observeRevocationRecordCheck(ctx, byJwt, "record_unavailable")
		return self.unavailable(self.settings.UnavailableRetryAfter, errors.Join(ErrAuthUnavailable, ErrSessionStoreUnavailable, err))
	}
	now := self.settings.Now()
	verdict, credentialChangeTime := query.verdict(byJwt, values)
	if verdict == revocationMiss {
		// what the record does hold refuses whatever it lacks
		verdict, credentialChangeTime = query.presentRefusal(byJwt, values)
	}
	switch verdict {
	case revocationValid:
		observeRevocationRecordCheck(ctx, byJwt, "record_valid")
		self.noteValid(ctx, *byJwt.ClientId, validKey, now)
		return nil
	case revocationSessionRevoked:
		observeRevocationRecordCheck(ctx, byJwt, "record_refused")
		return clientTokenRefused(ErrSessionRevoked)
	case revocationNoActiveRow:
		observeRevocationRecordCheck(ctx, byJwt, "record_refused")
		return clientTokenRefused(rejectNoActiveRow(byJwt))
	case revocationRotated:
		observeRevocationRecordCheck(ctx, byJwt, "record_refused")
		return clientTokenRefused(rejectRotated(byJwt, credentialChangeTime))
	}
	if validFound {
		if validMillis, err := strconv.ParseInt(validValue, 10, 64); err == nil {
			if now.Before(time.UnixMilli(validMillis).Add(self.settings.ValidGrace)) {
				// valid within the grace, which does not move it
				observeRevocationRecordCheck(ctx, byJwt, "record_grace")
				return nil
			}
		}
	}
	observeRevocationRecordCheck(ctx, byJwt, "record_miss_unavailable")
	return self.unavailable(self.settings.MissRetryAfter, errors.New("the revocation record cannot answer"))
}

// Writes the client's last valid time, unless this process wrote it less than
// GraceWriteInterval ago. A write that fails leaves the answer valid; only a
// later miss's grace is shorter.
func (self *ClientTokenVerifier) noteValid(ctx context.Context, clientId server.Id, validKey string, now time.Time) {
	due := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if writeTime, ok := self.clientGraceWriteTimes[clientId]; ok && now.Before(writeTime.Add(self.settings.GraceWriteInterval)) {
			return false
		}
		return true
	}()
	if !due {
		return
	}
	err := writeClientTokenValid(ctx, validKey, now, self.settings.ValidGrace)
	observeRevocationRecordWrite("client_valid", err)
	if err != nil {
		return
	}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.settings.GraceWriteMemoCount <= len(self.clientGraceWriteTimes) {
			for memoClientId, writeTime := range self.clientGraceWriteTimes {
				if !now.Before(writeTime.Add(self.settings.GraceWriteInterval)) {
					delete(self.clientGraceWriteTimes, memoClientId)
				}
			}
			if self.settings.GraceWriteMemoCount <= len(self.clientGraceWriteTimes) {
				clear(self.clientGraceWriteTimes)
			}
		}
		self.clientGraceWriteTimes[clientId] = now
	}()
}

// The refusal the record's present fields hold when the verdict is a miss: a
// revoked session, a deleted network, another admin, a removed client or root,
// a client on another device, or a credential cutoff after the token's
// creation. Any one refuses, whatever else the record lacks, so a lapsed
// positive field never lets the grace accept what the record refuses.
// revocationMiss when none refuses.
func (self *revocationRecordQuery) presentRefusal(byJwt *ByJwt, values []any) (revocationVerdict, time.Time) {
	value := func(index int) (string, bool) {
		if index < 0 || len(values) <= index || values[index] == nil {
			return "", false
		}
		text, ok := values[index].(string)
		return text, ok
	}
	if session, ok := value(self.sessionIndex); ok && session == revocationTombstone {
		return revocationSessionRevoked, time.Time{}
	}
	if _, ok := value(2); ok {
		return revocationNoActiveRow, time.Time{}
	}
	if user, ok := value(0); ok && user != byJwt.UserId.String() {
		return revocationNoActiveRow, time.Time{}
	}
	if client, ok := value(self.clientIndex); ok {
		if client == revocationTombstone {
			return revocationNoActiveRow, time.Time{}
		}
		if parts := strings.Fields(client); len(parts) == 3 && parts[0] == revocationActive && byJwt.DeviceId != nil && parts[1] != byJwt.DeviceId.String() {
			return revocationNoActiveRow, time.Time{}
		}
	}
	if root, ok := value(self.rootIndex); ok && root == revocationTombstone {
		return revocationNoActiveRow, time.Time{}
	}
	if credential, ok := value(self.credentialIdx); ok {
		if credentialMicros, err := strconv.ParseInt(credential, 10, 64); err == nil {
			credentialChangeTime := time.UnixMicro(credentialMicros).UTC()
			if byJwt.CreateTime.Before(credentialChangeTime) {
				return revocationRotated, credentialChangeTime
			}
		}
	}
	return revocationMiss, time.Time{}
}
