package model

// What POST /sidecar/wake reads and writes (§6.4's Bound, §6.16; D-TRUST-10, PROTO-M8-02): the verified state's
// rows of the caller's sidecar and the one it summons, which say both are admitted devices of one member; the
// summoned sidecar's current client, its newest binding generation, which the content-free core hint goes to; and
// {wake_<sidecarId>}, the hourly limit (§2.8's api-6), SET NX with a ttl of the interval, so one device of a member
// is summoned at most once an interval whoever asks.

import (
	"context"
	"fmt"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol/bridge"

	"github.com/urnetwork/server"
)

// {wake_<sidecarId>}.
func sidecarWakeKey(sidecarId server.Id) string {
	return "{wake_" + sidecarId.String() + "}"
}

// Takes the summoned sidecar's wake for the next ttl: true when no wake of it was taken in the last ttl, false when
// one was, so the caller answers 429.
func TakeSidecarWake(ctx context.Context, sidecarId server.Id, ttl time.Duration) (taken bool, returnErr error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			if err, ok := recovered.(error); ok {
				returnErr = err
			} else {
				returnErr = fmt.Errorf("sidecar wake: %v", recovered)
			}
		}
	}()
	server.Redis(ctx, func(client server.RedisClient) {
		taken, returnErr = client.SetNX(ctx, sidecarWakeKey(sidecarId), "1", ttl).Result()
	})
	return
}

// The client of sidecarId's newest binding generation, while its identity in networkId is active: where a hint to the
// sidecar goes. False for a sidecar api holds no active identity or binding of in networkId.
func GetSidecarCurrentClientId(ctx context.Context, networkId server.Id, sidecarId server.Id) (clientId server.Id, found bool) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT sidecar_identity_binding.client_id
				FROM sidecar_identity_binding
				INNER JOIN sidecar_identity ON sidecar_identity.sidecar_id = sidecar_identity_binding.sidecar_id
				WHERE
					sidecar_identity_binding.sidecar_id = $1 AND
					sidecar_identity.network_id = $2 AND
					sidecar_identity.status = $3
				ORDER BY sidecar_identity_binding.generation DESC
				LIMIT 1
			`,
			sidecarId,
			networkId,
			SidecarIdentityStatusActive,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&clientId))
				found = true
			}
		})
	})
	return
}

// The verified state's rows of sidecarIds in networkId, as this instance's verifier holds them, each a copy: a
// sidecar the state holds no row of is absent. A network whose log has no base yet holds none.
func (self *ConfigLogSequencer) SidecarRows(ctx context.Context, networkId server.Id, sidecarIds []sidecar.Id) (map[sidecar.Id]*bridge.SidecarRow, error) {
	network := self.network(networkId)
	network.stateLock.Lock()
	defer network.stateLock.Unlock()
	if err := network.loadWithLock(ctx, self, nil); err != nil {
		return nil, err
	}
	rows := map[sidecar.Id]*bridge.SidecarRow{}
	if network.log == nil {
		return rows, nil
	}
	state := network.log.State()
	for _, sidecarId := range sidecarIds {
		if row := state.SidecarRow(sidecarId); row != nil {
			rows[sidecarId] = proto.Clone(row).(*bridge.SidecarRow)
		}
	}
	return rows, nil
}
