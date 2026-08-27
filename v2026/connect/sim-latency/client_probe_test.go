package main

// Deterministic coverage for the measured-window matchmaking audit call.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/sdk/v2026"
	"github.com/urnetwork/server/v2026"
)

// Captures the exact wire request without relying on scheduler timing.
type observedMatchmakingProbe struct {
	args          sdk.FindProviders2Args
	forwardedFor  string
	authorization string
	err           error
}

func TestClientDriverProbeMatchmakingUsesPoolIdentityAndQualitySpec(t *testing.T) {
	providerId := sdk.NewId()
	providerStats := sdk.NewFindProvidersProviderList()
	providerStats.Add(&sdk.FindProvidersProvider{
		ClientId:                providerId,
		EstimatedBytesPerSecond: 1024,
	})
	observed := make(chan observedMatchmakingProbe, 1)
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		value := observedMatchmakingProbe{
			forwardedFor:  request.Header.Get("X-UR-Forwarded-For"),
			authorization: request.Header.Get("Authorization"),
		}
		if request.URL.Path != "/network/find-providers2" {
			value.err = fmt.Errorf("path = %q", request.URL.Path)
		} else if err := json.NewDecoder(request.Body).Decode(&value.args); err != nil {
			value.err = fmt.Errorf("decode request: %w", err)
		}
		observed <- value
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(&sdk.FindProviders2Result{ProviderStats: providerStats})
	}))
	defer apiServer.Close()

	config := defaultConfig(1, 1, 1, 60)
	config.Clients.QualityWindowSize = 3
	locationId := server.NewId()
	clientId := server.NewId()
	driver := &ClientDriver{
		config:     config,
		apiUrl:     apiServer.URL,
		locationId: locationId,
		pool: []ClientIdentity{{
			ClientId: clientId,
			ByJwt:    "matchmaking-probe-jwt",
		}},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := driver.ProbeMatchmaking(ctx); err != nil {
		t.Fatal(err)
	}
	value := <-observed
	if value.err != nil {
		t.Fatal(value.err)
	}
	if value.args.Count != 3 || value.args.RankMode != "quality" {
		t.Fatalf("probe shape = count %d rank %q", value.args.Count, value.args.RankMode)
	}
	if value.args.Specs == nil || value.args.Specs.Len() != 1 ||
		value.args.Specs.Get(0).LocationId == nil ||
		value.args.Specs.Get(0).LocationId.String() != locationId.String() {
		t.Fatalf("probe specs = %+v, want location %s", value.args.Specs, locationId)
	}
	if value.args.ExcludeClientIds == nil || value.args.ExcludeClientIds.Len() != 1 ||
		value.args.ExcludeClientIds.Get(0).String() != clientId.String() {
		t.Fatalf("probe exclusions = %+v, want client %s", value.args.ExcludeClientIds, clientId)
	}
	if value.forwardedFor != driver.clientForwardedFor(clientId) {
		t.Fatalf("forwarded-for = %q, want %q", value.forwardedFor, driver.clientForwardedFor(clientId))
	}
	if !strings.Contains(value.authorization, "matchmaking-probe-jwt") {
		t.Fatalf("authorization did not carry pool identity: %q", value.authorization)
	}
}

func TestClientDriverProbeMatchmakingRejectsEmptyProviderPool(t *testing.T) {
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(&sdk.FindProviders2Result{
			ProviderStats: sdk.NewFindProvidersProviderList(),
		})
	}))
	defer apiServer.Close()

	driver := &ClientDriver{
		config:     defaultConfig(1, 1, 1, 60),
		apiUrl:     apiServer.URL,
		locationId: server.NewId(),
		pool: []ClientIdentity{{
			ClientId: server.NewId(),
			ByJwt:    "matchmaking-probe-jwt",
		}},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := driver.ProbeMatchmaking(ctx)
	if err == nil || !strings.Contains(err.Error(), "empty provider pool") {
		t.Fatalf("empty provider pool error = %v", err)
	}
}
