// The authenticated public handler preserves the model's two-class progress
// contract and published place, using only generated synthetic providers.
package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

func TestProviderBlackholeDueServesFirstChecksBesideRetries(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		const secret = "synthetic-blackhole-operator-secret"
		defer withStubOperatorIngestSecret(secret)()
		ctx := context.Background()
		now := server.NowUtc()
		city := &model.Location{LocationType: model.LocationTypeCity, City: "Synthetic Harbor", Region: "Synthetic Region", Country: "United States", CountryCode: "us"}
		model.CreateLocation(ctx, city)
		var retries, first []server.Id
		for index := range 6 {
			id := server.NewId()
			testing_connectDueProvider(t, ctx, id, city.LocationId, fmt.Sprintf("192.0.2.%d:0", index+1))
			if index < 3 {
				at := now.Add(-time.Hour)
				due := now.Add(-time.Minute)
				model.SetProviderBlackholeCheck(ctx, &model.ProviderBlackholeCheck{ClientId: id, CheckedAt: at, OK: false,
					Failure: "all_destinations_failed", ConsecutiveFailures: 1, FirstFailedAt: &at, NextDueAt: &due})
				retries = append(retries, id)
			} else {
				first = append(first, id)
			}
		}
		model.UpdateClientLocationReliabilities(ctx, now.Add(-time.Hour), now)
		req := httptest.NewRequest(http.MethodGet, "/network/provider-blackhole-check-due?limit=2", nil)
		req.Header.Set(operatorSecretHeader, secret)
		response := httptest.NewRecorder()
		ProviderBlackholeCheckDue(response, req)
		var result ProviderEgressLocationDueResult
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &result) != nil {
			t.Fatalf("due handler failed: status=%d", response.Code)
		}
		ids := testDueClientIds(result)
		if len(ids) != 2 || !slices.Contains(retries, ids[0]) || !slices.Contains(first, ids[1]) {
			t.Fatalf("authenticated handler starved first checks: selected=%v", ids)
		}
	})
}

func TestProviderBlackholeDueFairnessKeepsAuthAndInputGates(t *testing.T) {
	const secret = "synthetic-blackhole-operator-secret"
	defer withStubOperatorIngestSecret(secret)()
	for _, testCase := range []struct {
		query, supplied string
		status          int
	}{
		{query: "?limit=2", supplied: "", status: http.StatusUnauthorized},
		{query: "?limit=2", supplied: "synthetic-wrong-secret", status: http.StatusUnauthorized},
		{query: "?limit=0", supplied: secret, status: http.StatusBadRequest},
		{query: "?limit=-1", supplied: secret, status: http.StatusBadRequest},
		{query: "?limit=2&shard_count=0", supplied: secret, status: http.StatusBadRequest},
		{query: "?limit=2&shard_count=4&shard_index=4", supplied: secret, status: http.StatusBadRequest},
	} {
		request := httptest.NewRequest(http.MethodGet, "/network/provider-blackhole-check-due"+testCase.query, nil)
		request.Header.Set(operatorSecretHeader, testCase.supplied)
		response := httptest.NewRecorder()
		ProviderBlackholeCheckDue(response, request)
		if response.Code != testCase.status {
			t.Fatalf("query=%q status=%d want=%d", testCase.query, response.Code, testCase.status)
		}
	}
}
