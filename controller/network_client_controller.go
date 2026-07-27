package controller

import (
	"context"
	"net/netip"
	// "strings"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

func ConnectNetworkClient(
	ctx context.Context,
	clientId server.Id,
	clientAddress string,
	handlerId server.Id,
	retryLocationTimeout time.Duration,
) (connectionId server.Id, clientAddressHash [32]byte, err error) {
	var clientIp string
	connectionId, clientIp, _, clientAddressHash, err = model.ConnectNetworkClient(ctx, clientId, clientAddress, handlerId)
	if err != nil {
		return
	}

	locationErr := SetConnectionLocation(ctx, connectionId, clientIp)
	if locationErr != nil && 0 < retryLocationTimeout {
		// keep the client ip in memory and do not persist to task, etc
		// the retry remains active as long as the context (which should be the connection context)
		go server.HandleError(func() {
			for {
				select {
				case <-ctx.Done():
					return
				case <-time.After(retryLocationTimeout):
				}

				locationErr := SetConnectionLocation(ctx, connectionId, clientIp)
				if locationErr == nil {
					return
				}
			}
		})
	}

	return
}

// FIXME store the result in a db by client address hash
// FIXME the client ip should be in memory only, never persisted
// FIXME consider using dbip+a latency test for quality metrics

// freshProviderEgressLocationForConnection is
// model.GetFreshProviderEgressLocationForConnection made non-fatal: on any
// failure it logs and returns nil, which the caller reads as "no probed
// location" and falls through to the mmdb lookup.
//
// See the block comment in SetConnectionLocation for why nothing here may be
// allowed to panic out.
func freshProviderEgressLocationForConnection(
	ctx context.Context,
	connectionId server.Id,
) *model.ProviderEgressLocation {
	return server.HandleError1(
		func() *model.ProviderEgressLocation {
			return model.GetFreshProviderEgressLocationForConnection(
				ctx,
				connectionId,
				model.ProviderEgressLocationMaxAge,
			)
		},
		func(err error) *model.ProviderEgressLocation {
			glog.Infof(
				"[ncc][%s]probed egress location lookup failed, using mmdb. err = %s\n",
				connectionId,
				err,
			)
			return nil
		},
	)
}

func SetConnectionLocation(
	ctx context.Context,
	connectionId server.Id,
	clientIp string,
) error {
	// a provider probed through its own egress is located from that probe, not
	// from a lookup on its control-connection ip: the egress is where user
	// traffic actually exits, and an operator-run prober learns it by routing
	// geolocation lookups through the provider itself and cross-checking them
	// across several sources, then submits the result here. When a fresh
	// probed entry exists we prefer it over the built-in mmdb lookup on the
	// control ip. GetFreshProviderEgressLocationForConnection is
	// a single query joining network_client_connection to
	// provider_egress_location: this runs for every connection (provider or
	// not) on the connect-announce path and inside a retry loop, so it must
	// not cost the two round trips (client lookup, then egress lookup) the
	// naive version would.
	//
	// The lookup is non-fatal by construction. model.Db raises non-transient,
	// non-connection postgres errors as a panic (see isTransientError /
	// isConnectionError in db.go), and this call sits on
	// ConnectNetworkClient's path *before* connect's disconnect-cleanup defer
	// is registered (connect/transport_announce.go) -- so an escaping panic
	// does not just fail the location lookup, it tears the connection down and
	// leaves its network_client_connection row orphaned as connected = true.
	//
	// The concrete hazard is deploy ordering. provider_egress_location is a
	// new table in this change; roll the binary before running
	// `bringyourctl db migrate` and every announce hits undefined_table
	// (42P01), which is neither transient nor a connection error, and every
	// connection in that window orphans. That exact failure mode has already
	// cost this project ~30k orphaned rows once, and deploy ordering is not
	// something a test suite catches.
	//
	// So: swallow ANY failure here and fall through to the mmdb path.
	// Deliberately not narrowed to transient errors -- the whole point is that
	// an unmigrated or otherwise unhappy database must not be able to break
	// connections. The probed location is an optimisation over mmdb, never a
	// requirement, so mmdb is the correct answer whenever it is unavailable.
	if egress := freshProviderEgressLocationForConnection(
		ctx,
		connectionId,
	); egress != nil {
		scores := &model.ConnectionLocationScores{}
		if egress.Hosting {
			scores.NetTypeHosting = 1
		}
		if egress.Proxy {
			scores.NetTypePrivacy = 1
		}
		// egress.Mobile deliberately does NOT feed NetTypeVirtual: unlike
		// Hosting/Proxy, Mobile has no mmdb-path equivalent (IpInfo has no
		// Mobile concept; NetTypeVirtual is set from the ipinfo schema's
		// is_satellite field only, see GetLocationForIp, and never from
		// DB-IP). Deriving NetTypeVirtual from Mobile here would penalize a
		// probed mobile provider's ranking with no equivalent penalty for an
		// otherwise-identical unprobed one -- the opposite of the parity
		// this feature is meant to preserve (see arinForeignScore's doc for
		// the same parity reasoning applied to net_type_foreign). Mobile
		// stays on the model/wire contract as metadata; it just does not
		// feed the ranking score.
		// keep the ARIN org-vs-country foreign check on the probed path too,
		// so a probed provider is ranked on equal terms with an equivalent
		// unprobed one (net_type_foreign feeds the ranking columns). Compute
		// it exactly as the mmdb path does in GetLocationForIp: the ARIN org
		// country of the control ip against the mmdb country of that SAME
		// control ip -- not the probed country, which is a different
		// question (whether probing changed the answer) and must not be
		// silently folded into this ranking penalty. Any lookup failure
		// just leaves NetTypeForeign at 0; it must never fail or panic this
		// path.
		if addr, err := netip.ParseAddr(clientIp); err == nil {
			if ipInfo, err := server.GetIpInfo(addr); err == nil {
				scores.NetTypeForeign = arinForeignScore(addr, ipInfo.CountryCode)
			}
		}
		err := model.SetConnectionLocation(ctx, connectionId, egress.LocationId, scores)
		if err == nil {
			return nil
		}
		// fall through to the mmdb path on a storage error
		glog.Infof("[ncc][%s]could not set probed egress location. err = %s\n", connectionId, err)
	}

	location, connectionLocationScores, err := GetLocationForIp(ctx, clientIp)
	if err != nil {
		// server.Logger().Printf("Get ip for location error: %s", err)
		glog.Infof("[ncc][%s]could not find client location. err = %s\n", connectionId, err)
		return err
	}

	model.CreateLocation(ctx, location)
	err = model.SetConnectionLocation(ctx, connectionId, location.LocationId, connectionLocationScores)
	if err != nil {
		// server.Logger().Printf("Get ip for location error: %s", err)
		glog.Infof("[ncc][%s]could set connection location. err = %s\n", connectionId, err)
		return err
	}
	return nil
}

/*
func SetMissingConnectionLocations(ctx context.Context, minTime time.Time) {
	connectionIpStrs := map[server.Id]string{}

	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT
				    network_client_connection.connection_id,
				    network_client_connection.client_address
				FROM network_client_connection

				LEFT JOIN network_client_location ON network_client_location.connection_id = network_client_connection.connection_id

				WHERE
					network_client_connection.connect_time < $1 AND
					network_client_connection.connected AND
					network_client_location.connection_id IS NULL
			`,
			minTime,
		)

		server.WithPgResult(result, err, func() {
			for result.Next() {
				var connectionId server.Id
				var clientAddress string
				server.Raise(result.Scan(
					&connectionId,
					&clientAddress,
				))
				host, _, err := server.ParseClientAddress(clientAddress)
				if err == nil {
					connectionIpStrs[connectionId] = host
				} else {
					glog.Infof("[ncc][%s]Could not parse client address. Skipping.\n", connectionId)
				}
			}
		})
	})

	for connectionId, ipStr := range connectionIpStrs {
		SetConnectionLocation(ctx, connectionId, ipStr)
	}
}
*/
