package model

// §5.15's sidecar_id to client lookup for §6.8's hints (plan-KEYS-hint-addressing, QD-146): each named sidecar's
// current client, the client of its newest binding generation, while its identity is active. api answers it to the
// hosts that keep Postgres off the hint path (gkm, mxedge's router) on its internal route
// (server.SidecarIdentityClientsGatePath), and taskworker's hint writer, which reads Postgres, calls it itself. A
// sidecar api holds no identity of, or holds revoked, is absent, which loses that sidecar's hint until its hourly
// resync. It reads sidecar_identity and sidecar_identity_binding alone, §2.8's api-5 rows, and writes nothing; nothing
// here logs a sidecar or a client id.

import (
	"context"
	"errors"
	"fmt"

	"github.com/urnetwork/server"
)

// The lookup cannot run in this binary: it does not link MG1, whose tables it reads.
var ErrSidecarIdentityClientsUnlinked = errors.New("sidecar identity clients: MG1 is not linked")

// A sidecar's current client: its newest binding's client and that binding's generation.
type SidecarCurrentClient struct {
	ClientId   server.Id
	Generation int64
}

// Each of sidecarIds' current client while its identity is active, by sidecar id; an id api holds no active identity
// with a binding of is absent. One query for the whole list, which the caller bounds (the route's format takes at most
// 1,000 ids). ErrSidecarIdentityClientsUnlinked without MG1; any other error is the store's.
func GetSidecarCurrentClients(ctx context.Context, sidecarIds []server.Id) (clients map[server.Id]*SidecarCurrentClient, returnErr error) {
	clients = map[server.Id]*SidecarCurrentClient{}
	if len(sidecarIds) == 0 {
		return clients, nil
	}
	if !server.SidecarMg1Linked() {
		return nil, ErrSidecarIdentityClientsUnlinked
	}
	server.HandleError(func() {
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(
				ctx,
				`
					SELECT DISTINCT ON (sidecar_identity_binding.sidecar_id)
						sidecar_identity_binding.sidecar_id,
						sidecar_identity_binding.client_id,
						sidecar_identity_binding.generation
					FROM sidecar_identity_binding
					INNER JOIN sidecar_identity ON sidecar_identity.sidecar_id = sidecar_identity_binding.sidecar_id
					WHERE
						sidecar_identity_binding.sidecar_id = ANY($1) AND
						sidecar_identity.status = $2
					ORDER BY sidecar_identity_binding.sidecar_id, sidecar_identity_binding.generation DESC
				`,
				sidecarIds,
				SidecarIdentityStatusActive,
			)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var sidecarId server.Id
					client := &SidecarCurrentClient{}
					server.Raise(result.Scan(&sidecarId, &client.ClientId, &client.Generation))
					clients[sidecarId] = client
				}
			})
		})
	}, func(err error) {
		// the store's words, which name no id
		returnErr = fmt.Errorf("sidecar identity clients: %w", err)
	})
	if returnErr != nil {
		return nil, returnErr
	}
	return clients, nil
}
