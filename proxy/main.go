package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	// "net/netip"
	"flag"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	gojwt "github.com/golang-jwt/jwt/v5"
	"github.com/samber/lo"
	"github.com/stripe/goproxy"
	"github.com/things-go/go-socks5"
	"github.com/urfave/cli/v2"
	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/proxy"
)

// FIXME this is meant to be deployed with no lb and no containers
// FIXME is launches a block of ports per version,
// FIXME and registers the host, ports, version when online
// FIXME the ncm will use the latest versions when distributing new proxies
// FIXME a client can run a proxy on any host:port
// FIXME the proxy server spins up a new resident unique to the service
// FIXME each proxy instance has an idle timeout where it will shut down if not used in M minutes

// this value is set via the linker, e.g.
// -ldflags "-X main.Version=$WARP_VERSION-$WARP_VERSION_CODE"
var Version string

func init() {
	initGlog()
}

func initGlog() {
	// flag.Set("logtostderr", "true")
	flag.Set("alsologtostderr", "true")
	flag.Set("stderrthreshold", "INFO")
	flag.Set("v", "0")
	// unlike unix, the android/ios standard is for diagnostics to go to stdout
	os.Stderr = os.Stdout
}

func main() {
	cfg := struct {
		addr        string
		apiURL      string
		platformURL string
		userAuth    string
		password    string
		providerID  string
		city        string
		country     string
		region      string
	}{}
	app := &cli.App{
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:        "addr",
				Usage:       "Socks5 server address",
				EnvVars:     []string{"ADDR"},
				Destination: &cfg.addr,
				Value:       ":9999",
			},
			&cli.StringFlag{
				Name:        "api-url",
				Usage:       "API URL",
				EnvVars:     []string{"API_URL"},
				Destination: &cfg.apiURL,
				Value:       "https://api.bringyour.com",
			},
			&cli.StringFlag{
				Name:        "platform-url",
				Usage:       "Platform URL",
				EnvVars:     []string{"PLATFORM_URL"},
				Destination: &cfg.platformURL,
				Value:       "wss://connect.bringyour.com",
			},
			&cli.StringFlag{
				Name:        "user-auth",
				Usage:       "User auth",
				EnvVars:     []string{"USER_AUTH"},
				Destination: &cfg.userAuth,
				Required:    true,
			},
			&cli.StringFlag{
				Name:        "password",
				Usage:       "Password",
				EnvVars:     []string{"PASSWORD"},
				Destination: &cfg.password,
				Required:    true,
			},
			&cli.StringFlag{
				Name:        "provider-id",
				Usage:       "Provider ID",
				EnvVars:     []string{"PROVIDER_ID"},
				Destination: &cfg.providerID,
			},
			&cli.StringFlag{
				Name:        "city",
				Usage:       "City",
				EnvVars:     []string{"CITY"},
				Destination: &cfg.city,
			},
			&cli.StringFlag{
				Name:        "country",
				Usage:       "Country",
				EnvVars:     []string{"COUNTRY"},
				Destination: &cfg.country,
			},
			&cli.StringFlag{
				Name:        "region",
				Usage:       "Region",
				EnvVars:     []string{"REGION"},
				Destination: &cfg.region,
			},
		},
		Name: "socksproxy",
		Action: func(c *cli.Context) error {

			ctx := c.Context

			jwt, err := login(ctx, cfg.apiURL, cfg.userAuth, cfg.password)
			if err != nil {
				return fmt.Errorf("login failed: %w", err)
			}

			locations, err := getProviderLocations(
				ctx,
				cfg.apiURL,
				jwt,
			)
			if err != nil {
				return fmt.Errorf("get locations failed: %w", err)
			}

			providersSpec, err := getProviderSpec(
				locations,
				cfg.city,
				cfg.country,
				cfg.region,
				cfg.providerID,
			)
			if err != nil {
				return fmt.Errorf("get provider spec failed: %w", err)
			}

			clientJWT, err := authNetworkClient(
				ctx,
				cfg.apiURL,
				jwt,
				&connect.AuthNetworkClientArgs{
					Description: "my device",
					DeviceSpec:  "socks5",
				},
			)

			if err != nil {
				return fmt.Errorf("auth network client failed: %w", err)
			}

			clientID, err := parseByJwtClientId(clientJWT)
			if err != nil {
				return fmt.Errorf("parse byJwt client id failed: %w", err)
			}

			fmt.Println("my clientID:", clientID)

			generator := connect.NewApiMultiClientGenerator(
				ctx,
				providersSpec,
				connect.NewClientStrategyWithDefaults(ctx),
				// exclude self
				[]connect.Id{
					clientID,
				},
				cfg.apiURL,
				clientJWT,
				cfg.platformURL,
				"my device",
				"socks5",
				"0.0.0",
				&clientID,
				// connect.DefaultClientSettingsNoNetworkEvents,
				connect.DefaultClientSettings,
				connect.DefaultApiMultiClientGeneratorSettings(),
			)

			tnet, err := proxy.CreateNetTUN(
				ctx,
				1440,
			)
			if err != nil {
				return fmt.Errorf("create net tun failed: %w", err)
			}
			dev := proxy.Device(tnet)

			mc := connect.NewRemoteUserNatMultiClientWithDefaults(
				ctx,
				generator,
				func(source connect.TransferPath, provideMode protocol.ProvideMode, ipPath *connect.IpPath, packet []byte) {
					_, err := dev.Write(packet)
					if err != nil {
						fmt.Println("packet write error:", err)
					}
				},
				protocol.ProvideMode_Network,
			)

			source := connect.SourceId(clientID)

			go func() {
				for {
					packet, err := dev.Read()
					if err == nil {
						mc.SendPacket(
							source,
							protocol.ProvideMode_Network,
							packet,
							time.Second*15,
						)
					}
					if err != nil {
						fmt.Println("read error:", err)
						return
					}
				}
			}()

			server := socks5.NewServer(
				socks5.WithLogger(socks5.NewLogger(log.New(os.Stdout, "socks5: ", log.LstdFlags))),
				socks5.WithDialAndRequest(func(ctx context.Context, network, addr string, request *socks5.Request) (net.Conn, error) {

					fmt.Println("Dialing", network, addr, request.RawDestAddr.FQDN)

					// ap, err := netip.ParseAddrPort(addr)
					// if err != nil {
					// 	return nil, err
					// }

					return tnet.DialContext(ctx, network, addr)
				}),
			)

			go server.ListenAndServe("tcp", cfg.addr)

			fmt.Printf("socks5 server is listening on %s\n", cfg.addr)

			httpProxy := goproxy.NewProxyHttpServer()

			// tr := &http.Transport{
			// 	IdleConnTimeout: 300 * time.Second,
			// 	TLSHandshakeTimeout: 30 * time.Second,

			// }
			// httpProxy.Tr = tr

			// httpProxy.AllowHTTP2 = false
			httpProxy.ConnectDialContext = func(proxyCtx *goproxy.ProxyCtx, network string, addr string) (conn net.Conn, err error) {
				return tnet.DialContext(ctx, network, addr)
			}

			// proxyConnection.httpProxy = httpProxy

			addr := fmt.Sprintf(":%d", 9998)

			// tlsConfig := &tls.Config{
			// 	GetConfigForClient: self.transportTls.GetTlsConfigForClient,
			// }

			httpServer := &http.Server{
				Addr:    addr,
				Handler: httpProxy,
				// TLSConfig: tlsConfig,
			}

			// listenIpv4, _, listenPort := server.RequireListenIpPort(self.settings.ListenHttpsPort)

			listenConfig := net.ListenConfig{}

			serverConn, err := listenConfig.Listen(
				ctx,
				"tcp",
				// net.JoinHostPort(listenIpv4, strconv.Itoa(listenPort)),
				addr,
			)
			if err != nil {
				panic(err)
			}
			defer serverConn.Close()

			// if self.settings.EnableProxyProtocol {
			// 	serverConn = NewPpServerConn(serverConn, DefaultWarpPpSettings())
			// }

			go httpServer.Serve(serverConn)

			fmt.Printf("http server is listening on %s\n", 9998)

			<-ctx.Done()

			return nil

		},
	}
	app.RunAndExitOnError()

}

