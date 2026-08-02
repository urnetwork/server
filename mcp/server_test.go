package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/oauth"
	"github.com/urnetwork/server/router"
	urSession "github.com/urnetwork/server/session"
)

// Starts the production mcp assembly (middleware, tools, stateless
// streamable http handler) on an ephemeral port and returns its url and
// cleanup function. The listener queues connections from the moment it is
// created, so there is no readiness race.
func startTestServer(t testing.TB) (string, func()) {
	listener, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatalf("Failed to listen: %v", err)
	}

	// the production route table, not just the mcp handler: the protected
	// resource metadata route and the auth wrapping only exist here
	ctx, cancelRoutes := context.WithCancel(context.Background())
	httpServer := &http.Server{
		Handler: router.NewRouter(ctx, Routes()),
	}

	go func() {
		if err := httpServer.Serve(listener); err != nil && err != http.ErrServerClosed {
			t.Logf("Server error: %v", err)
		}
	}()

	cleanup := func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		httpServer.Shutdown(shutdownCtx)
		cancelRoutes()
	}

	return fmt.Sprintf("http://%s", listener.Addr().String()), cleanup
}

// A network and an access token for it, carrying the given scopes.
//
// The token is minted by the real authorization server code with this
// resource's audience, so the tests exercise the same verification path
// production does rather than a stub.
func mintTestAccessToken(t testing.TB, ctx context.Context, scopes []string) (string, server.Id, server.Id) {
	networkId := server.NewId()
	userId := server.NewId()
	networkName := fmt.Sprintf("mcptest-%s", networkId)
	model.Testing_CreateNetwork(ctx, networkId, networkName, userId)

	accessToken, _, err := oauth.MintAccessToken(&oauth.MintAccessTokenArgs{
		UserId:    userId,
		NetworkId: networkId,
		ClientId:  "https://claude.ai/mcp",
		Audience:  McpResource,
		Scopes:    scopes,
	})
	if err != nil {
		t.Fatalf("Failed to mint an access token: %v", err)
	}

	return accessToken, networkId, userId
}

// Connects an mcp client bearing an access token with the given scopes.
func connectAuthedTestClient(
	t testing.TB,
	ctx context.Context,
	serverUrl string,
	scopes []string,
) *mcpsdk.ClientSession {
	accessToken, _, _ := mintTestAccessToken(t, ctx, scopes)
	return connectTestClientWithToken(t, ctx, serverUrl, accessToken)
}

func connectTestClientWithToken(
	t testing.TB,
	ctx context.Context,
	serverUrl string,
	accessToken string,
) *mcpsdk.ClientSession {
	header := http.Header{}
	header.Set("Authorization", fmt.Sprintf("Bearer %s", accessToken))

	client := mcpsdk.NewClient(&mcpsdk.Implementation{
		Name:    "test-client",
		Version: "1.0.0",
	}, nil)

	session, err := client.Connect(ctx, &mcpsdk.StreamableClientTransport{
		Endpoint: serverUrl,
		HTTPClient: &http.Client{
			Timeout: 60 * time.Second,
			Transport: &headerRoundTripper{
				header: header,
				base:   http.DefaultTransport,
			},
		},
	}, nil)
	if err != nil {
		t.Fatalf("Failed to connect: %v", err)
	}

	return session
}

// Adds fixed headers to every outgoing request.
type headerRoundTripper struct {
	header http.Header
	base   http.RoundTripper
}

func (self *headerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	// per the RoundTripper contract, do not mutate the original request
	req = req.Clone(req.Context())
	for name, values := range self.header {
		req.Header.Del(name)
		for _, value := range values {
			req.Header.Add(name, value)
		}
	}
	return self.base.RoundTrip(req)
}

