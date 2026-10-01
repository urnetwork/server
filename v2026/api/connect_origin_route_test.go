package api

// Exercise the real authenticated control route and committed origin writer.
// A successful contract must use the router's owner; the legacy bare route
// remains a safe no-notifier control rather than creating a hidden singleton.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/jwt"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/router"
	"google.golang.org/protobuf/proto"
)

func TestConnectControlOwnedRouterPublishesCommittedOrigins(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		networkId, userId := server.NewId(), server.NewId()
		sourceId, deviceId, destinationId := server.NewId(), server.NewId(), server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "origin-route-"+networkId.String(), userId)
		model.Testing_CreateDevice(ctx, networkId, deviceId, sourceId, "origin-route-source", "test")
		model.Testing_CreateDevice(ctx, networkId, server.NewId(), destinationId, "origin-route-destination", "test")
		model.SetProvide(ctx, destinationId, map[model.ProvideMode][]byte{model.ProvideModeNetwork: bytes.Repeat([]byte{1}, 32)})
		token := jwt.NewByJwt(networkId, userId, "origin-route", false, false).Client(deviceId, sourceId).Sign()
		owned, closeOwned, err := NewRouter(ctx, ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer closeOwned()
		bare := router.NewRouter(ctx, Routes())
		counter := func(event string) float64 {
			metrics, err := prometheus.DefaultGatherer.Gather()
			if err != nil {
				t.Fatal(err)
			}
			for _, family := range metrics {
				if family.GetName() != "urnetwork_contract_origin_notifications_total" {
					continue
				}
				for _, metric := range family.Metric {
					for _, label := range metric.Label {
						if label.GetName() == "event" && label.GetValue() == event {
							return metric.GetCounter().GetValue()
						}
					}
				}
			}
			t.Fatalf("notification metric event %q is absent", event)
			return 0
		}
		for _, testCase := range []struct {
			handler http.Handler
			event   string
		}{{handler: owned, event: "enqueued"}, {handler: bare, event: "unowned"}} {
			frame, err := connect.ToFrame(&protocol.CreateContract{DestinationId: destinationId.Bytes(), TransferByteCount: 1024 * 1024}, connect.DefaultProtocolVersion)
			if err != nil {
				t.Fatal(err)
			}
			packBytes, err := proto.Marshal(&protocol.Pack{Frames: []*protocol.Frame{frame}})
			connect.MessagePoolReturn(frame.MessageBytes)
			if err != nil {
				t.Fatal(err)
			}
			body, err := json.Marshal(&controller.ConnectControlArgs{Pack: base64.StdEncoding.EncodeToString(packBytes)})
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodPost, "/connect/control", bytes.NewReader(body)).WithContext(ctx)
			request.RemoteAddr = "192.0.2.1:4000"
			request.Header.Set("Authorization", "Bearer "+token)
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			before := counter(testCase.event)
			testCase.handler.ServeHTTP(response, request)
			if response.Code != http.StatusOK {
				t.Fatalf("%s control route status=%d", testCase.event, response.Code)
			}
			var result controller.ConnectControlResult
			if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || result.Error != nil {
				t.Fatalf("%s control response failed: decode=%v", testCase.event, err)
			}
			responseBytes, err := base64.StdEncoding.DecodeString(result.Pack)
			if err != nil {
				t.Fatal(err)
			}
			var responsePack protocol.Pack
			if err := proto.Unmarshal(responseBytes, &responsePack); err != nil || len(responsePack.Frames) != 1 {
				t.Fatalf("%s control response has no single result: decode=%v", testCase.event, err)
			}
			message, err := connect.FromFrame(responsePack.Frames[0])
			if err != nil {
				t.Fatal(err)
			}
			contract, ok := message.(*protocol.CreateContractResult)
			if !ok || contract.Error != nil || contract.Contract == nil {
				t.Fatalf("%s control route did not create a contract", testCase.event)
			}
			if after := counter(testCase.event); after != before+1 {
				t.Fatalf("%s control route did not use its notification boundary: delta=%v", testCase.event, after-before)
			}
		}
	})
}
