package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/urnetwork/server"
	connectserver "github.com/urnetwork/server/connect"
)

// This command only resolves the explicitly selected Connect processes. It
// does not require a publisher endpoint or infer whole-fleet coverage from a
// subset. The owning carrier validates the fixed publication pair before and
// after this command; this projection alone makes no publication claim.
type currentCauseEndpointRef struct {
	Inventory    captureInventoryFile `json:"inventory"`
	ProcessNonce server.Id            `json:"process_nonce"`
}

type currentCauseConfig struct {
	RunId       server.Id                 `json:"run_id"`
	KeyFile     string                    `json:"key_file"`
	NotBefore   time.Time                 `json:"not_before"`
	NotAfter    time.Time                 `json:"not_after"`
	Revision    string                    `json:"revision"`
	InputPath   string                    `json:"input_path"`
	InputSHA256 string                    `json:"input_sha256"`
	Endpoints   []currentCauseEndpointRef `json:"endpoints"`
	Bridges     []captureBridge           `json:"bridges"`
}

type currentCauseEntry struct {
	ClientId     server.Id `json:"client_id"`
	ConnectionId server.Id `json:"connection_id"`
	HandlerId    server.Id `json:"handler_id"`
}

type currentCauseInput struct {
	ExpectedEpoch   int64               `json:"expected_epoch"`
	LookupNotBefore time.Time           `json:"lookup_not_before"`
	Entries         []currentCauseEntry `json:"entries"`
}

type currentCauseOutput struct {
	Report                      server.ArinCurrentCauseAggregate `json:"report"`
	RequestedProcesses          int                              `json:"requested_processes"`
	QualifiedInventories        int                              `json:"qualified_inventories"`
	UnavailableInventories      int                              `json:"unavailable_inventories"`
	DuplicateRequestedHandlers  int                              `json:"duplicate_requested_handlers"`
	BridgeStarts                int                              `json:"bridge_starts"`
	PeakConcurrentBridges       int                              `json:"peak_concurrent_bridges"`
	ExpectedEpoch               int64                            `json:"expected_epoch"`
	LookupNotBefore             time.Time                        `json:"lookup_not_before"`
	StartedAt                   time.Time                        `json:"started_at"`
	CompletedAt                 time.Time                        `json:"completed_at"`
	PublicationBindingValidated bool                             `json:"publication_binding_validated"`
	FleetCoverageClaimed        bool                             `json:"fleet_coverage_claimed"`
}

func currentCauseHex(value string, bytes int) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == bytes && value == strings.ToLower(value)
}

func validateCurrentCauseInput(input currentCauseInput, now time.Time) error {
	if input.ExpectedEpoch <= 0 || input.LookupNotBefore.Before(time.Unix(input.ExpectedEpoch, 0)) || input.LookupNotBefore.After(now.Add(2*time.Second)) || len(input.Entries) == 0 || len(input.Entries) > server.ArinCurrentCauseLimit {
		return invalid
	}
	clients, connections := map[server.Id]bool{}, map[server.Id]bool{}
	for _, entry := range input.Entries {
		if entry.ClientId == (server.Id{}) || entry.ConnectionId == (server.Id{}) || entry.HandlerId == (server.Id{}) || clients[entry.ClientId] || connections[entry.ConnectionId] {
			return invalid
		}
		clients[entry.ClientId], connections[entry.ConnectionId] = true, true
	}
	return nil
}

