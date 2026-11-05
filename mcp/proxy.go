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
//
// A call without a live handle first reuses the newest active proxy this tool
// created for the network and location (model.FindReusableNetworkProxy), and
// provisions only when there is none. The handle lasts fetchSealTtl and the
// caller never sees the proxy's credential, so a proxy whose handle lapsed was
// unreachable, yet stayed active until the idle reap: every new conversation
// created another, each kept a device on a proxy host and counted against the
// network's active proxy cap, where a free network's leftovers blocked the
// proxies its people create themselves. Proxies are shared across a network's
// calls for one location; cookies stay per call (newCookieJar).

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

// The device spec of the proxies this tool provisions, which is also how it
// finds them again; changing it orphans the proxies created under the old one.
const fetchProxyDeviceSpec = "mcp"

var errProxyGone = errors.New("the proxy for this signed_proxy_id no longer exists")

// Acquired egress, either reused or freshly provisioned.
type acquiredProxy struct {
	// opaque tenant-bound handle returned to the caller
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

type sealedProxyHandle struct {
	SignedProxyId string `json:"p"`
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
	binding string,
	signedProxyId string,
	locationQuery string,
) (*acquiredProxy, *upgradeRequired, error) {
	if signedProxyId != "" {
		proxy, err := reuseProxy(clientSession, binding, signedProxyId)
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

	return createProxy(clientSession, binding, locationQuery)
}

func reuseProxy(clientSession *session.ClientSession, binding string, signedProxyId string) (*acquiredProxy, error) {
	handle := &sealedProxyHandle{}
	if err := unseal(sealLabelProxy, binding, signedProxyId, handle); err != nil {
		return nil, fmt.Errorf("invalid signed_proxy_id")
	}
	proxyId, err := model.ParseSignedProxyId(handle.SignedProxyId)
	if err != nil {
		return nil, fmt.Errorf("invalid signed_proxy_id")
	}

	proxyDeviceConfig := model.GetProxyDeviceConfig(clientSession.Ctx, proxyId)
	if proxyDeviceConfig == nil {
		return nil, errProxyGone
	}
	networkClient := model.GetNetworkClient(clientSession.Ctx, proxyDeviceConfig.ClientId)
	if networkClient == nil || networkClient.NetworkId != clientSession.ByJwt.NetworkId {
		return nil, fmt.Errorf("signed_proxy_id is not owned by this network")
	}

	proxyClient, err := model.GetProxyClient(clientSession.Ctx, proxyId)
	if err != nil || proxyClient == nil {
		return nil, errProxyGone
	}

	location := ""
	if proxyDeviceConfig.InitialDeviceState != nil && proxyDeviceConfig.InitialDeviceState.Location != nil {
		location = proxyDeviceConfig.InitialDeviceState.Location.Name
	}

	refreshedHandle, err := seal(
		sealLabelProxy,
		binding,
		&sealedProxyHandle{SignedProxyId: handle.SignedProxyId},
		fetchSealTtl,
	)
	if err != nil {
		return nil, err
	}

	return &acquiredProxy{
		signedProxyId: refreshedHandle,
		proxyClient:   proxyClient,
		location:      location,
		created:       false,
	}, nil
}

func createProxy(
	clientSession *session.ClientSession,
	binding string,
	locationQuery string,
) (*acquiredProxy, *upgradeRequired, error) {
	connectLocation, locationName, err := resolveLocation(clientSession, locationQuery)
	if err != nil {
		return nil, nil, err
	}

	if proxy := reuseLocationProxy(clientSession, binding, connectLocation, locationName); proxy != nil {
		return proxy, nil, nil
	}

	// no ip lock: the proxy is used from this service, and locking to the
	// calling agent's ip would refuse our own egress connection
	authClientResult, err := model.AuthNetworkClient(
		&model.AuthNetworkClientArgs{
			Description: "mcp fetch",
			DeviceSpec:  fetchProxyDeviceSpec,
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
	sealedProxyId, err := seal(
		sealLabelProxy,
		binding,
		&sealedProxyHandle{SignedProxyId: proxyClient.AuthToken},
		fetchSealTtl,
	)
	if err != nil {
		return nil, nil, err
	}
	return &acquiredProxy{
		signedProxyId: sealedProxyId,
		proxyClient:   &proxyClient,
		location:      locationName,
		created:       true,
	}, nil, nil
}

// The newest active proxy this tool created for the network at the connect
// location, sealed into a fresh handle, or nil when there is none (or its
// credential cannot be read), in which case the caller provisions one.
func reuseLocationProxy(
	clientSession *session.ClientSession,
	binding string,
	connectLocation *sdk.ConnectLocation,
	locationName string,
) *acquiredProxy {
	proxyId := model.FindReusableNetworkProxy(
		clientSession.Ctx,
		clientSession.ByJwt.NetworkId,
		fetchProxyDeviceSpec,
		connectLocation,
		model.DefaultReusableNetworkProxySettings(),
	)
	if proxyId == nil {
		return nil
	}
	proxyClient, err := model.GetProxyClient(clientSession.Ctx, *proxyId)
	if err != nil || proxyClient == nil {
		return nil
	}
	sealedProxyId, err := seal(
		sealLabelProxy,
		binding,
		&sealedProxyHandle{SignedProxyId: proxyClient.AuthToken},
		fetchSealTtl,
	)
	if err != nil {
		return nil
	}
	return &acquiredProxy{
		signedProxyId: sealedProxyId,
		proxyClient:   proxyClient,
		location:      locationName,
		created:       false,
	}
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
		Transport:     transport,
		Timeout:       timeout,
		CheckRedirect: validateFetchRedirect,
	}, nil
}

func validateFetchRedirect(req *http.Request, via []*http.Request) error {
	if fetchMaxRedirects <= len(via) {
		return fmt.Errorf("stopped after %d redirects", fetchMaxRedirects)
	}
	if err := validateFetchUrl(req.URL); err != nil {
		return err
	}
	if len(via) == 0 || sameFetchOrigin(via[0].URL, req.URL) {
		return nil
	}

	// A body-preserving redirect must never send caller data to a different
	// origin. Redirects that have become GET/HEAD are safe after stripping
	// everything except the public header allowlist.
	if req.Body != nil || 0 < req.ContentLength {
		return fmt.Errorf("refusing to redirect a request body across origins")
	}
	stripCrossOriginRequestHeaders(req)
	return nil
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