func TestProvidersList(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		serverURL, cleanup := startTestServer(t)
		defer cleanup()

		ctx := context.Background()
		session := connectAuthedTestClient(t, ctx, serverURL, []string{oauth.ScopeMcpRead})
		defer session.Close()

		// ensure providerLocations is an available tool
		toolsResult, err := session.ListTools(ctx, nil)
		connect.AssertEqual(t, err, nil)

		found := false
		for _, tool := range toolsResult.Tools {
			if tool.Name == "providerLocations" {
				found = true
				break
			}
		}
		connect.AssertEqual(t, found, true)

		/**
		 * No provider locations found
		 */

		model.UpdateClientLocations(ctx, 30*time.Minute)

		result, err := session.CallTool(ctx, &mcpsdk.CallToolParams{
			Name: "providerLocations",
			// passing empty should fetch a list of available countries
			Arguments: map[string]any{
				"query": "",
			},
		})
		connect.AssertEqual(t, err, nil)
		// summary text plus the mirrored structured json
		connect.AssertEqual(t, len(result.Content), 2)

		textContent, ok := result.Content[0].(*mcpsdk.TextContent)
		connect.AssertEqual(t, ok, true)

		connect.AssertEqual(t, strings.Contains(textContent.Text, MsgNoProviderLocations), true)

		/**
		 * Setup location group
		 */

		country := &model.Location{
			LocationType: model.LocationTypeCountry,
			Country:      "United States",
			CountryCode:  "us",
		}
		model.CreateLocation(ctx, country)

		city := &model.Location{
			LocationType: model.LocationTypeCity,
			City:         "Palo Alto",
			Region:       "California",
			Country:      "United States",
			CountryCode:  "us",
		}
		model.CreateLocation(ctx, city)

		connect.AssertEqual(t, city.CountryLocationId, country.LocationId)

		createLocationGroup := &model.LocationGroup{
			Name:     model.StrongPrivacyLaws,
			Promoted: true,
			MemberLocationIds: []server.Id{
				city.CityLocationId,
				city.RegionLocationId,
				city.CountryLocationId,
			},
		}

		model.CreateLocationGroup(ctx, createLocationGroup)

		/**
		 * Setup providers
		 */
		clientSessions := map[server.Id]*urSession.ClientSession{}
		n := 16

		for i := range n {
			networkId := server.NewId()

			userId := server.NewId()
			guestMode := false
			isPro := false

			clientSession := urSession.Testing_CreateClientSession(
				ctx,
				jwt.NewByJwt(
					networkId,
					userId,
					fmt.Sprintf("network%d", i),
					guestMode,
					isPro,
				),
			)

			clientId := server.NewId()

			clientSessions[clientId] = clientSession

			model.Testing_CreateDevice(
				ctx,
				networkId,
				server.NewId(),
				clientId,
				"",
				"",
			)

			handlerId := model.CreateNetworkClientHandler(ctx)
			connectionId, _, _, _, err := model.ConnectNetworkClient(
				ctx,
				clientId,
				// use a unique ip per connection
				fmt.Sprintf("0.0.0.%d:0", i),
				handlerId,
			)
			connect.AssertEqual(t, err, nil)

			secretKeys := map[model.ProvideMode][]byte{
				model.ProvideModePublic: make([]byte, 32),
			}

			model.SetProvide(ctx, clientId, secretKeys)

			model.SetConnectionLocation(ctx, connectionId, city.LocationId, &model.ConnectionLocationScores{})

			// Insert speed and latency test records directly to satisfy reliability requirements
			// UpdateClientScores penalizes clients without these tests, causing them to be excluded.
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(
					ctx,
					`
                    INSERT INTO network_client_speed (connection_id, bytes_per_second)
                    VALUES ($1, $2)
                    `,
					connectionId,
					int64(100*1024*1024), // 100 MB/s
				))
				server.RaisePgResult(tx.Exec(
					ctx,
					`
                    INSERT INTO network_client_latency (connection_id, latency_ms)
                    VALUES ($1, $2)
                    `,
					connectionId,
					20, // 20ms
				))
			})

			clientAddressHash, _, err := clientSession.ClientAddressHashPort()
			connect.AssertEqual(t, err, nil)
			stats := &model.ClientReliabilityStats{
				ConnectionEstablishedCount: 1,
				ProvideEnabledCount:        1,
				ReceiveMessageCount:        1,
				ReceiveByteCount:           1024,
				SendMessageCount:           1,
				SendByteCount:              1024,
			}
			model.AddClientReliabilityStatsRange(
				ctx,
				networkId,
				clientId,
				clientAddressHash,
				server.NowUtc().Add(-13*time.Hour),
				server.NowUtc(),
				stats,
			)
		}

		model.UpdateClientReliabilityScores(ctx, server.NowUtc(), true)
		model.UpdateClientScores(ctx, 5*time.Second, 1)
		model.UpdateClientLocations(ctx, 30*time.Minute)

		// call providerLocations tool. The query argument is optional now,
		// so omit it to fetch the available countries
		result, err = session.CallTool(ctx, &mcpsdk.CallToolParams{
			Name:      "providerLocations",
			Arguments: map[string]any{},
		})
		connect.AssertEqual(t, err, nil)

		connect.AssertEqual(t, len(result.Content), 2)

		textContent, ok = result.Content[0].(*mcpsdk.TextContent)
		connect.AssertEqual(t, ok, true)

		expectedCount := 1
		expectedMsg := fmt.Sprintf(MsgFoundProviderLocations, expectedCount)
		connect.AssertEqual(t, strings.Contains(textContent.Text, expectedMsg), true)

		// the second content block mirrors the structured output as json
		jsonContent, ok := result.Content[1].(*mcpsdk.TextContent)
		connect.AssertEqual(t, ok, true)

		out := &ProviderLocationsResult{}
		err = json.Unmarshal([]byte(jsonContent.Text), out)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(out.Locations), expectedCount)
		connect.AssertEqual(t, out.Locations[0].Name, "United States")

		// the structured content is also set
		connect.AssertEqual(t, result.StructuredContent != nil, true)
	})
}