// Exact process selection is authoritative, not the complete bit of a fleet
// inventory. Other unqualified or absent owners remain unknown to this sample.
func currentCauseEndpoints(c currentCauseConfig, now time.Time, read func(string, int64) ([]byte, error)) ([]captureEndpoint, map[string][]string, error) {
	if read == nil || c.RunId == (server.Id{}) || !currentCauseHex(c.Revision, 20) || c.NotBefore.IsZero() || now.Before(c.NotBefore) || !now.Before(c.NotAfter) || !c.NotBefore.Before(c.NotAfter) || len(c.Endpoints) == 0 || len(c.Endpoints) > 32 || len(c.Bridges) > 8 || !filepath.IsAbs(c.KeyFile) || !filepath.IsAbs(c.InputPath) || !currentCauseHex(c.InputSHA256, 32) {
		return nil, nil, invalid
	}
	commands := map[string][]string{}
	for _, bridge := range c.Bridges {
		if bridge.Name == "" || len(bridge.Name) > 64 || commands[bridge.Name] != nil || len(bridge.Argv) == 0 || len(bridge.Argv) > 64 || !filepath.IsAbs(bridge.Argv[0]) {
			return nil, nil, invalid
		}
		commands[bridge.Name] = bridge.Argv
	}
	usedBridges := map[string]bool{}
	seenNonces, seenContainers := map[server.Id]bool{}, map[string]bool{}
	endpoints := []captureEndpoint{}
	for _, requested := range c.Endpoints {
		ref := requested.Inventory
		if requested.ProcessNonce == (server.Id{}) || seenNonces[requested.ProcessNonce] || ref.Host == "" || len(ref.Host) > 255 || !filepath.IsAbs(ref.Path) || !currentCauseHex(ref.SHA256, 32) || (ref.Bridge != "" && commands[ref.Bridge] == nil) {
			return nil, nil, invalid
		}
		data, err := read(ref.Path, 128<<10)
		hash := sha256.Sum256(data)
		if err != nil || hex.EncodeToString(hash[:]) != ref.SHA256 {
			return nil, nil, invalid
		}
		var inventory hostCaptureInventory
		if decode(data, &inventory) != nil || inventory.Hostname != ref.Host || inventory.Environment != "main" || inventory.StartedAt.IsZero() || inventory.FinishedAt.Before(inventory.StartedAt) || inventory.FinishedAt.Sub(inventory.StartedAt) > 30*time.Second || inventory.FinishedAt.After(now.Add(2*time.Second)) || now.Sub(inventory.FinishedAt) > 5*time.Minute || len(inventory.Processes) == 0 || len(inventory.Processes) > 64 {
			return nil, nil, invalid
		}
		var found *captureEndpoint
		for _, process := range inventory.Processes {
			if process.Endpoint == nil || process.Endpoint.Identity.ProcessNonce != requested.ProcessNonce {
				continue
			}
			row, endpoint := process.Container, *process.Endpoint
			identity := endpoint.Identity
			if found != nil || process.Reason != "qualified" || !currentCauseHex(process.ExecutableSHA256, 32) || !currentCauseHex(row.ID, 32) || !strings.HasPrefix(row.Image, "sha256:") || !currentCauseHex(strings.TrimPrefix(row.Image, "sha256:"), 32) || seenContainers[row.ID] || row.PID <= 0 || row.Environment != "main" || row.Service != "connect" || row.Block == "" || row.Version == "" || row.StartedAt.IsZero() ||
				identity.Role != "connect" || identity.Host != ref.Host || identity.Environment != "main" || identity.Block != row.Block || identity.ImageDigest != row.Image || identity.Revision != c.Revision || identity.Modified || strings.ReplaceAll(identity.Version, "+", "-") != row.Version || identity.StartedAt.Before(row.StartedAt.Add(-2*time.Second)) || identity.StartedAt.After(inventory.FinishedAt.Add(2*time.Second)) ||
				endpoint.Bridge != "" || !filepath.IsAbs(endpoint.Socket) || filepath.Clean(endpoint.Socket) != endpoint.Socket {
				return nil, nil, invalid
			}
			seenContainers[row.ID] = true
			if ref.Bridge != "" {
				endpoint.Socket, endpoint.Bridge = "", ref.Bridge
				usedBridges[ref.Bridge] = true
			}
			found = &endpoint
		}
		if found == nil {
			return nil, nil, invalid
		}
		seenNonces[requested.ProcessNonce] = true
		endpoints = append(endpoints, *found)
	}
	if len(usedBridges) != len(commands) {
		return nil, nil, invalid
	}
	return endpoints, commands, nil
}

