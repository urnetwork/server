// What a provider declares it can serve beyond its provide keys, and the read
// that provider discovery makes of it (support inbox 7918, 7843,
// https://github.com/urnetwork/connect/issues/220).
//
// A client that requires post-quantum encryption runs every window client
// fail closed, so a provider that cannot seal a post-quantum session can never
// carry its traffic; offered one anyway, the client dials it, evaluates it and
// waits out its failure before it tries the next. A provider declares the
// sessions in each provide (`Provide.post_quantum_encryption`), and
// SetProvideWithCapabilities keeps the declaration of its latest provide that
// registered a Public or Network key in provide_capability. FindProviders2
// keeps only declaring providers for a request that requires post-quantum
// encryption. A provider that never declared, which is every provider before
// the field, is left out. The declaration only narrows discovery: the client
// still verifies every session before any of its traffic is sealed with it.
package model

import (
	"context"
	"fmt"

	"github.com/urnetwork/server/v2026"
)

// The capabilities one provide declares. The zero value declares none, as
// every provide before a capability existed does.
type ProvideCapabilities struct {
	// the provider serves the per-peer post-quantum encryption sessions a
	// client that requires post-quantum encryption opens
	PostQuantumEncryption bool
}

// The ids among clientIds whose latest provide with a Public or Network key
// declared post-quantum encryption. Each query reads at most 256 distinct ids
// and observes the caller's context. An error is the caller's: a request that
// requires the capability cannot be answered without it, and an unread
// provider is never treated as declaring it.
func readProvidePostQuantumEncryption(ctx context.Context, clientIds []server.Id) (map[server.Id]bool, error) {
	const chunkSize = 256
	uniqueClientIds := make([]server.Id, 0, len(clientIds))
	seenClientIds := make(map[server.Id]bool, len(clientIds))
	for _, clientId := range clientIds {
		if !seenClientIds[clientId] {
			seenClientIds[clientId] = true
			uniqueClientIds = append(uniqueClientIds, clientId)
		}
	}
	capableClientIds := map[server.Id]bool{}
	if len(uniqueClientIds) == 0 {
		return capableClientIds, nil
	}
	var returnErr error
	// a connection that cannot be acquired raises; it is the caller's error
	// all the same
	server.HandleError(func() {
		server.Db(ctx, func(conn server.PgConn) {
			for start := 0; start < len(uniqueClientIds); start += chunkSize {
				chunkClientIds := uniqueClientIds[start:min(start+chunkSize, len(uniqueClientIds))]
				chunkClientIdSet := make(map[server.Id]bool, len(chunkClientIds))
				for _, clientId := range chunkClientIds {
					chunkClientIdSet[clientId] = true
				}
				if err := ctx.Err(); err != nil {
					returnErr = err
					return
				}
				rows, err := conn.Query(
					ctx,
					`
				SELECT client_id
				FROM provide_capability
				WHERE
					client_id = ANY($1) AND
					post_quantum_encryption
				`,
					chunkClientIds,
				)
				if err != nil {
					returnErr = err
					return
				}
				func() {
					defer rows.Close()
					for rows.Next() {
						var clientId server.Id
						if err := rows.Scan(&clientId); err != nil {
							returnErr = err
							return
						}
						if !chunkClientIdSet[clientId] || capableClientIds[clientId] {
							returnErr = fmt.Errorf("invalid provide capability result population")
							return
						}
						capableClientIds[clientId] = true
					}
					returnErr = rows.Err()
				}()
				if returnErr != nil {
					return
				}
			}
		}, server.OptReadOnly(), server.OptNoRetry())
	}, func(err error) {
		returnErr = err
	})
	if returnErr != nil {
		return nil, returnErr
	}
	return capableClientIds, nil
}
