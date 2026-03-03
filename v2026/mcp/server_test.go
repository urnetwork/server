package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/go-playground/assert/v2"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/jwt"
	"github.com/urnetwork/server/v2026/model"
	urSession "github.com/urnetwork/server/v2026/session"
)

// startTestServer starts a test server and returns its URL and cleanup function
func startTestServer(t *testing.T) (string, func()) {
	// Find available port
	listener, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatalf("Failed to find available port: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()

	addr := fmt.Sprintf("localhost:%d", port)

	// create mcp server
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "urnetwork-mcp",
		Version: "1.0.0",
	}, nil)

	registerTools(server)

	handler := mcp.NewStreamableHTTPHandler(func(req *http.Request) *mcp.Server {
		return server
	}, nil)

	// start server in background
	httpServer := &http.Server{
		Addr:    addr,
		Handler: handler,
	}

	go func() {
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			t.Logf("Server error: %v", err)
		}
	}()

	// Wait for server to be ready
	time.Sleep(100 * time.Millisecond)

	// Return URL and cleanup function
	cleanup := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		httpServer.Shutdown(ctx)
	}

	return fmt.Sprintf("http://%s", addr), cleanup
}

func TestProvidersList(t *testing.T) {
	server.DefaultTestEnv().Run(func() {
		serverURL, cleanup := startTestServer(t)
		defer cleanup()

		ctx := context.Background()
		client := mcp.NewClient(&mcp.Implementation{
			Name:    "test-client",
			Version: "1.0.0",
		}, nil)

		session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: serverURL}, nil)
		if err != nil {
			t.Fatalf("Failed to connect: %v", err)
		}
		defer session.Close()

		// ensure providerLocations is an available tool
		toolsResult, err := session.ListTools(ctx, nil)
		assert.Equal(t, err, nil)

		found := false
		for _, tool := range toolsResult.Tools {
			if tool.Name == "providerLocations" {
				found = true
				break
			}
		}
		assert.Equal(t, found, true)

		/**
		 * No provider locations found
		 */

		model.UpdateClientLocations(ctx, 30*time.Minute)

		result, err := session.CallTool(ctx, &mcp.CallToolParams{
			Name: "providerLocations",
			// passing empty should fetch a list of available countries
			Arguments: map[string]any{
				"query": "",
			},
		})
		assert.Equal(t, err, nil)
		assert.Equal(t, len(result.Content), 1)

		textContent, ok := result.Content[0].(*mcp.TextContent)
		assert.Equal(t, ok, true)

		assert.MatchRegex(t, textContent.Text, MsgNoProviderLocations)

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

		assert.Equal(t, city.CountryLocationId, country.LocationId)

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
			assert.Equal(t, err, nil)

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
			assert.Equal(t, err, nil)
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

		// call providerLocations tool
		result, err = session.CallTool(ctx, &mcp.CallToolParams{
			Name: "providerLocations",
			// passing empty should fetch all providers
			Arguments: map[string]any{
				"query": "",
			},
		})
		assert.Equal(t, err, nil)

		assert.Equal(t, len(result.Content), 1)

		textContent, ok = result.Content[0].(*mcp.TextContent)
		assert.Equal(t, ok, true)

		expectedCount := 1
		expectedMsg := fmt.Sprintf(MsgFoundProviderLocations, expectedCount)
		assert.Equal(t, strings.Contains(textContent.Text, expectedMsg), true)

	})
}
