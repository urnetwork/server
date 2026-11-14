package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// GET /health serves the stored status as the site reads it, cacheable for
// thirty seconds, and never says who set it.
func TestHealthServesTheStatus(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		model.Testing_ResetNetworkHealthCache()
		defer model.Testing_ResetNetworkHealthCache()
		get := func() (http.Header, map[string]any) {
			recorder := httptest.NewRecorder()
			Health(recorder, httptest.NewRequest("GET", "/health", nil))
			if recorder.Code != http.StatusOK {
				t.Fatalf("status %d", recorder.Code)
			}
			var body map[string]any
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			return recorder.Header(), body
		}

		header, body := get()
		if body["status"] != "healthy" || len(body) != 1 {
			t.Fatalf("healthy: %v", body)
		}
		if header.Get("Cache-Control") != "public, max-age=30" || header.Get("Content-Type") != "application/json" {
			t.Fatalf("healthy: %v", header)
		}

		if _, err := model.SetNetworkHealth(ctx, model.NetworkHealthPartial, "Some connections fail to start.", model.NetworkHealthSourceOperator); err != nil {
			t.Fatal(err)
		}
		defer model.ClearNetworkHealth(ctx)
		header, body = get()
		if body["status"] != "partial" || body["reason"] != "Some connections fail to start." || body["since"] == nil || body["updated"] == nil {
			t.Fatalf("partial: %v", body)
		}
		if _, ok := body["source"]; ok || len(body) != 4 {
			t.Fatalf("partial carries a field it must not: %v", body)
		}
		if header.Get("Cache-Control") != "public, max-age=30" {
			t.Fatalf("partial: %v", header)
		}

		server.Redis(ctx, func(client server.RedisClient) {
			server.Raise(client.Set(ctx, "network_health:status:v1", "not json", 0).Err())
		})
		model.Testing_ResetNetworkHealthCache()
		header, body = get()
		if body["status"] != "unknown" || len(body) != 1 || header.Get("Cache-Control") != "no-store" {
			t.Fatalf("unknown: %v %v", header, body)
		}
	})
}
