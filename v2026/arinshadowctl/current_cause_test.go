package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	connectserver "github.com/urnetwork/server/v2026/connect"
)

func currentCauseTestConfig(t *testing.T) (currentCauseConfig, hostCaptureInventory) {
	t.Helper()
	old, inventory := captureTestConfig(t)
	// Unavailable unrelated owners do not turn one explicitly selected,
	// qualified Connect endpoint into a claim of full-fleet completeness.
	inventory.Complete = false
	inventory.Processes[1].Endpoint = nil
	inventory.Processes[1].Reason = "unavailable"
	ref := old.HostInventories[0]
	ref.SHA256 = captureTestWrite(t, ref.Path, inventory)
	return currentCauseConfig{RunId: old.RunId, KeyFile: filepath.Join(filepath.Dir(ref.Path), "key"),
		NotBefore: old.NotBefore, NotAfter: old.NotAfter, Revision: inventory.Processes[0].Endpoint.Identity.Revision,
		InputPath: filepath.Join(filepath.Dir(ref.Path), "input.json"), InputSHA256: strings.Repeat("b", 64),
		Endpoints: []currentCauseEndpointRef{{Inventory: ref, ProcessNonce: inventory.Processes[0].Endpoint.Identity.ProcessNonce}},
		Bridges:   old.Bridges}, inventory
}

func TestCurrentCauseEndpointsBindOnlyExplicitConnectOwner(t *testing.T) {
	config, _ := currentCauseTestConfig(t)
	endpoints, commands, err := currentCauseEndpoints(config, server.NowUtc(), readProtectedCaptureFile)
	if err != nil || len(endpoints) != 1 || endpoints[0].Identity.Role != "connect" || endpoints[0].Socket != "" || endpoints[0].Bridge != "host" || len(commands) != 1 {
		t.Fatal("bounded selected process required an unrelated native owner", err)
	}
	for _, which := range []string{"wrong_hash", "missing_nonce", "wrong_revision", "duplicate_process", "native_process", "old_inventory", "ambiguous_nonce", "extra_bridge"} {
		t.Run(which, func(t *testing.T) {
			c, inventory := currentCauseTestConfig(t)
			switch which {
			case "wrong_hash":
				c.Endpoints[0].Inventory.SHA256 = strings.Repeat("a", 64)
			case "missing_nonce":
				c.Endpoints[0].ProcessNonce = server.NewId()
			case "wrong_revision":
				c.Revision = strings.Repeat("d", 40)
			case "duplicate_process":
				c.Endpoints = append(c.Endpoints, c.Endpoints[0])
			case "native_process":
				inventory.Processes[0].Container.Service = "taskworker"
			case "old_inventory":
				inventory.StartedAt = inventory.StartedAt.Add(-10 * time.Minute)
				inventory.FinishedAt = inventory.FinishedAt.Add(-10 * time.Minute)
			case "ambiguous_nonce":
				inventory.Processes = append(inventory.Processes, inventory.Processes[0])
			case "extra_bridge":
				c.Bridges = append(c.Bridges, captureBridge{Name: "extra", Argv: []string{"/usr/bin/false"}})
			}
			if which == "native_process" || which == "old_inventory" || which == "ambiguous_nonce" {
				c.Endpoints[0].Inventory.SHA256 = captureTestWrite(t, c.Endpoints[0].Inventory.Path, inventory)
			}
			if _, _, err := currentCauseEndpoints(c, server.NowUtc(), readProtectedCaptureFile); err == nil {
				t.Fatal("unbound endpoint accepted")
			}
		})
	}
}

func TestCurrentCauseCallerBoundsBeforeProtectedReads(t *testing.T) {
	config, _ := currentCauseTestConfig(t)
	for len(config.Endpoints) < 33 {
		config.Endpoints = append(config.Endpoints, config.Endpoints[0])
	}
	reads := 0
	_, _, err := currentCauseEndpoints(config, server.NowUtc(), func(string, int64) ([]byte, error) { reads++; return nil, invalid })
	if err == nil || reads != 0 {
		t.Fatal("oversized endpoint scope reached protected file reader")
	}
	input := currentCauseInput{ExpectedEpoch: 1791310718, LookupNotBefore: server.NowUtc().Add(-time.Hour)}
	for i := 0; i < 65; i++ {
		input.Entries = append(input.Entries, currentCauseEntry{server.NewId(), server.NewId(), server.NewId()})
	}
	calls := 0
	_, err = collectCurrentCauses(context.Background(), input, []captureEndpoint{{}}, func(captureEndpoint) (*server.ArinShadowRPCClient, error) { calls++; return nil, invalid })
	if err == nil || calls != 0 {
		t.Fatal("oversized sample reached endpoint opener")
	}
}

