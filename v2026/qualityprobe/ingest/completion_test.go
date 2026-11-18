package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026/qualityprobe"
)

// Retrying a failed acknowledgement sends identical claim/time bytes, not a
// new completion generated from the HTTP retry's wall clock.
func TestUrlCompletedIngestReplayPreservesWireIdentity(t *testing.T) {
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	completion := qualityprobe.UrlProbeCompletion{ClientId: "00000000-0000-0000-0000-000000000001",
		ClaimOrdinal: 17, CompletedAt: at, ProbeFailure: strings.Repeat("é", 40), AllowPacing: true}
	var stateLock sync.Mutex
	bodies := [][]byte{}
	endpoint := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Error(err)
			return
		}
		stateLock.Lock()
		bodies = append(bodies, body)
		count := len(bodies)
		stateLock.Unlock()
		if request.Method != http.MethodPost || request.URL.Path != "/network/provider-egress-attempt" || request.Header.Get("X-UR-Operator-Secret") != "synthetic-secret" {
			t.Error("completion used a different authenticated endpoint contract")
		}
		if count == 1 {
			writer.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(writer).Encode(map[string]any{"claim_ordinal": 17, "attempt_at": at, "received_at": at, "replay": true})
	}))
	defer endpoint.Close()
	client := &Client{ServerUrl: endpoint.URL, OperatorSecret: "synthetic-secret", Http: endpoint.Client()}
	if err := client.ReportUrlProbeCompletion(context.Background(), completion); !errors.Is(err, ErrRejected) {
		t.Fatalf("failed acknowledgement was hidden: %v", err)
	}
	if err := client.ReportUrlProbeCompletion(context.Background(), completion); err != nil {
		t.Fatal(err)
	}
	stateLock.Lock()
	defer stateLock.Unlock()
	if len(bodies) != 2 || !bytes.Equal(bodies[0], bodies[1]) {
		t.Fatalf("acknowledgement retry changed identity bytes: %q", bodies)
	}
	var reported qualityprobe.UrlProbeCompletion
	if err := json.Unmarshal(bodies[0], &reported); err != nil {
		t.Fatal(err)
	}
	if reported.ClaimOrdinal != completion.ClaimOrdinal || !reported.CompletedAt.Equal(at) || !reported.AllowPacing || len(reported.ProbeFailure) != 64 || len(completion.ProbeFailure) != 80 {
		t.Fatalf("wire receipt lost identity or mutated caller input: %+v", reported)
	}
}

// A legacy200 cannot certify a durable count, and maintenance backlog is not
// an empty schedule or permission to bypass admission through old enumeration.
func TestUrlCompletedIngestRequiresNewAcknowledgement(t *testing.T) {
	for _, response := range []string{
		`{"attempt_at":"2026-01-02T03:04:05Z"}`,
		`{"claim_ordinal":18,"attempt_at":"2026-01-02T03:04:05Z","received_at":"2026-01-02T03:04:05Z"}`,
		`{"claim_ordinal":17,"attempt_at":"2026-01-02T03:04:05Z"}`,
	} {
		endpoint := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			_, _ = writer.Write([]byte(response))
		}))
		client := &Client{ServerUrl: endpoint.URL, Http: endpoint.Client()}
		err := client.ReportUrlProbeCompletion(context.Background(), qualityprobe.UrlProbeCompletion{
			ClientId: "synthetic-provider", ClaimOrdinal: 17, CompletedAt: time.Now().UTC(), AllowPacing: true,
		})
		endpoint.Close()
		if !errors.Is(err, qualityprobe.ErrUrlProbeCompletionUnsupported) {
			t.Fatalf("unsupported completion response counted as acknowledged: %s error=%v", response, err)
		}
	}
	endpoint := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = writer.Write([]byte(`{"providers":[],"completed_run_priority_ready":true,"priority_maintenance_pending":true}`))
	}))
	defer endpoint.Close()
	client := &Client{ServerUrl: endpoint.URL, Http: endpoint.Client()}
	providers, err := client.Due(context.Background(), 10)
	if !errors.Is(err, ErrDuePriorityMaintenancePending) || errors.Is(err, ErrDueUnsupported) || len(providers) != 0 {
		t.Fatalf("maintenance became empty/legacy schedule: providers=%+v error=%v", providers, err)
	}
}

func TestUrlCompletedIngestDueKeepsZeroDistinctFromUnknown(t *testing.T) {
	endpoint := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = writer.Write([]byte(`{"providers":[
			{"client_id":"zero","claim_ordinal":5,"claimed_at":"2026-01-02T03:04:05Z","completed_run_count":0},
			{"client_id":"unknown","claim_ordinal":6,"claimed_at":"2026-01-02T03:04:05Z"}]}`))
	}))
	defer endpoint.Close()
	client := &Client{ServerUrl: endpoint.URL, Http: endpoint.Client()}
	providers, err := client.Due(context.Background(), 2)
	if err != nil || len(providers) != 2 || providers[0].CompletedRunCount == nil || *providers[0].CompletedRunCount != 0 ||
		providers[1].CompletedRunCount != nil || providers[0].ClaimOrdinal != 5 || providers[0].ClaimedAt.IsZero() {
		t.Fatalf("wire due count lost zero/unknown or claim identity: providers=%+v error=%v", providers, err)
	}
}
