package model

import (
	"context"
	"net/netip"

	"github.com/urnetwork/server"
)

// One active address of an active extender and the client that activated it.
type NetworkExtenderOwnerAddress struct {
	ClientId server.Id
	Ip       netip.Addr
}

// GetActiveNetworkExtenderOwnerAddresses is every active extender's active
// addresses with the client that activated each. The camouflage report reads it
// once per process per cache interval, and keeps it in memory only, to tell who
// may report as an extender and which caller addresses are relays
// (controller/extender_camouflage_report.go).
func GetActiveNetworkExtenderOwnerAddresses(ctx context.Context) []*NetworkExtenderOwnerAddress {
	ownerAddresses := []*NetworkExtenderOwnerAddress{}

	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				network_extender.client_id,
				network_extender_address.ip
			FROM network_extender
			INNER JOIN network_extender_address ON
				network_extender_address.extender_id = network_extender.extender_id AND
				network_extender_address.active
			WHERE network_extender.active
			`,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				ownerAddress := &NetworkExtenderOwnerAddress{}
				server.Raise(result.Scan(
					&ownerAddress.ClientId,
					&ownerAddress.Ip,
				))
				ownerAddresses = append(ownerAddresses, ownerAddress)
			}
		})
	})

	return ownerAddresses
}