func collectCurrentCauses(ctx context.Context, input currentCauseInput, endpoints []captureEndpoint, open func(captureEndpoint) (*server.ArinShadowRPCClient, error)) (currentCauseOutput, error) {
	out := currentCauseOutput{RequestedProcesses: len(endpoints), ExpectedEpoch: input.ExpectedEpoch, LookupNotBefore: input.LookupNotBefore, StartedAt: server.NowUtc()}
	if ctx == nil || ctx.Err() != nil || validateCurrentCauseInput(input, out.StartedAt) != nil || len(endpoints) == 0 || len(endpoints) > 32 || open == nil {
		return out, invalid
	}
	bounded, cancel := context.WithTimeout(ctx, server.ArinShadowCaptureMaxAge)
	defer cancel()
	wanted := map[server.Id]bool{}
	entries := []server.ArinCurrentCauseSampleEntry{}
	for _, entry := range input.Entries {
		wanted[entry.HandlerId] = true
		entries = append(entries, server.ArinCurrentCauseSampleEntry{ClientId: entry.ClientId, ConnectionId: entry.ConnectionId, HandlerId: entry.HandlerId})
	}
	handlers := map[server.Id]*server.ArinShadowRPCClient{}
	duplicates := map[server.Id]bool{}
	for _, endpoint := range endpoints {
		out.UnavailableInventories++
		if bounded.Err() != nil {
			continue
		}
		client, err := open(endpoint)
		if err != nil || client == nil || client.Identity() != endpoint.Identity {
			continue
		}
		var inventory connectserver.ArinCurrentCauseHandlerInventory
		if client.Call(bounded, server.ArinCurrentCauseInventoryMethod, struct{}{}, &inventory) != nil || !inventory.Complete || inventory.ReaderEpoch != input.ExpectedEpoch || len(inventory.Handlers) > 256 || inventory.TrackedConnections < 0 || inventory.OverflowConnections != 0 {
			continue
		}
		seen := map[server.Id]bool{}
		valid := true
		for _, id := range inventory.Handlers {
			if id == (server.Id{}) || seen[id] {
				valid = false
				break
			}
			seen[id] = true
		}
		if !valid {
			continue
		}
		out.UnavailableInventories--
		out.QualifiedInventories++
		for _, id := range inventory.Handlers {
			if !wanted[id] || duplicates[id] {
				continue
			}
			if handlers[id] != nil {
				duplicates[id] = true
				delete(handlers, id)
				continue
			}
			handlers[id] = client
		}
	}
	out.DuplicateRequestedHandlers = len(duplicates)
	var err error
	if bounded.Err() != nil {
		// Preserve the requested-key denominator even if inventory exhausted the
		// deadline. No extra callback or network operation is performed.
		out.Report = server.ArinCurrentCauseAggregate{RequestedConnections: len(entries), Reasons: map[string]int{"owner_unavailable": len(entries)}, Causes: []server.ArinCurrentCauseCount{}}
	} else {
		out.Report, err = server.CollectArinCurrentCauseSample(bounded, entries, handlers, input.ExpectedEpoch, input.LookupNotBefore)
	}
	out.CompletedAt = server.NowUtc()
	return out, err
}

func runCurrentCause(output io.Writer, args []string) error {
	flags := flag.NewFlagSet("current-cause", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	path := flags.String("config", "", "")
	if flags.Parse(args) != nil || flags.NArg() != 0 {
		return invalid
	}
	data, err := readProtectedCaptureFile(*path, 128<<10)
	var config currentCauseConfig
	if err != nil || decode(data, &config) != nil {
		return invalid
	}
	endpoints, commands, err := currentCauseEndpoints(config, server.NowUtc(), readProtectedCaptureFile)
	if err != nil {
		return invalid
	}
	data, err = readProtectedCaptureFile(config.InputPath, 32<<10)
	hash := sha256.Sum256(data)
	var input currentCauseInput
	if err != nil || hex.EncodeToString(hash[:]) != config.InputSHA256 || decode(data, &input) != nil || validateCurrentCauseInput(input, server.NowUtc()) != nil {
		return invalid
	}
	keyBytes, err := readProtectedCaptureFile(config.KeyFile, 32)
	if err != nil || len(keyBytes) != 32 {
		return invalid
	}
	var key [32]byte
	copy(key[:], keyBytes)
	ctx, cancel := context.WithDeadline(context.Background(), config.NotAfter)
	defer cancel()
	ctx, stop := context.WithTimeout(ctx, server.ArinShadowCaptureMaxAge)
	defer stop()
	pool, err := server.NewArinShadowRPCPipePool(ctx, commands)
	if err != nil {
		return invalid
	}
	defer pool.Close()
	open := func(endpoint captureEndpoint) (*server.ArinShadowRPCClient, error) {
		transport := server.ArinShadowUnixRoundTrip(endpoint.Socket)
		if endpoint.Bridge != "" {
			transport = pool.RoundTrip(endpoint.Bridge)
		}
		return server.NewArinShadowRPCClient(config.RunId, key, endpoint.Identity, transport)
	}
	report, err := collectCurrentCauses(ctx, input, endpoints, open)
	pool.Close()
	report.BridgeStarts, report.PeakConcurrentBridges = pool.Counts()
	if err != nil || json.NewEncoder(output).Encode(report) != nil {
		return invalid
	}
	return nil
}
