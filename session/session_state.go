// Fleet-wide terminal horizons and Redis marker authority shared by all callers.
package session

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"github.com/urnetwork/server"
)

const SessionGrace = 60 * 24 * time.Hour
const SessionRetentionMargin = 5 * time.Minute
const ClientAncestryDepthLimit = 1024

var ErrSessionStoreUnavailable = errors.New("session store temporarily unavailable")
var ErrSessionRevoked = errors.New("session_revoked")

func SessionKey(networkId server.Id, suffix string) string {
	return fmt.Sprintf("{ns_%s}%s", networkId, suffix)
}
func SessionMarkerKey(networkId, sessionId server.Id) string {
	return SessionKey(networkId, "r:"+sessionId.String())
}

// The exclusive terminal boundary is independent of the local strict-expiry flag.
func (self *ByJwt) AcceptUntil() time.Time {
	if self.ExpiresAt == nil {
		return time.Time{}
	}
	return self.ExpiresAt.Time.Add(SessionGrace + DefaultByJwtSettings().ClockLeeway)
}

// The checks of a credential's lifetime no gate relaxes, made wherever a
// credential is accepted, so a connection admitted before the end retires too.
func validateSessionHorizon(byJwt *ByJwt, now time.Time) error {
	if byJwt.StrictExp {
		if err := validateStrictExp(byJwt, now); err != nil {
			return err
		}
	}
	if byJwt.SessionId == nil && byJwt.RootClientId == nil {
		return nil
	}
	if byJwt.ExpiresAt == nil {
		return rejectByJwtClaims(AuthRejectionMissingClaims, "tagged credential requires exp")
	}
	if !now.Before(byJwt.AcceptUntil()) {
		return rejectByJwt(AuthRejectionExpired, "terminal horizon ended")
	}
	return nil
}

// A strict_exp credential ends at its exp, with no grace (owner decision 103):
// neither reject_expired, nor AcceptUntil, nor the clock leeway extends it, and
// since M1 (D-TEAM-14 enforced) nothing in sidecar.yml does either. Every
// strict_exp token is minted with exp and its root, so one without them is
// refused too.
func validateStrictExp(byJwt *ByJwt, now time.Time) error {
	if byJwt.ExpiresAt == nil || byJwt.ClientId == nil || byJwt.RootClientId == nil {
		return rejectByJwtClaims(AuthRejectionMissingClaims, "strict_exp credential requires exp and its root")
	}
	if !now.Before(byJwt.ExpiresAt.Time) {
		return rejectByJwt(AuthRejectionExpired, "strict_exp token is expired")
	}
	return nil
}

// Retention rounds up, so sub-millisecond precision can never shorten authority.
func DeadlineMillis(deadline time.Time) int64 {
	return deadline.UnixMilli() + int64((deadline.Nanosecond()%int(time.Millisecond)+int(time.Millisecond)-1)/int(time.Millisecond))
}

func CheckSession(ctx context.Context, networkId, sessionId server.Id) (returnErr error) {
	start := time.Now()
	defer func() { observeSessionStorage("check", start, returnErr) }()
	var revoked int64
	err := server.RedisAuth(ctx, func(ctx context.Context, r server.RedisClient) error {
		var err error
		revoked, err = r.Exists(ctx, SessionMarkerKey(networkId, sessionId)).Result()
		return err
	})
	if err != nil {
		return errors.Join(ErrAuthUnavailable, ErrSessionStoreUnavailable)
	}
	if revoked != 0 {
		return ErrSessionRevoked
	}
	return nil
}

// v1 groups legacy credential lineage; it is deliberately not a historical
// sign-in identifier. The domain is followed by network/user ID bytes and an
// eight-byte big-endian UTC Unix nanosecond timestamp. auth_session_ids never
// participates in the derivation.
func LegacySessionId(byJwt *ByJwt, absentTimeId server.Id) server.Id {
	if byJwt.CreateTime.IsZero() {
		return absentTimeId
	}
	hash := sha256.New()
	hash.Write([]byte("urnetwork:legacy-session:v1\x00"))
	hash.Write(byJwt.NetworkId[:])
	hash.Write(byJwt.UserId[:])
	var nanos [8]byte
	binary.BigEndian.PutUint64(nanos[:], uint64(byJwt.CreateTime.UTC().UnixNano()))
	hash.Write(nanos[:])
	var id server.Id
	var timestamp [8]byte
	binary.BigEndian.PutUint64(timestamp[:], uint64(byJwt.CreateTime.UnixMilli()))
	copy(id[:6], timestamp[2:])
	copy(id[6:], hash.Sum(nil)[:10])
	return id
}

// Topology lookup is separate from legacy activity authorization. An inactive
// intermediate row does not add a recursive restriction to the session branch.
func ResolveClientRootInTx(ctx context.Context, conn server.PgCanQuery, networkId, clientId server.Id) (server.Id, error) {
	var root server.Id
	found := false
	rows, err := conn.Query(ctx, `WITH RECURSIVE chain AS (
	 SELECT client_id, source_client_id, network_id, 1 AS depth FROM network_client WHERE client_id=$1
	 UNION ALL SELECT c.client_id,c.source_client_id,c.network_id,p.depth+1 FROM network_client c JOIN chain p ON c.client_id=p.source_client_id WHERE p.depth < $3
	) SELECT client_id FROM chain WHERE source_client_id IS NULL AND network_id=$2 AND NOT EXISTS (SELECT 1 FROM chain WHERE network_id<>$2)`, clientId, networkId, ClientAncestryDepthLimit)
	if err != nil {
		return root, err
	}
	defer rows.Close()
	if rows.Next() {
		err = rows.Scan(&root)
		found = true
	}
	if err == nil {
		err = rows.Err()
	}
	if err != nil {
		return root, err
	}
	if !found {
		return root, errors.New("invalid client ancestry")
	}
	return root, nil
}
