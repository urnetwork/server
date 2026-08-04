package mcp

// Egress proxy acquisition for the fetch tool.
//
// A fetch either reuses the proxy the caller threaded back as a signed proxy
// id, or provisions a new one for the requested location. Provisioning goes
// through `model.AuthNetworkClient` -- the same path the /network/auth-client
// route uses -- so the plan's concurrent-client limit, the pro feature gates,
// and the upgrade signal that drives x402 all behave identically here.
//
// The signed proxy id is a bearer credential for the proxy (it is both the
// https proxy hostname label and the Proxy-Authorization token), and by design
// it is not ip-locked, matching the default for proxies created through the
// api. It is handed to the caller so follow-on loads reuse the same egress.
//
// Reuse guarantees the same LOCATION, not the same exit ip: the underlying
// provider selection rebalances, so callers must not depend on ip stability.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/urnetwork/sdk"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// x402 resource identifier quoted in payment terms for this tool
const fetchX402Resource = "mcp:fetch"

var errProxyGone = errors.New("the proxy for this signed_proxy_id no longer exists")

// Acquired egress, either reused or freshly provisioned.
type acquiredProxy struct {
	signedProxyId string
	proxyClient   *model.ProxyClient
	// human readable egress location, echoed back so the caller can see where
	// the request actually left from
	location string
	created  bool
}

// Raised when the network is at its plan's client limit. When x402 is enabled
// the terms are quoted so the caller can pay and retry in one round trip.
type upgradeRequired struct {
	message string
	terms   *controller.X402PaymentRequired
}

// Settles an x402 payment presented on a fetch call, so the retry that carries
// it finds the network already upgraded. Mirrors the inline settle the
// /network/auth-client route performs on the X-PAYMENT header.
func settleFetchPayment(clientSession *session.ClientSession, payment string) error {
	if !controller.X402Enabled() {
		return fmt.Errorf("payments are not enabled")
	}

	_, err := controller.X402Purchase(
		clientSession.Ctx,
		clientSession.ByJwt.NetworkId,
		payment,
		&controller.X402PurchaseArgs{SkuId: controller.X402SkuProMonth},
	)
	return err
}

// Reuses the proxy named by signedProxyId, or provisions one for
// locationQuery. A signed proxy id whose proxy has since been removed falls
// back to provisioning when a location is available, so a caller threading a
// stale handle recovers without a special case.
func acquireProxy(
	clientSession *session.ClientSession,
	signedProxyId string,
	locationQuery string,
) (*acquiredProxy, *upgradeRequired, error) {
	if signedProxyId != "" {
		proxy, err := reuseProxy(clientSession, signedProxyId)
		if err == nil {
			return proxy, nil, nil
		}
		if !errors.Is(err, errProxyGone) {
			return nil, nil, err
		}
		// the proxy expired or was removed. Without a location there is
		// nothing to re-establish from, so tell the caller what to do
		if locationQuery == "" {
			return nil, nil, fmt.Errorf(
				"%w; call again with a location to establish a new one",
				errProxyGone,
			)
		}
	}

	return createProxy(clientSession, locationQuery)
}

func reuseProxy(clientSession *session.ClientSession, signedProxyId string) (*acquiredProxy, error) {
	proxyId, err := model.ParseSignedProxyId(signedProxyId)
	if err != nil {
		return nil, fmt.Errorf("invalid signed_proxy_id")
	}

	proxyDeviceConfig := model.GetProxyDeviceConfig(clientSession.Ctx, proxyId)
	if proxyDeviceConfig == nil {
		return nil, errProxyGone
	}

	proxyClient, err := model.GetProxyClient(clientSession.Ctx, proxyId)
	if err != nil || proxyClient == nil {
		return nil, errProxyGone
	}

	location := ""
	if proxyDeviceConfig.InitialDeviceState != nil && proxyDeviceConfig.InitialDeviceState.Location != nil {
		location = proxyDeviceConfig.InitialDeviceState.Location.Name
	}

	return &acquiredProxy{
		signedProxyId: signedProxyId,
		proxyClient:   proxyClient,
		location:      location,
		created:       false,
	}, nil
}

func createProxy(
	clientSession *session.ClientSession,
	locationQuery string,
) (*acquiredProxy, *upgradeRequired, error) {
	connectLocation, locationName, err := resolveLocation(clientSession, locationQuery)
	if err != nil {
		return nil, nil, err
	}

	// no ip lock: the proxy is used from this service, and locking to the
	// calling agent's ip would refuse our own egress connection
	authClientResult, err := model.AuthNetworkClient(
		&model.AuthNetworkClientArgs{
			Description: "mcp fetch",
			DeviceSpec:  "mcp",
			ProxyConfig: &model.ProxyConfig{
				InitialDeviceState: &model.ExtendedProxyDeviceState{
					ProxyDeviceState: model.ProxyDeviceState{
						Location: connectLocation,
					},
				},
			},
		},
		clientSession,
	)
	if err != nil {
		return nil, nil, err
	}

	if authClientResult.Error != nil {
		if authClientResult.Error.UpgradeRequired {
			upgrade := &upgradeRequired{
				message: authClientResult.Error.Message,
			}
			if controller.X402Enabled() {
				terms, err := controller.X402PaymentRequiredFor(
					fetchX402Resource,
					controller.X402SkuProMonth,
					authClientResult.Error.Message,
				)
				if err == nil {
					upgrade.terms = terms
				}
			}
			return nil, upgrade, nil
		}
		return nil, nil, errors.New(authClientResult.Error.Message)
	}

	if authClientResult.ProxyConfigResult == nil {
		return nil, nil, fmt.Errorf("could not establish an egress proxy")
	}

	proxyClient := authClientResult.ProxyConfigResult.ProxyClient
	return &acquiredProxy{
		signedProxyId: proxyClient.AuthToken,
		proxyClient:   &proxyClient,
		location:      locationName,
		created:       true,
	}, nil, nil
}

