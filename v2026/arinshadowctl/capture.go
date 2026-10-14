package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"flag"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/urnetwork/server/v2026"
	connectserver "github.com/urnetwork/server/v2026/connect"
	"github.com/urnetwork/server/v2026/model"
)

type captureEndpoint struct {
	Identity server.ArinShadowRPCIdentity `json:"identity"`
	Socket   string                       `json:"socket,omitempty"`
	Bridge   string                       `json:"bridge,omitempty"`
}

type captureBridge struct {
	Name string   `json:"name"`
	Argv []string `json:"argv"`
}

const captureOperatorTimeout = server.ArinShadowRPCBridgeMaxAge

type captureConfig struct {
	RunId             server.Id              `json:"run_id"`
	KeyFile           string                 `json:"key_file"`
	NotBefore         time.Time              `json:"not_before"`
	NotAfter          time.Time              `json:"not_after"`
	InventoryComplete bool                   `json:"inventory_complete"`
	InventorySHA256   string                 `json:"inventory_sha256"`
	Expected          []captureExpectedSlot  `json:"expected_slots"`
	HostInventories   []captureInventoryFile `json:"host_inventories"`
	Native            []captureEndpoint      `json:"native"`
	Connect           []captureEndpoint      `json:"connect"`
	Bridges           []captureBridge        `json:"bridges"`
	ActivePath        string                 `json:"active_path"`
	ActiveSHA256      string                 `json:"active_sha256"`
	CandidatePath     string                 `json:"candidate_path"`
	CandidateSHA256   string                 `json:"candidate_sha256"`
	RequiredBuckets   []string               `json:"required_buckets"`
}