func TestStatelessTransport(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		serverURL, cleanup := startTestServer(t)
		defer cleanup()

		ctx := context.Background()

		// stateless mode does not offer the standalone sse stream. The request
		// is authorized first, so this asserts the transport behavior rather
		// than tripping the 401 that an anonymous GET now gets
		accessToken, _, _ := mintTestAccessToken(t, ctx, []string{oauth.ScopeMcpRead})
		request, err := http.NewRequest(http.MethodGet, serverURL, nil)
		connect.AssertEqual(t, err, nil)
		request.Header.Set("Authorization", fmt.Sprintf("Bearer %s", accessToken))
		response, err := http.DefaultClient.Do(request)
		connect.AssertEqual(t, err, nil)
		response.Body.Close()
		connect.AssertEqual(t, response.StatusCode, http.StatusMethodNotAllowed)

		// independent clients can call without any shared server state
		for range 2 {
			session := connectAuthedTestClient(t, ctx, serverURL, []string{oauth.ScopeMcpRead})

			toolsResult, err := session.ListTools(ctx, nil)
			connect.AssertEqual(t, err, nil)

			// every tool is listed to every client, with the annotations the
			// connectors directory requires
			toolNames := map[string]bool{}
			for _, tool := range toolsResult.Tools {
				toolNames[tool.Name] = true
				connect.AssertEqual(t, tool.Annotations != nil, true)
				connect.AssertEqual(t, tool.Annotations.Title != "", true)
			}
			connect.AssertEqual(t, toolNames["providerLocations"], true)
			connect.AssertEqual(t, toolNames["fetch"], true)

			session.Close()
		}
	})
}

// The resource server refuses anything that is not one of its own access
// tokens. This is the cut-over guarantee: a platform ByJwt or an api key is
// exactly the "other token" the mcp spec says must not be accepted here.
func TestRejectsNonOauthCredentials(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		serverURL, cleanup := startTestServer(t)
		defer cleanup()

		ctx := context.Background()

		networkId := server.NewId()
		userId := server.NewId()
		networkName := fmt.Sprintf("mcpcut-%s", networkId)
		model.Testing_CreateNetwork(ctx, networkId, networkName, userId)
		byJwt := jwt.NewByJwt(networkId, userId, networkName, false, false).Sign()

		refused := []string{
			// a full platform credential, which used to work here
			byJwt,
			// an api key
			"urn_invalid",
			"not-a-token",
		}

		for _, credential := range refused {
			status := mcpStatusWithAuth(t, serverURL, fmt.Sprintf("Bearer %s", credential))
			connect.AssertEqual(t, status, http.StatusUnauthorized)
		}

		// no credential at all is refused the same way, and the challenge tells
		// the client where to authorize
		response := mcpResponseWithAuth(t, serverURL, "")
		defer response.Body.Close()
		connect.AssertEqual(t, response.StatusCode, http.StatusUnauthorized)
		challenge := response.Header.Get("WWW-Authenticate")
		connect.AssertEqual(t, strings.Contains(challenge, "resource_metadata="), true)
		connect.AssertEqual(t, strings.Contains(challenge, ProtectedResourceMetadataUrl()), true)
	})
}