func getProviderSpec(
	locations *FindLocationsResult,
	city string,
	country string,
	region string,
	providerID string,
) ([]*connect.ProviderSpec, error) {

	if providerID != "" {
		cid, err := connect.ParseId(providerID)
		if err != nil {
			return nil, fmt.Errorf("parse provider id failed: %w", err)
		}

		fmt.Println("provider match", cid)

		return []*connect.ProviderSpec{
			{
				ClientId: &cid,
			},
		}, nil
	}

	if city != "" {
		for _, v := range locations.Locations.Values() {

			switch v.LocationType {
			case "city":
				if strings.ToLower(v.Name) == strings.ToLower(city) {
					fmt.Printf("city matched %q, provider count %d\n", v.Name, v.ProviderCount)
					return []*connect.ProviderSpec{
						{
							LocationId: v.LocationId,
						},
					}, nil
				}
			}

		}
	}

	if country != "" {

		for _, v := range locations.Locations.Values() {

			switch v.LocationType {
			case "country":
				if strings.ToLower(v.Name) == strings.ToLower(country) {
					fmt.Printf("country matched %q, provider count %d\n", v.Name, v.ProviderCount)
					return []*connect.ProviderSpec{
						{
							LocationId: v.LocationId,
						},
					}, nil
				}
			}

		}
	}

	if region != "" {

		for _, v := range locations.Locations.Values() {

			switch v.LocationType {
			case "region":
				if strings.ToLower(v.Name) == strings.ToLower(region) {
					fmt.Printf("region matched %q, provider count %d\n", v.Name, v.ProviderCount)
					return []*connect.ProviderSpec{
						{
							LocationId: v.LocationId,
						},
					}, nil
				}
			}

		}
	}

	regions := lo.Filter(locations.Locations.Values(), func(v *LocationResult, _ int) bool {
		return v.LocationType == "region"
	})

	cities := lo.Filter(locations.Locations.Values(), func(v *LocationResult, _ int) bool {
		return v.LocationType == "city"
	})

	countries := lo.Filter(locations.Locations.Values(), func(v *LocationResult, _ int) bool {
		return v.LocationType == "country"
	})

	uniqNames := func(locations []*LocationResult) []string {
		names := lo.Map(locations, func(v *LocationResult, _ int) string {
			return v.Name
		})
		slices.Sort(names)
		return lo.Uniq(names)
	}

	prefixEach := func(prefix string, names []string) []string {
		return lo.Map(names, func(v string, _ int) string {
			return prefix + v
		})
	}

	return nil, fmt.Errorf(
		`please specify a location: city, country, region or provider id from this list:
 countries:
%s
 regions:
%s
 cities:
%s`,
		strings.Join(prefixEach("  ", uniqNames(countries)), "\n"),
		strings.Join(prefixEach("  ", uniqNames(regions)), "\n"),
		strings.Join(prefixEach("  ", uniqNames(cities)), "\n"),
	)

}

