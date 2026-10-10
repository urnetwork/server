package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/maxmind/mmdbwriter"
	"github.com/maxmind/mmdbwriter/mmdbtype"
	"github.com/urnetwork/server/v2026"
)

func captureTestWrite(t *testing.T, path string, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

func captureTestConfig(t *testing.T) (captureConfig, hostCaptureInventory) {
	t.Helper()
	now := time.Now().UTC()
	dir := t.TempDir()
	c := captureConfig{RunId: server.NewId(), NotBefore: now.Add(-time.Second), NotAfter: now.Add(time.Minute), ActiveSHA256: strings.Repeat("a", 64), CandidateSHA256: strings.Repeat("b", 64), Expected: []captureExpectedSlot{{"fixture-host", "connect", "g1"}, {"fixture-host", "taskworker", "g1"}}, Bridges: []captureBridge{{Name: "host", Argv: []string{"/usr/bin/false"}}}}
	inventory := hostCaptureInventory{Hostname: "fixture-host", Environment: "main", StartedAt: now.Add(-time.Second), FinishedAt: now, Complete: true}
	for i, service := range []string{"connect", "taskworker", "connect"} {
		role := "connect"
		if service == "taskworker" {
			role = "native"
		}
		identity := server.ArinShadowRPCIdentity{ProcessNonce: server.NewId(), StartedAt: now.Add(-time.Minute), Revision: strings.Repeat("c", 40), Modified: i == 2, ImageDigest: "sha256:" + strings.Repeat("d", 64), Role: role, Version: "2026.10.2+123", Environment: "main", Host: "fixture-host", Block: "g1"}
		// A second, recorded generation remains in the inventory even when its
		// source is dirty. Neither clean nor newest-only is the authority.
		row := hostContainer{ID: strings.Repeat(string(rune('a'+i)), 64), PID: 100 + i, Image: identity.ImageDigest, StartedAt: now.Add(-time.Hour), Environment: "main", Service: service, Block: "g1", Version: "2026.10.2-123"}
		inventory.Processes = append(inventory.Processes, hostCaptureProcess{Container: row, ExecutableSHA256: strings.Repeat("e", 64), Endpoint: &captureEndpoint{Identity: identity, Socket: filepath.Join("/proc", string(rune('1'+i)), "root/tmp/arin-shadow/connect/capture.sock")}, Reason: "qualified"})
	}
	ref := captureInventoryFile{Host: "fixture-host", Path: filepath.Join(dir, "host.json"), Bridge: "host"}
	ref.SHA256 = captureTestWrite(t, ref.Path, inventory)
	c.HostInventories = []captureInventoryFile{ref}
	var err error
	c.Connect, c.Native, c.InventorySHA256, err = captureInventoryEndpoints(c, now)
	if err != nil {
		t.Fatal(err)
	}
	c.InventoryComplete = true
	return c, inventory
}

func TestCaptureInventoryBindsEveryExpectedSlotAndGeneration(t *testing.T) {
	c, _ := captureTestConfig(t)
	if validateCaptureInventory(c, time.Now()) != nil || len(c.Connect) != 2 || len(c.Native) != 1 {
		t.Fatal("complete predecessor-inclusive inventory rejected")
	}
	for _, which := range []string{"empty", "missing_slot", "missing_host", "old_endpoint", "file_hash", "reused_nonce", "wrong_run", "changed_resource", "omitted_endpoint", "endpoint_source", "old_inventory", "wrong_matrix", "unreadable"} {
		t.Run(which, func(t *testing.T) {
			c, inv := captureTestConfig(t)
			switch which {
			case "empty":
				inv.Processes = nil
			case "missing_slot":
				inv.Processes = append(inv.Processes[:1], inv.Processes[2:]...)
			case "missing_host":
				c.Expected = append(c.Expected, captureExpectedSlot{"absent-host", "connect", "g2"})
			case "old_endpoint":
				inv.Processes[2].Endpoint = nil
				inv.Processes[2].Reason = "endpoint_absent"
				inv.Complete = false
			case "file_hash":
				c.HostInventories[0].SHA256 = strings.Repeat("0", 64)
			case "reused_nonce":
				inv.Processes[2].Endpoint.Identity.ProcessNonce = inv.Processes[0].Endpoint.Identity.ProcessNonce
			case "wrong_run":
				c.RunId = server.NewId()
			case "changed_resource":
				c.CandidateSHA256 = strings.Repeat("0", 64)
			case "omitted_endpoint":
				c.Connect = c.Connect[:1]
			case "endpoint_source":
				c.Connect[0].Identity.Revision = strings.Repeat("0", 40)
			case "old_inventory":
				inv.StartedAt = inv.StartedAt.Add(-6 * time.Minute)
				inv.FinishedAt = inv.FinishedAt.Add(-6 * time.Minute)
			case "wrong_matrix":
				inv.Processes[0].Container.Block = "g9"
			case "unreadable":
				if os.Chmod(c.HostInventories[0].Path, 0644) != nil {
					t.Fatal("chmod")
				}
			}
			if which == "empty" || which == "missing_slot" || which == "old_endpoint" || which == "reused_nonce" || which == "old_inventory" || which == "wrong_matrix" {
				c.HostInventories[0].SHA256 = captureTestWrite(t, c.HostInventories[0].Path, inv)
			}
			if validateCaptureInventory(c, time.Now()) == nil {
				t.Fatal("incomplete/unbound inventory accepted")
			}
		})
	}
}

func TestCaptureAssembleReadsPinnedFilesAndCreatesPrivateOutput(t *testing.T) {
	c, _ := captureTestConfig(t)
	dir := t.TempDir()
	plan := captureInventoryPlan{c.Expected, c.HostInventories, c.Bridges}
	c.Expected = nil
	c.HostInventories = nil
	c.Bridges = nil
	c.Connect = nil
	c.Native = nil
	c.InventorySHA256 = ""
	c.InventoryComplete = false
	template, planPath, result := filepath.Join(dir, "template.json"), filepath.Join(dir, "plan.json"), filepath.Join(dir, "result.json")
	captureTestWrite(t, template, c)
	captureTestWrite(t, planPath, plan)
	args := []string{"--template", template, "--inventory-plan", planPath, "--output", result}
	var out bytes.Buffer
	if runCaptureAssemble(&out, args) != nil {
		t.Fatal("assembly failed")
	}
	var assembled captureConfig
	data, err := readProtectedCaptureFile(result, 256<<10)
	if err != nil || decode(data, &assembled) != nil || validateCaptureInventory(assembled, time.Now()) != nil {
		t.Fatal("assembled authority not recheckable")
	}
	if strings.Contains(out.String(), "fixture-host") || strings.Contains(out.String(), "/proc") || strings.Contains(out.String(), "2026.10.2") {
		t.Fatal("private inventory escaped aggregate")
	}
	out.Reset()
	if runCaptureAssemble(&out, args) == nil || out.Len() != 0 {
		t.Fatal("existing output overwritten")
	}
}

func captureTestMMDB(t *testing.T) (string, string) {
	t.Helper()
	w, err := mmdbwriter.New(mmdbwriter.Options{DatabaseType: "urnetwork arindb", IncludeReservedNetworks: true})
	if err != nil {
		t.Fatal(err)
	}
	_, network, _ := net.ParseCIDR("192.0.2.0/24")
	if err = w.Insert(network, mmdbtype.Map{"classifier_version": mmdbtype.Uint32(1), "quality_policy_version": mmdbtype.Uint32(2), "quality_state": mmdbtype.String("subscriber"), "non_quality": mmdbtype.Bool(false), "risk": mmdbtype.Bool(false)}); err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	if _, err = w.WriteTo(&b); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "test.mmdb")
	if err = os.WriteFile(p, b.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(b.Bytes())
	return p, hex.EncodeToString(h[:])
}

func TestCapturePrepareKeepsCredentialPrivateAndInventoryUnqualified(t *testing.T) {
	path, pin := captureTestMMDB(t)
	dir := filepath.Join(t.TempDir(), "capture")
	args := []string{"--output", dir, "--active-mmdb", path, "--active-sha256", pin, "--candidate-mmdb", path, "--candidate-sha256", pin, "--expires-at", time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)}
	var out bytes.Buffer
	if runCapturePrepare(&out, args) != nil {
		t.Fatal("prepare failed")
	}
	key, err := readProtectedCaptureFile(filepath.Join(dir, "capture.key"), 32)
	if err != nil || len(key) != 32 || strings.Contains(out.String(), hex.EncodeToString(key)) {
		t.Fatal("secret output or missing credential")
	}
	data, err := readProtectedCaptureFile(filepath.Join(dir, "arin-shadow-capture.json"), 16<<10)
	var runtime server.ArinShadowRuntimeConfig
	if err != nil || decode(data, &runtime) != nil || runtime.KeyHex != hex.EncodeToString(key) || runtime.ActiveResource == runtime.CandidateResource {
		t.Fatal("runtime config mismatch")
	}
	data, err = readProtectedCaptureFile(filepath.Join(dir, "operator-template.json"), 256<<10)
	var c captureConfig
	if err != nil || decode(data, &c) != nil || c.InventoryComplete || len(c.RequiredBuckets) != 677 || validateCaptureInventory(c, time.Now()) == nil {
		t.Fatal("template manufactured inventory or lost buckets")
	}
	out.Reset()
	if runCapturePrepare(&out, args) == nil || out.Len() != 0 {
		t.Fatal("prepare overwrote key")
	}
}
