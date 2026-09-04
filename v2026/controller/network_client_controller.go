package controller

import (
	"context"
	"net/netip"
	"strings"
	"time"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// AuthNetworkClient keeps ordinary client creation in the model while owning
// the deployment-configured post-allocation verify feed. The model cannot
// resolve verify.yml without coupling protocol configuration into generic
// client persistence.
func AuthNetworkClient(
	authClient *model.AuthNetworkClientArgs,
	clientSession *session.ClientSession,
) (*model.AuthNetworkClientResult, error) {
	var verifySettings *model.VerifySettings
	if StEnabled() {
		// Validate the enabled subsystem's required vault/config before the
		// model commits a proxy allocation. A missing verify.yml must not turn
		// into a post-commit 500 with an ownerless allocation.
		verifySettings = VerifySettings()
	}
	result, err := model.AuthNetworkClient(authClient, clientSession)
	if err != nil || result == nil || verifySettings == nil || result.ClientId == nil || result.ProxyConfigResult == nil || result.ProxyConfigResult.WgConfig == nil {
		return result, err
	}
	feedAuthNetworkClientVerifyEgress(clientSession.Ctx, result, verifySettings)
	return result, nil
}

func feedAuthNetworkClientVerifyEgress(
	ctx context.Context,
	result *model.AuthNetworkClientResult,
	settings *model.VerifySettings,
) {
	if result == nil || result.ClientId == nil || result.ProxyConfigResult == nil || result.ProxyConfigResult.WgConfig == nil || settings == nil {
		return
	}
	model.FeedVerifyEgress(
		ctx,
		*result.ClientId,
		result.ProxyConfigResult.WgConfig.ClientIpv4,
		settings,
	)
}

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
	// the mmdb lookup on the control ip. This is resolved up front even when
	// the probed path is about to win, because the probed path has to know how
	// precise the mmdb answer is before it can decide whether replacing it is
	// an improvement (see probedLocationPreferred), and because it costs no db
	// round trip -- it is an in-process maxminddb read plus an ARIN lookup.
	// `err` is deliberately not returned yet: a failed mmdb lookup is not a
	// reason to discard a perfectly good probed location.
	location, connectionLocationScores, err := GetLocationForIp(ctx, clientIp)

	// a provider probed through its own egress is located from that probe, not
	// from a lookup on its control-connection ip: the egress is where user
	// traffic actually exits, and an operator-run prober learns it by routing
	// geolocation lookups through the provider itself and cross-checking them
	// across several sources, then submits the result here. When a fresh
	// probed entry exists we prefer it over the built-in mmdb lookup on the
	// control ip -- subject to probedLocationPreferred, which is what stops the
	// probe making a provider *less* locatable than it was.
	// GetFreshProviderEgressLocationForConnection is
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
	); egress != nil && probedLocationPreferred(egress, location) {
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
		// unprobed one (net_type_foreign feeds the ranking columns). It is
		// computed exactly as the mmdb path computes it in GetLocationForIp:
		// the ARIN org country of the control ip against the mmdb country of
		// that SAME control ip -- not the probed country, which is a different
		// question (whether probing changed the answer) and must not be
		// silently folded into this ranking penalty. It is recomputed here
		// rather than lifted off connectionLocationScores because that struct
		// is nil whenever GetLocationForIp failed, including the case where
		// mmdb resolved the ip fine but GuessLocationType could not classify
		// the result -- the foreign check is still meaningful there. Any
		// lookup failure just leaves NetTypeForeign at 0; it must never fail
		// or panic this path.
		if addr, err := netip.ParseAddr(clientIp); err == nil {
			if ipInfo, err := server.GetIpInfo(addr); err == nil {
				scores.NetTypeForeign = arinForeignScore(addr, ipInfo.CountryCode)
			}
		}
		setErr := model.SetConnectionLocation(ctx, connectionId, egress.LocationId, scores)
		if setErr == nil {
			return nil
		}
		// fall through to the mmdb path on a storage error
		glog.Infof("[ncc][%s]could not set probed egress location. err = %s\n", connectionId, setErr)
	}

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

// probedLocationPreferred decides whether the probed egress location should be
// written for this connection instead of the mmdb location resolved from the
// control ip. mmdbLocation is nil when the mmdb lookup failed.
//
// Read this before changing it: the naive rule -- "a probe is better evidence
// than mmdb, so the probe always wins" -- is wrong, and produced a live
// regression. The probed location is not always as *precise* as the mmdb one.
// SubmitProviderEgressLocation only stores a city when the probed city matches
// a location row that already exists; anything else is stored at country
// granularity, deliberately, so that a probe can never mint new city rows in
// the shared `location` table. Cities are not seeded either -- AddDefaultLocations
// runs with cityLimit = 0 -- so the pool a probed city can match against is only
// the rows organic traffic happened to create, and a country-granular fallback
// is the common case, not a rare one.
//
// Letting that country row overwrite an mmdb *city* row would drop the provider
// out of every city filter in FindProviders2 and GetProviderLocations. Being
// probed would make a provider less discoverable than never having been probed
// at all -- a penalty for participating.
//
// So the rule is: a probe may CORRECT the location, but never COARSEN it.
//
//   - The probe stored a city (CityConfident): it is at least as precise as
//     anything mmdb has, and it is better evidence. It wins.
//   - No usable mmdb answer: the probe is the only evidence there is. It wins.
//   - The mmdb answer is itself country-granular: nothing to lose. The probe
//     wins, and this is the case country-level correction exists for.
//   - The mmdb answer is city- or region-granular in a DIFFERENT country: the
//     mmdb row is not more precise, it is precisely wrong, and its city is a
//     city in the wrong country. The probe wins. This is the other half of
//     country-level correction and the reason this is not just "keep whichever
//     is finer".
//   - The mmdb answer is city- or region-granular in the SAME country the probe
//     reports: the probe agrees with mmdb and adds nothing except a loss of
//     granularity. mmdb wins.
//
// CityConfident is used as the probed row's granularity because the schema
// invariant is that provider_egress_location.location_id is a city row exactly
// when city_confident is set (see the provider_egress_location migration and
// SubmitProviderEgressLocation). Reading it off the flag keeps this on the hot
// connect-announce path without a second query for the location row's type.
func probedLocationPreferred(
	egress *model.ProviderEgressLocation,
	mmdbLocation *model.Location,
) bool {
	if egress.CityConfident {
		return true
	}
	if mmdbLocation == nil {
		return true
	}
	if mmdbLocation.LocationType != model.LocationTypeCity &&
		mmdbLocation.LocationType != model.LocationTypeRegion {
		return true
	}
	// both country codes are stored lowercased -- SubmitProviderEgressLocation
	// lowercases the probed one and GetLocationForIp takes mmdb's, which the
	// location table also stores lowercased -- but fold anyway rather than let
	// a casing difference read as a country disagreement and silently coarsen.
	return !strings.EqualFold(egress.CountryCode, mmdbLocation.CountryCode)
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