// A protected operator config is the exact endpoint/command authority. This
// command never discovers an arbitrary host or expands an endpoint from SQL.
// Inventory completeness is attested separately; every durable source key must
// still join a current handler or remain an explicit unknown in the report.
func runCaptureCurrent(output io.Writer, args []string) error {
	flags := flag.NewFlagSet("capture-current", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	path := flags.String("config", "", "")
	if flags.Parse(args) != nil || flags.NArg() != 0 {
		return invalid
	}
	data, err := readProtectedCaptureFile(*path, 256<<10)
	if err != nil {
		return invalid
	}
	var config captureConfig
	if decode(data, &config) != nil || !config.InventoryComplete || len(config.InventorySHA256) != 64 || len(config.Connect) == 0 || len(config.Connect) > 256 || len(config.Native) == 0 || len(config.Native) > 16 || len(config.Bridges) > 8 || time.Now().Before(config.NotBefore) || !time.Now().Before(config.NotAfter) {
		return invalid
	}
	if _, err := hex.DecodeString(config.InventorySHA256); err != nil {
		return invalid
	}
	if err := validateCaptureInventory(config, time.Now()); err != nil {
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
	ctx, stop := context.WithTimeout(ctx, captureOperatorTimeout)
	defer stop()
	operatorStarted := time.Now()
	setup, stopSetup := context.WithTimeout(ctx, server.ArinShadowCaptureMaxAge)
	defer stopSetup()
	commands := map[string][]string{}
	for _, bridge := range config.Bridges {
		if bridge.Name == "" || commands[bridge.Name] != nil {
			return invalid
		}
		commands[bridge.Name] = bridge.Argv
	}
	pool, err := server.NewArinShadowRPCPipePool(ctx, commands)
	if err != nil {
		return invalid
	}
	defer pool.Close()
	open := func(endpoint captureEndpoint) (*server.ArinShadowRPCClient, error) {
		var transport server.ArinShadowRPCRoundTrip
		if endpoint.Socket != "" && endpoint.Bridge == "" && filepath.IsAbs(endpoint.Socket) {
			transport = server.ArinShadowUnixRoundTrip(endpoint.Socket)
		} else if endpoint.Socket == "" && commands[endpoint.Bridge] != nil {
			transport = pool.RoundTrip(endpoint.Bridge)
		} else {
			return nil, invalid
		}
		return server.NewArinShadowRPCClient(config.RunId, key, endpoint.Identity, transport)
	}
	recorder, err := server.OpenArinShadowCaptureRecorder(config.ActivePath, config.ActiveSHA256, config.CandidatePath, config.CandidateSHA256, server.NowUtc(), 1)
	if err != nil {
		return invalid
	}
	defer recorder.Close()
	pins, err := recorder.ResourcePins()
	if err != nil {
		return invalid
	}
	fleet := model.ArinShadowRemoteFleet{Handlers: map[server.Id]*server.ArinShadowRPCClient{}, InventoryComplete: true}
	seen := map[server.Id]bool{}
	clients := []*server.ArinShadowRPCClient{}
	released := false
	release := func() int {
		if released {
			return 0
		}
		released = true
		unreleased := 0
		for _, client := range clients {
			var reply struct{}
			if client.Call(ctx, "release", struct{}{}, &reply) != nil {
				unreleased++
			}
		}
		return unreleased
	}
	// An inventory failure can occur after an earlier owner opened readers.
	// Release those readers on every return, within the same finite owner.
	defer release()
	for _, endpoint := range config.Connect {
		if endpoint.Identity.Role != "connect" || seen[endpoint.Identity.ProcessNonce] {
			return invalid
		}
		seen[endpoint.Identity.ProcessNonce] = true
		client, err := open(endpoint)
		if err != nil {
			return invalid
		}
		clients = append(clients, client)
		var inventory connectserver.ArinShadowHandlerInventory
		if client.Call(setup, "inventory", struct{}{}, &inventory) != nil || !inventory.Complete || inventory.Resources != pins || len(inventory.Handlers) > 256 || inventory.OverflowConnections != 0 {
			return invalid
		}
		for _, id := range inventory.Handlers {
			if id == (server.Id{}) || fleet.Handlers[id] != nil {
				return invalid
			}
			fleet.Handlers[id] = client
		}
	}
	// Select the actual current publisher after lazy resource setup, immediately
	// before acquiring its immutable generation lease.
	for _, endpoint := range config.Native {
		if endpoint.Identity.Role != "native" {
			return invalid
		}
		client, err := open(endpoint)
		if err != nil {
			return invalid
		}
		var status model.ArinShadowNativeStatus
		if client.Call(setup, "native_status", struct{}{}, &status) != nil {
			return invalid
		}
		if status.Available {
			if fleet.Native != nil {
				return invalid
			}
			fleet.Native = client
		}
	}
	if fleet.Native == nil {
		return invalid
	}
	setupElapsed := time.Since(operatorStarted)
	stopSetup()
	captureStarted := time.Now()
	report, collectErr := model.CollectArinShadowRemotePublic(ctx, recorder, fleet, config.RequiredBuckets)
	captureElapsed := time.Since(captureStarted)
	// Release candidate readers after the final capture; a failed release is
	// not retried. Runtime expiration independently bounds any unreachable one.
	unreleased := release()
	started, peak := pool.Counts()
	result := struct {
		Report                     server.ArinShadowCaptureReport `json:"report"`
		SourceComplete             bool                           `json:"source_complete"`
		InventorySHA256            string                         `json:"inventory_sha256"`
		PolicyActivated            bool                           `json:"policy_activated"`
		UnreleasedOwners           int                            `json:"unreleased_owners"`
		BridgeStarts               int                            `json:"bridge_starts"`
		PeakConcurrentBridges      int                            `json:"peak_concurrent_bridges"`
		SetupElapsedMilliseconds   int64                          `json:"setup_elapsed_ms"`
		CaptureElapsedMilliseconds int64                          `json:"capture_elapsed_ms"`
		OperatorMaximumSeconds     int                            `json:"operator_maximum_seconds"`
	}{report, collectErr == nil, config.InventorySHA256, false, unreleased, started, peak, setupElapsed.Milliseconds(), captureElapsed.Milliseconds(), int(captureOperatorTimeout / time.Second)}
	if json.NewEncoder(output).Encode(result) != nil {
		return invalid
	}
	return collectErr
}

func readProtectedCaptureFile(path string, limit int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > limit {
		return nil, invalid
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, invalid
	}
	defer file.Close()
	actual, err := file.Stat()
	if err != nil || !os.SameFile(info, actual) {
		return nil, invalid
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, invalid
	}
	return data, nil
}

// The reviewed remote bridge opens only one explicitly named Unix socket per
// request. Its lifetime, frame count and bytes are bounded. It reads no vault,
// address, SQL, environment credential, inventory or shell command.
func runCaptureBridge(input io.Reader, output io.Writer, args []string) error {
	flags := flag.NewFlagSet("capture-bridge", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	socket := flags.String("socket", "", "")
	inventoryPath := flags.String("inventory", "", "")
	if flags.Parse(args) != nil || flags.NArg() != 0 || (*socket == "") == (*inventoryPath == "") || (*socket != "" && !filepath.IsAbs(*socket)) {
		return invalid
	}
	sockets := map[server.Id]string{}
	if *inventoryPath != "" {
		data, err := readProtectedCaptureFile(*inventoryPath, 64<<10)
		if err != nil {
			return invalid
		}
		var inventory struct {
			Endpoints []struct {
				ProcessNonce server.Id `json:"process_nonce"`
				Socket       string    `json:"socket"`
			} `json:"endpoints"`
		}
		if decode(data, &inventory) != nil || len(inventory.Endpoints) == 0 || len(inventory.Endpoints) > 64 {
			return invalid
		}
		for _, endpoint := range inventory.Endpoints {
			if endpoint.ProcessNonce == (server.Id{}) || !filepath.IsAbs(endpoint.Socket) || filepath.Clean(endpoint.Socket) != endpoint.Socket || sockets[endpoint.ProcessNonce] != "" {
				return invalid
			}
			sockets[endpoint.ProcessNonce] = endpoint.Socket
		}
	}
	return server.ServeArinShadowRPCBridge(context.Background(), input, output, sockets, *socket)
}
