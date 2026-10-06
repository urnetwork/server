package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/urnetwork/server/v2026"
)

type captureExpectedSlot struct {
	Host    string `json:"host"`
	Service string `json:"service"`
	Block   string `json:"block"`
}

type captureInventoryFile struct {
	Host   string `json:"host"`
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Bridge string `json:"bridge"`
}

type captureInventoryPlan struct {
	Expected        []captureExpectedSlot  `json:"expected_slots"`
	HostInventories []captureInventoryFile `json:"host_inventories"`
	Bridges         []captureBridge        `json:"bridges"`
}

// The protected plan is an explicit operator authority for the enabled
// host/service/block matrix. Local inventory files must cover that matrix and
// every observed running generation. A boolean alone never supplies coverage.
func captureInventoryEndpoints(c captureConfig, now time.Time) ([]captureEndpoint, []captureEndpoint, string, error) {
	if len(c.Expected) == 0 || len(c.Expected) > 64 || len(c.HostInventories) == 0 || len(c.HostInventories) > 8 || len(c.Bridges) != len(c.HostInventories) || c.RunId == (server.Id{}) || !now.Before(c.NotAfter) {
		return nil, nil, "", invalid
	}
	expected := map[captureExpectedSlot]bool{}
	hosts := map[string]bool{}
	for _, slot := range c.Expected {
		if slot.Host == "" || len(slot.Host) > 255 || slot.Block == "" || len(slot.Block) > 64 || (slot.Service != "connect" && slot.Service != "taskworker") || expected[slot] {
			return nil, nil, "", invalid
		}
		expected[slot] = true
		hosts[slot.Host] = true
	}
	bridges := map[string]bool{}
	for _, bridge := range c.Bridges {
		if bridge.Name == "" || bridges[bridge.Name] || len(bridge.Argv) == 0 || len(bridge.Argv) > 64 || !filepath.IsAbs(bridge.Argv[0]) {
			return nil, nil, "", invalid
		}
		bridges[bridge.Name] = true
	}
	seenSlots := map[captureExpectedSlot]bool{}
	seenNonces := map[server.Id]bool{}
	seenContainers := map[string]bool{}
	connectEndpoints, nativeEndpoints := []captureEndpoint{}, []captureEndpoint{}
	for _, ref := range c.HostInventories {
		if !hosts[ref.Host] || !bridges[ref.Bridge] || len(ref.SHA256) != 64 {
			return nil, nil, "", invalid
		}
		delete(hosts, ref.Host)
		delete(bridges, ref.Bridge)
		data, err := readProtectedCaptureFile(ref.Path, 128<<10)
		if err != nil {
			return nil, nil, "", invalid
		}
		hash := sha256.Sum256(data)
		if hex.EncodeToString(hash[:]) != ref.SHA256 {
			return nil, nil, "", invalid
		}
		var inventory hostCaptureInventory
		if decode(data, &inventory) != nil || !inventory.Complete || inventory.Hostname != ref.Host || inventory.Environment != "main" || inventory.StartedAt.IsZero() || inventory.FinishedAt.Before(inventory.StartedAt) || inventory.FinishedAt.Sub(inventory.StartedAt) > 30*time.Second || inventory.FinishedAt.After(now.Add(2*time.Second)) || now.Sub(inventory.FinishedAt) > 5*time.Minute || len(inventory.Processes) == 0 || len(inventory.Processes) > 64 {
			return nil, nil, "", invalid
		}
		for _, process := range inventory.Processes {
			row := process.Container
			slot := captureExpectedSlot{ref.Host, row.Service, row.Block}
			if !expected[slot] || seenContainers[row.ID] || process.Reason != "qualified" || process.Endpoint == nil || len(process.ExecutableSHA256) != 64 || row.PID <= 0 || row.Environment != "main" || row.StartedAt.IsZero() {
				return nil, nil, "", invalid
			}
			if b, e := hex.DecodeString(process.ExecutableSHA256); e != nil || len(b) != 32 {
				return nil, nil, "", invalid
			}
			if b, e := hex.DecodeString(row.ID); e != nil || len(b) != 32 {
				return nil, nil, "", invalid
			}
			endpoint := *process.Endpoint
			identity := endpoint.Identity
			role := "connect"
			if row.Service == "taskworker" {
				role = "native"
			}
			if identity.ProcessNonce == (server.Id{}) || seenNonces[identity.ProcessNonce] || identity.Role != role || identity.Host != ref.Host || identity.Environment != "main" || identity.Block != row.Block || identity.ImageDigest != row.Image || strings.ReplaceAll(identity.Version, "+", "-") != row.Version || identity.StartedAt.Before(row.StartedAt.Add(-2*time.Second)) || identity.StartedAt.After(inventory.FinishedAt.Add(2*time.Second)) || len(identity.Revision) != 40 || endpoint.Bridge != "" || !filepath.IsAbs(endpoint.Socket) || filepath.Clean(endpoint.Socket) != endpoint.Socket {
				return nil, nil, "", invalid
			}
			if b, e := hex.DecodeString(identity.Revision); e != nil || len(b) != 20 {
				return nil, nil, "", invalid
			}
			seenSlots[slot], seenNonces[identity.ProcessNonce], seenContainers[row.ID] = true, true, true
			endpoint.Socket, endpoint.Bridge = "", ref.Bridge
			if role == "connect" {
				connectEndpoints = append(connectEndpoints, endpoint)
			} else {
				nativeEndpoints = append(nativeEndpoints, endpoint)
			}
		}
	}
	if len(hosts) != 0 || len(bridges) != 0 || len(seenSlots) != len(expected) || len(connectEndpoints) == 0 || len(connectEndpoints) > 256 || len(nativeEndpoints) == 0 || len(nativeEndpoints) > 16 {
		return nil, nil, "", invalid
	}
	// Bind the reviewed matrix, actual file bytes, exact commands, run and
	// resources. Run/epoch equality is then verified again by authenticated RPC.
	bound := struct {
		RunId           server.Id `json:"run_id"`
		ActiveSHA256    string    `json:"active_sha256"`
		CandidateSHA256 string    `json:"candidate_sha256"`
		captureInventoryPlan
	}{c.RunId, c.ActiveSHA256, c.CandidateSHA256, captureInventoryPlan{c.Expected, c.HostInventories, c.Bridges}}
	data, err := json.Marshal(bound)
	if err != nil {
		return nil, nil, "", invalid
	}
	hash := sha256.Sum256(data)
	return connectEndpoints, nativeEndpoints, hex.EncodeToString(hash[:]), nil
}