func login(ctx context.Context, apiURL, userAuth, password string) (string, error) {
	api := connect.NewBringYourApi(
		ctx,
		connect.NewClientStrategyWithDefaults(ctx),
		apiURL,
	)

	// api.AuthNetworkClient()
	type loginResult struct {
		res *connect.AuthLoginWithPasswordResult
		err error
	}

	resChan := make(chan loginResult)

	api.AuthLoginWithPassword(
		&connect.AuthLoginWithPasswordArgs{
			UserAuth: userAuth,
			Password: password,
		},
		connect.NewApiCallback(
			func(res *connect.AuthLoginWithPasswordResult, err error) {
				resChan <- loginResult{res, err}
			},
		),
	)

	res := <-resChan
	if res.res.Error != nil {
		return "", errors.New(res.res.Error.Message)
	}

	if res.res.VerificationRequired != nil {
		return "", errors.New("verification required")
	}

	return res.res.Network.ByJwt, nil

}

func getProviderLocations(ctx context.Context, apiURL string, jwt string) (*FindLocationsResult, error) {

	strategy := connect.NewClientStrategyWithDefaults(ctx)

	return connect.HttpGetWithStrategy(
		ctx,
		strategy,
		fmt.Sprintf("%s/network/provider-locations", apiURL),
		jwt,
		&FindLocationsResult{},
		connect.NewNoopApiCallback[*FindLocationsResult](),
	)

}

// func (self *BringYourApi) FindProviders(findProviders *FindProvidersArgs, callback FindProvidersCallback) {
// 	go connect.HandleError(func() {
// 		connect.HttpPostWithStrategy(
// 			self.ctx,
// 			self.clientStrategy,
// 			fmt.Sprintf("%s/network/find-providers", self.apiUrl),
// 			findProviders,
// 			self.GetByJwt(),
// 			&FindProvidersResult{},
// 			callback,
// 		)
// 	})
// }

func findProviders(ctx context.Context, apiURL string, jwt string, args *FindProvidersArgs) (*FindProvidersResult, error) {
	strategy := connect.NewClientStrategyWithDefaults(ctx)

	return connect.HttpPostWithStrategy(
		ctx,
		strategy,
		fmt.Sprintf("%s/network/find-providers", apiURL),
		args,
		jwt,
		&FindProvidersResult{},
		connect.NewNoopApiCallback[*FindProvidersResult](),
	)
}

func authNetworkClient(ctx context.Context, apiURL, jwt string, req *connect.AuthNetworkClientArgs) (string, error) {
	strategy := connect.NewClientStrategyWithDefaults(ctx)

	res, err := connect.HttpPostWithStrategy(
		ctx,
		strategy,
		fmt.Sprintf("%s/network/auth-client", apiURL),
		req,
		jwt,
		&connect.AuthNetworkClientResult{},
		connect.NewNoopApiCallback[*connect.AuthNetworkClientResult](),
	)

	if err != nil {
		return "", err
	}

	if res.Error != nil {
		return "", errors.New(res.Error.Message)
	}

	return res.ByClientJwt, nil
}

func parseByJwtClientId(byJwt string) (connect.Id, error) {
	claims := gojwt.MapClaims{}
	gojwt.NewParser().ParseUnverified(byJwt, claims)

	jwtClientId, ok := claims["client_id"]
	if !ok {
		return connect.Id{}, fmt.Errorf("byJwt does not contain claim client_id")
	}
	switch v := jwtClientId.(type) {
	case string:
		return connect.ParseId(v)
	default:
		return connect.Id{}, fmt.Errorf("byJwt hav invalid type for client_id: %T", v)
	}
}