// A token minted for a different resource must not work here, which is the
// rfc 8707 audience binding.
func TestRejectsTokenForAnotherResource(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		serverURL, cleanup := startTestServer(t)
		defer cleanup()

		ctx := context.Background()

		networkId := server.NewId()
		userId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, fmt.Sprintf("mcpaud-%s", networkId), userId)

		otherToken, _, err := oauth.MintAccessToken(&oauth.MintAccessTokenArgs{
			UserId:    userId,
			NetworkId: networkId,
			ClientId:  "https://claude.ai/mcp",
			Audience:  "https://other.bringyour.com",
			Scopes:    []string{oauth.ScopeMcpRead},
		})
		connect.AssertEqual(t, err, nil)

		status := mcpStatusWithAuth(t, serverURL, fmt.Sprintf("Bearer %s", otherToken))
		connect.AssertEqual(t, status, http.StatusUnauthorized)
	})
}

// The protected resource metadata is what a client reads after the 401, so it
// must be reachable without a token.
func TestProtectedResourceMetadataIsPublic(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		serverURL, cleanup := startTestServer(t)
		defer cleanup()

		response, err := http.Get(serverURL + "/.well-known/oauth-protected-resource")
		connect.AssertEqual(t, err, nil)
		defer response.Body.Close()
		connect.AssertEqual(t, response.StatusCode, http.StatusOK)

		metadata := &oauth.ProtectedResourceMetadata{}
		connect.AssertEqual(t, json.NewDecoder(response.Body).Decode(metadata), nil)
		connect.AssertEqual(t, metadata.Resource, McpResource)
		connect.AssertEqual(t, metadata.AuthorizationServers[0], oauth.Issuer())
		// offline_access is never a resource requirement, per the mcp spec
		connect.AssertEqual(t, oauth.HasScope(metadata.ScopesSupported, oauth.ScopeOfflineAccess), false)
	})
}

// A token that authenticated but lacks a tool's scope gets a tool error naming
// the scope to request, rather than a blanket failure.
func TestToolScopeIsEnforced(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		serverURL, cleanup := startTestServer(t)
		defer cleanup()

		ctx := context.Background()
		// read scope only: enough for the transport, not enough for fetch
		session := connectAuthedTestClient(t, ctx, serverURL, []string{oauth.ScopeMcpRead})
		defer session.Close()

		result, err := session.CallTool(ctx, &mcpsdk.CallToolParams{
			Name: "fetch",
			Arguments: map[string]any{
				"url": "https://example.com/",
			},
		})
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.IsError, true)

		text := ""
		for _, content := range result.Content {
			if textContent, ok := content.(*mcpsdk.TextContent); ok {
				text += textContent.Text
			}
		}
		connect.AssertEqual(t, strings.Contains(text, oauth.ScopeMcpFetch), true)
	})
}

// Issues a bare mcp request with the given Authorization header and returns the
// status, for the cases where the failure is at the transport rather than in a
// tool result.
func mcpStatusWithAuth(t testing.TB, serverUrl string, authorization string) int {
	response := mcpResponseWithAuth(t, serverUrl, authorization)
	defer response.Body.Close()
	return response.StatusCode
}

func mcpResponseWithAuth(t testing.TB, serverUrl string, authorization string) *http.Response {
	request, err := http.NewRequest(
		http.MethodPost,
		serverUrl,
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`),
	)
	if err != nil {
		t.Fatalf("Failed to build a request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("Failed to send a request: %v", err)
	}
	return response
}
