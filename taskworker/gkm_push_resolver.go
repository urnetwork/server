package taskworker

// gkm's reconciler's sidecar_id resolver in taskworker (plan-KEYS-hint-addressing, QD-146): taskworker reads Postgres,
// so its hint writer turns the sidecar ids gkm's pending hints name into clients straight from
// sidecar_identity_binding (model.GetSidecarCurrentClients), each sidecar's newest binding's client while its identity
// is active, in queries of at most the internal route's SidecarIdMaxCount ids. It keeps nothing between calls: a
// reconciler pass hints few sidecars, and the query is one indexed read a batch. An unknown or revoked sidecar is
// absent, which loses its hint, and a failed read is an error the hint writer counts and passes over; the clients'
// hourly resync covers both (§6.8). Nothing here logs a sidecar or a client id. Safe for concurrent use.

import (
	"context"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar/pushservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// See the file header.
type gkmPushResolver struct {
	format *server.SidecarIdentityClientsGateFormatSettings
	lookup func(ctx context.Context, sidecarIds []server.Id) (map[server.Id]*model.SidecarCurrentClient, error)
}

var _ pushservice.PushClientResolver = (*gkmPushResolver)(nil)

// The resolver on model's lookup.
func newGkmPushResolver() *gkmPushResolver {
	return &gkmPushResolver{
		format: server.DefaultSidecarIdentityClientsGateFormatSettings(),
		lookup: model.GetSidecarCurrentClients,
	}
}

// pushservice.PushClientResolver's.
func (self *gkmPushResolver) PushClientIds(ctx context.Context, sidecarIds []connect.Id) (map[connect.Id]connect.Id, error) {
	clientIds := map[connect.Id]connect.Id{}
	batchCount := max(1, self.format.SidecarIdMaxCount)
	for first := 0; first < len(sidecarIds); first += batchCount {
		batch := sidecarIds[first:min(first+batchCount, len(sidecarIds))]
		serverIds := make([]server.Id, len(batch))
		for index, sidecarId := range batch {
			serverIds[index] = server.Id(sidecarId)
		}
		clients, err := self.lookup(ctx, serverIds)
		if err != nil {
			return nil, err
		}
		for sidecarId, client := range clients {
			clientIds[connect.Id(sidecarId)] = connect.Id(client.ClientId)
		}
	}
	return clientIds, nil
}