func TestCurrentCauseCallerAuthenticatedCausesAndUnknownOwners(t *testing.T) {
	for _, which := range []string{"one_qualified", "duplicate_handler", "wrong_reader", "absent_method", "wrong_process"} {
		t.Run(which, func(t *testing.T) {
			config, inventory := currentCauseTestConfig(t)
			input := currentCauseInput{ExpectedEpoch: 1791310718, LookupNotBefore: server.NowUtc().Add(-time.Hour),
				Entries: []currentCauseEntry{{server.NewId(), server.NewId(), server.NewId()}, {server.NewId(), server.NewId(), server.NewId()}}}
			first := input.Entries[0]
			endpoint := *inventory.Processes[0].Endpoint
			endpoints := []captureEndpoint{endpoint}
			if which == "duplicate_handler" {
				other := endpoint
				other.Identity.ProcessNonce = server.NewId()
				endpoints = append(endpoints, other)
			}
			var key [32]byte
			key[0] = 1
			inventoryCalls, causeCalls := 0, 0
			open := func(endpoint captureEndpoint) (*server.ArinShadowRPCClient, error) {
				identity := endpoint.Identity
				if which == "wrong_process" {
					identity.ProcessNonce = server.NewId()
				}
				service, err := server.NewArinShadowRPCService(context.Background(), config.RunId, key, identity,
					func(ctx context.Context, method string, raw json.RawMessage) (any, error) {
						switch method {
						case server.ArinCurrentCauseInventoryMethod:
							inventoryCalls++
							if which == "absent_method" {
								return nil, invalid
							}
							epoch := input.ExpectedEpoch
							if which == "wrong_reader" {
								epoch--
							}
							return connectserver.ArinCurrentCauseHandlerInventory{Handlers: []server.Id{first.HandlerId}, Complete: true, ReaderEpoch: epoch, TrackedConnections: 1}, nil
						case server.ArinCurrentCauseMethod:
							causeCalls++
							var request server.ArinCurrentCauseRequest
							if server.DecodeArinShadowRPC(raw, &request) != nil || len(request.Connections) != 1 || request.Connections[0] != first.ConnectionId {
								return nil, invalid
							}
							now := server.NowUtc()
							cause := &server.ArinCurrentCause{AddressFamily: "ipv4", DatabaseBuildEpoch: input.ExpectedEpoch, State: "unknown", NonQuality: true, RegistrationAttribution: "unavailable", OriginAttribution: "absent"}
							return server.ArinCurrentCauseReply{ExpectedEpoch: input.ExpectedEpoch, LookupNotBefore: input.LookupNotBefore,
								Rows: []server.ArinCurrentCauseRow{{ConnectionId: first.ConnectionId, ClientId: first.ClientId, HandlerId: first.HandlerId,
									ActualAt: now.Add(-time.Second), ObservedAt: now, CapturedAt: now, Reason: "qualified", Cause: cause}}}, nil
						default:
							t.Fatal("legacy capture or unexpected method invoked", method)
							return nil, invalid
						}
					})
				if err != nil {
					return nil, err
				}
				return server.NewArinShadowRPCClient(config.RunId, key, identity, service.Handle)
			}
			report, err := collectCurrentCauses(context.Background(), input, endpoints, open)
			if err != nil || report.Report.RequestedConnections != 2 || report.PublicationBindingValidated || report.FleetCoverageClaimed {
				t.Fatal("sample denominator or scope lost", err)
			}
			if which == "one_qualified" {
				if report.Report.QualifiedConnections != 1 || report.Report.Reasons["owner_unavailable"] != 1 || inventoryCalls != 1 || causeCalls != 1 {
					t.Fatal("expected one cause and one retained unknown", report)
				}
			} else if report.Report.QualifiedConnections != 0 || report.Report.Reasons["owner_unavailable"] != 2 || causeCalls != 0 {
				t.Fatal("uncertain owner became an attributed cause", report)
			}
			if which == "duplicate_handler" && report.DuplicateRequestedHandlers != 1 {
				t.Fatal("ambiguous ownership not retained")
			}
			data, err := json.Marshal(report)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range input.Entries {
				for _, id := range []server.Id{entry.ClientId, entry.ConnectionId, entry.HandlerId} {
					encoded, _ := json.Marshal(id)
					if strings.Contains(string(data), string(encoded)) {
						t.Fatal("private key escaped public report")
					}
				}
			}
		})
	}
}