func validateCaptureInventory(c captureConfig, now time.Time) error {
	connectEndpoints, nativeEndpoints, digest, err := captureInventoryEndpoints(c, now)
	if err != nil || !c.InventoryComplete || digest != c.InventorySHA256 || !slices.Equal(connectEndpoints, c.Connect) || !slices.Equal(nativeEndpoints, c.Native) {
		return invalid
	}
	return nil
}

func runCaptureAssemble(output io.Writer, args []string) error {
	flags := flag.NewFlagSet("capture-assemble", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	template := flags.String("template", "", "")
	plan := flags.String("inventory-plan", "", "")
	destination := flags.String("output", "", "")
	if flags.Parse(args) != nil || flags.NArg() != 0 || !filepath.IsAbs(*destination) {
		return invalid
	}
	data, err := readProtectedCaptureFile(*template, 256<<10)
	var config captureConfig
	if err != nil || decode(data, &config) != nil || config.InventoryComplete {
		return invalid
	}
	data, err = readProtectedCaptureFile(*plan, 256<<10)
	var inventory captureInventoryPlan
	if err != nil || decode(data, &inventory) != nil {
		return invalid
	}
	config.Expected, config.HostInventories, config.Bridges = inventory.Expected, inventory.HostInventories, inventory.Bridges
	config.Connect, config.Native, config.InventorySHA256, err = captureInventoryEndpoints(config, time.Now())
	if err != nil {
		return invalid
	}
	config.InventoryComplete = true
	data, err = json.MarshalIndent(config, "", "  ")
	if err != nil {
		return invalid
	}
	file, err := os.OpenFile(*destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return invalid
	}
	_, writeErr := file.Write(data)
	syncErr := file.Sync()
	closeErr := file.Close()
	if writeErr != nil || syncErr != nil || closeErr != nil {
		return invalid
	}
	dir, err := os.Open(filepath.Dir(*destination))
	if err != nil {
		return invalid
	}
	err = dir.Sync()
	dir.Close()
	if err != nil {
		return invalid
	}
	return json.NewEncoder(output).Encode(map[string]any{"inventory_complete": true, "inventory_sha256": config.InventorySHA256, "connect_processes": len(config.Connect), "native_processes": len(config.Native), "policy_activated": false})
}
