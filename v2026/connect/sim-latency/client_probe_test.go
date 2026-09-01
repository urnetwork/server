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
	helloObserved := make(chan struct{}, 1)
	observed := make(chan observedMatchmakingProbe, 1)
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodGet && request.URL.Path == "/hello" {
			select {
			case helloObserved <- struct{}{}:
			default:
			}
			w.WriteHeader(http.StatusOK)
			return
		}
		if request.Method != http.MethodPost || request.URL.Path != "/network/find-providers2" {
			http.Error(w, "unexpected matchmaking probe route", http.StatusNotFound)
			return
		}
		value := observedMatchmakingProbe{
			forwardedFor:  request.Header.Get("X-UR-Forwarded-For"),
			authorization: request.Header.Get("Authorization"),
		}
		if err := json.NewDecoder(request.Body).Decode(&value.args); err != nil {
			value.err = fmt.Errorf("decode request: %w", err)
		}
		select {
		case observed <- value:
		default:
			http.Error(w, "duplicate matchmaking probe", http.StatusConflict)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(&sdk.FindProviders2Result{ProviderStats: providerStats})
	}))
	defer apiServer.Close()

	config := defaultConfig(1, 1, 1, 60)
	config.Clients.QualityWindowSize = 3
	locationId := server.NewId()
	decoyClientId := server.NewId()
	clientId := server.NewId()
	driver := &ClientDriver{
		config:     config,
		apiUrl:     apiServer.URL,
		locationId: locationId,
		pool: []ClientIdentity{
			{ClientId: decoyClientId, ByJwt: "decoy-matchmaking-probe-jwt"},
			{ClientId: clientId, ByJwt: "matchmaking-probe-jwt"},
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := driver.ProbeMatchmaking(ctx, 1); err != nil {
		t.Fatal(err)
	}
	select {
	case <-helloObserved:
	default:
		t.Fatal("matchmaking probe skipped route hello")
	}
	var value observedMatchmakingProbe
	select {
	case value = <-observed:
	case <-ctx.Done():
		t.Fatal("matchmaking POST was not observed")
	}
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
		if request.Method == http.MethodGet && request.URL.Path == "/hello" {
			w.WriteHeader(http.StatusOK)
			return
		}
		if request.Method != http.MethodPost || request.URL.Path != "/network/find-providers2" {
			http.Error(w, "unexpected matchmaking probe route", http.StatusNotFound)
			return
		}
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
	err := driver.ProbeMatchmaking(ctx, 0)
	if err == nil || !strings.Contains(err.Error(), "empty provider pool") {
		t.Fatalf("empty provider pool error = %v", err)
	}
}

func TestMatchmakingProbeOffsetsSpanMeasurementWindow(t *testing.T) {
	durations := []time.Duration{time.Second, 20 * time.Second, 3 * time.Minute}
	for _, duration := range durations {
		offsets := matchmakingProbeOffsets(duration)
		if len(offsets) < 2 || offsets[0] != 0 {
			t.Errorf("duration %s offsets = %v, want a zero-based multi-probe schedule", duration, offsets)
			continue
		}
		lastOffset := offsets[len(offsets)-1]
		spanFraction := float64(lastOffset) / float64(duration)
		if spanFraction < minimumFindProvidersSampleSpanFraction || duration <= lastOffset {
			t.Errorf(
				"duration %s last offset %s spans %.6f, want [%.2f, 1)",
				duration,
				lastOffset,
				spanFraction,
				minimumFindProvidersSampleSpanFraction,
			)
		}
	}
}

func TestRunMatchmakingProbesUsesCompleteAbsoluteSchedule(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	measureStart := time.Unix(1_000, 0)
	offsets := matchmakingProbeOffsets(3 * time.Minute)
	waitTargets := []time.Time{}
	probeIndexes := []int{}
	err := runMatchmakingProbes(
		ctx,
		measureStart,
		offsets,
		0,
		func(_ context.Context, target time.Time) error {
			waitTargets = append(waitTargets, target)
			return nil
		},
		func(_ context.Context, probeIndex int) error {
			probeIndexes = append(probeIndexes, probeIndex)
			if len(probeIndexes) == len(offsets) {
				cancel()
			}
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(waitTargets) != len(offsets) || len(probeIndexes) != len(offsets) {
		t.Fatalf("waits=%d probes=%d, want %d", len(waitTargets), len(probeIndexes), len(offsets))
	}
	for index, offset := range offsets {
		if waitTargets[index] != measureStart.Add(offset) || probeIndexes[index] != index {
			t.Fatalf(
				"schedule %d = wait %s probe %d, want wait %s probe %d",
				index,
				waitTargets[index],
				probeIndexes[index],
				measureStart.Add(offset),
				index,
			)
		}
	}
}

func TestRunMatchmakingProbesCancellationJoinsPendingWait(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	entered := make(chan struct{})
	probeCalled := make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() {
		done <- runMatchmakingProbes(
			ctx,
			time.Unix(1_000, 0),
			[]time.Duration{10 * time.Second},
			1,
			func(waitCtx context.Context, _ time.Time) error {
				close(entered)
				<-waitCtx.Done()
				return waitCtx.Err()
			},
			func(_ context.Context, _ int) error {
				probeCalled <- struct{}{}
				return nil
			},
		)
	}()
	<-entered
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	select {
	case <-probeCalled:
		t.Fatal("probe ran after cancellation released its pending wait")
	default:
	}
}
