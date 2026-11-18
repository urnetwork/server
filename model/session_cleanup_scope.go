package model

import (
	"context"

	"github.com/urnetwork/server"
)

// A session revocation's cleanup deactivates the clients bound to its target
// sessions, and the deactivation cascades to their active descendants
// (deactivateLockedNetworkClientsCountedInTx). In that cascade, a descendant
// explicitly bound to a session outside the targets is not the revoked
// session's to end: a network credential of another session re-authenticated
// it. It stays active, with its own descendants (REVOKE-REVIEW R3; the
// cascade of 2026-11-05 passed over the binding).
type sessionCleanupScopeKey struct{}

func withSessionCleanupScope(ctx context.Context, targetSessionIds []server.Id) context.Context {
	return context.WithValue(ctx, sessionCleanupScopeKey{}, targetSessionIds)
}

func sessionCleanupScope(ctx context.Context) ([]server.Id, bool) {
	targetSessionIds, ok := ctx.Value(sessionCleanupScopeKey{}).([]server.Id)
	return targetSessionIds, ok
}

// The locked clients the cleanup of targetSessionIds may end: those bound to
// no session or to one of the targets.
func sessionCleanupDescendantsInTx(ctx context.Context, tx server.PgTx, clientIds []server.Id, networkId server.Id, targetSessionIds []server.Id) ([]server.Id, error) {
	if len(clientIds) == 0 {
		return clientIds, nil
	}
	result, err := tx.Query(
		ctx,
		`
			SELECT client_id
			FROM network_client
			WHERE
				client_id = ANY($1) AND
				network_id = $2 AND
				(session_id IS NULL OR session_id = ANY($3))
			ORDER BY client_id
		`,
		clientIds,
		networkId,
		targetSessionIds,
	)
	if err != nil {
		return nil, err
	}
	defer result.Close()
	ended := []server.Id{}
	for result.Next() {
		var clientId server.Id
		if err := result.Scan(&clientId); err != nil {
			return nil, err
		}
		ended = append(ended, clientId)
	}
	return ended, result.Err()
}