// Maps a free-text location query to a connect location. An empty query means
// no preference, which connects to the best available provider.
func resolveLocation(
	clientSession *session.ClientSession,
	locationQuery string,
) (*sdk.ConnectLocation, string, error) {
	if locationQuery == "" {
		return &sdk.ConnectLocation{
			ConnectLocationId: &sdk.ConnectLocationId{
				BestAvailable: true,
			},
		}, "best available", nil
	}

	findLocationsResult, err := model.FindProviderLocations(
		&model.FindLocationsArgs{Query: locationQuery},
		clientSession,
	)
	if err != nil {
		return nil, "", err
	}

	// a query that parses as a client id pins directly to that device
	if 0 < len(findLocationsResult.Devices) {
		device := findLocationsResult.Devices[0]
		return &sdk.ConnectLocation{
			ConnectLocationId: &sdk.ConnectLocationId{
				ClientId: server.ToSdkId(device.ClientId),
			},
		}, device.DeviceName, nil
	}

	// the search is ranked, and ties are broken by match distance then
	// provider count, so the first entry is the best match
	var best *model.LocationResult
	for _, locationResult := range findLocationsResult.Locations {
		if best == nil {
			best = locationResult
			continue
		}
		if locationResult.MatchDistance < best.MatchDistance {
			best = locationResult
		} else if locationResult.MatchDistance == best.MatchDistance &&
			best.ProviderCount < locationResult.ProviderCount {
			best = locationResult
		}
	}

	if best == nil {
		return nil, "", fmt.Errorf(
			"no provider locations match %q; try a broader location such as a region or country",
			locationQuery,
		)
	}

	return &sdk.ConnectLocation{
		ConnectLocationId: &sdk.ConnectLocationId{
			LocationId: server.ToSdkId(best.LocationId),
		},
		Name: best.Name,
	}, best.Name, nil
}

// Builds an http client whose requests egress through the proxy. Redirects are
// followed up to a cap, with each hop re-validated as a fetch target.
func newProxyHttpClient(proxy *acquiredProxy, timeout time.Duration) (*http.Client, error) {
	proxyUrl, err := proxyRequestUrl(proxy.proxyClient)
	if err != nil {
		return nil, err
	}

	// `DialContext` dials the proxy, not the target: with a proxy configured
	// the transport connects here and then tunnels with CONNECT, so this
	// governs the server to server hop only. The target host is resolved at
	// the egress location and is unaffected.
	dialer := &net.Dialer{
		Timeout:   fetchProxyDialTimeout,
		KeepAlive: fetchProxyKeepAlive,
	}

	transport := &http.Transport{
		Proxy: http.ProxyURL(proxyUrl),
		DialContext: func(ctx context.Context, network string, addr string) (net.Conn, error) {
			// server to server connections to the proxy are ipv4 only
			switch network {
			case "tcp", "tcp6":
				network = "tcp4"
			}
			return dialer.DialContext(ctx, network, addr)
		},
		MaxIdleConns:          fetchMaxIdleConns,
		IdleConnTimeout:       fetchIdleConnTimeout,
		TLSHandshakeTimeout:   fetchTlsHandshakeTimeout,
		ExpectContinueTimeout: 1 * time.Second,
	}

	return &http.Client{
		Transport: transport,
		Timeout:   timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if fetchMaxRedirects <= len(via) {
				return fmt.Errorf("stopped after %d redirects", fetchMaxRedirects)
			}
			return validateFetchUrl(req.URL)
		},
	}, nil
}

// The https proxy carries the signed proxy id as the leading hostname label,
// which the proxy reads from the tls server name. The plain http proxy has no
// tls to read it from, so the id rides in Proxy-Authorization instead (as the
// basic-auth username, which is what the proxy parses).
func proxyRequestUrl(proxyClient *model.ProxyClient) (*url.URL, error) {
	if fetchUsePlainHttpProxy {
		addr := fetchProxyAddrOverride
		if addr == "" {
			addr = net.JoinHostPort(
				proxyClient.ProxyHost,
				strconv.Itoa(proxyClient.HttpProxyPort),
			)
		}
		return &url.URL{
			Scheme: "http",
			Host:   addr,
			User:   url.UserPassword(proxyClient.AuthToken, ""),
		}, nil
	}

	proxyUrl, err := url.Parse(proxyClient.HttpsProxyUrl)
	if err != nil {
		return nil, fmt.Errorf("could not parse the proxy url")
	}
	return proxyUrl, nil
}
