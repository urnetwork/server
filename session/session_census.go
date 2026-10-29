// Read-only views of a network's session state, for an operator census such
// as bringyourctl network sign-out-all. Neither read changes the store.
package session

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/server"
)

// A live tracked session and the end of its acceptance horizon.
type LiveNetworkSession struct {
	SessionId   server.Id
	AcceptUntil time.Time
}

// LiveNetworkSessions lists the network's live tracked sessions, the members of
// its inventory whose horizon has not ended at now, in session id order. It is
// one ZRANGEBYSCORE: unlike the inventory script it neither prunes ended
// members, initializes the revision nor extends a key, so a dry run can read
// it without writing.
func LiveNetworkSessions(ctx context.Context, networkId server.Id, now time.Time) ([]LiveNetworkSession, error) {
	var members []redis.Z
	err := server.RedisAuth(ctx, func(ctx context.Context, r server.RedisClient) error {
		var err error
		members, err = r.ZRangeByScoreWithScores(ctx, SessionKey(networkId, "z"), &redis.ZRangeBy{
			Min: "(" + strconv.FormatInt(now.UnixMilli(), 10),
			Max: "+inf",
		}).Result()
		return err
	})
	if err != nil {
		return nil, errors.Join(ErrAuthUnavailable, ErrSessionStoreUnavailable)
	}
	sessions := make([]LiveNetworkSession, 0, len(members))
	for _, member := range members {
		text, ok := member.Member.(string)
		if !ok {
			return nil, ErrSessionStoreUnavailable
		}
		sessionId, err := server.ParseId(text)
		if err != nil {
			return nil, ErrSessionStoreUnavailable
		}
		sessions = append(sessions, LiveNetworkSession{
			SessionId:   sessionId,
			AcceptUntil: time.UnixMilli(int64(member.Score)).UTC(),
		})
	}
	slices.SortFunc(sessions, func(a LiveNetworkSession, b LiveNetworkSession) int {
		return a.SessionId.Cmp(b.SessionId)
	})
	return sessions, nil
}

// A retirement JournalSessionRetirementInTx journaled, as the journal holds it.
type JournaledRetirement struct {
	Action string
	Status string
	// the retirement's cutoff: credentials created before it are retired
	Cutoff time.Time
}

// ReadJournaledRetirement reads a journaled retirement of the network, or nil
// when the journal holds no such operation: none was journaled, or its
// completed row has expired.
func ReadJournaledRetirement(ctx context.Context, query server.PgCanQuery, networkId server.Id, operationId server.Id) (*JournaledRetirement, error) {
	rows, err := query.Query(ctx, `SELECT action,status,credential_epoch FROM network_session_operation WHERE network_id=$1 AND operation_id=$2`, networkId, operationId)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, rows.Err()
	}
	retirement := &JournaledRetirement{}
	if err := rows.Scan(&retirement.Action, &retirement.Status, &retirement.Cutoff); err != nil {
		return nil, err
	}
	return retirement, rows.Err()
}
