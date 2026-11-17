package model

// Reuse of the hosted proxies a server-side caller provisioned for a network.
//
// A caller that provisions hosted proxies on a network's behalf, and keeps no
// durable record of them, creates a new proxy whenever its own handle is
// missing or expired. The MCP fetch tool is one: its sealed handle lasts six
// hours, the agent never sees the proxy's credential, and nothing removed the
// proxy afterwards. Each such proxy stayed active until the idle reap, kept a
// device on a proxy host, and counted against the network's active proxy cap
// (network_client_proxy_limit.go), where a free network's leftovers blocked
// the proxies its people create themselves. Before creating, such a caller
// looks for the newest active proxy it created for the same network and
// location, and reuses it.
//
// The lookup walks a bounded window of the network's newest active top-level
// clients (a hosted proxy is a top-level client), so a network with many
// clients costs a bounded scan. A proxy older than the window is not found,
// and the caller creates a new one.

import (
	"bytes"
	"context"
	"encoding/json"

	"github.com/urnetwork/sdk"

	"github.com/urnetwork/server"
)

type ReusableNetworkProxySettings struct {
	// The most of the network's newest active top-level clients the lookup
	// examines. Non-positive finds nothing, so the caller always creates.
	ScanLimit int
}

func DefaultReusableNetworkProxySettings() *ReusableNetworkProxySettings {
	return &ReusableNetworkProxySettings{
		// a network's own proxies are few, and a caller's are among its newest
		// clients; the window bounds the walk for a network with many clients
		ScanLimit: 2000,
	}
}

// FindReusableNetworkProxy returns the newest active hosted proxy of the network
// whose device was created with `deviceSpec` and whose initial location is
// `location`, or nil. Proxies are matched on their location's id only (a
// device, location, location group, or best available); its display fields do
// not take part.
func FindReusableNetworkProxy(
	ctx context.Context,
	networkId server.Id,
	deviceSpec string,
	location *sdk.ConnectLocation,
	settings *ReusableNetworkProxySettings,
) (proxyId *server.Id) {
	if location == nil || location.ConnectLocationId == nil || settings.ScanLimit <= 0 {
		return nil
	}
	locationIdJson, err := json.Marshal(location.ConnectLocationId)
	if err != nil {
		return nil
	}

	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT
					proxy_device_config.proxy_id,
					proxy_device_config.config_json
				FROM (
					SELECT
						network_client.client_id,
						network_client.device_id
					FROM network_client
					WHERE
						network_client.network_id = $1 AND
						network_client.active = true AND
						network_client.source_client_id IS NULL
					ORDER BY network_client.client_id DESC
					LIMIT $3
				) recent_client
				INNER JOIN device ON
					device.device_id = recent_client.device_id AND
					device.device_spec = $2
				INNER JOIN proxy_device_config ON
					proxy_device_config.client_id = recent_client.client_id
				ORDER BY recent_client.client_id DESC, proxy_device_config.proxy_id DESC
			`,
			networkId,
			deviceSpec,
			settings.ScanLimit,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var candidateProxyId server.Id
				var configJson string
				server.Raise(result.Scan(&candidateProxyId, &configJson))
				if proxyId != nil {
					// drain: the newest match is already chosen
					continue
				}
				if reusableProxyLocationMatches(configJson, locationIdJson) {
					proxyId = &candidateProxyId
				}
			}
		})
	})
	return
}

// reusableProxyLocationMatches reports whether a proxy device config's initial
// location has the location id encoded in `locationIdJson`. A config that does
// not parse, or has no initial location, never matches.
func reusableProxyLocationMatches(configJson string, locationIdJson []byte) bool {
	var proxyDeviceConfig ProxyDeviceConfig
	if err := json.Unmarshal([]byte(configJson), &proxyDeviceConfig); err != nil {
		return false
	}
	initialDeviceState := proxyDeviceConfig.InitialDeviceState
	if initialDeviceState == nil || initialDeviceState.Location == nil || initialDeviceState.Location.ConnectLocationId == nil {
		return false
	}
	candidateLocationIdJson, err := json.Marshal(initialDeviceState.Location.ConnectLocationId)
	if err != nil {
		return false
	}
	return bytes.Equal(candidateLocationIdJson, locationIdJson)
}
