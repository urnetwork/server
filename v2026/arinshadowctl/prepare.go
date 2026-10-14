package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/urnetwork/server/v2026"
)

// prepare writes secrets only to a newly created private directory. It does
// not load Vault, build/publish a resource, choose a host or contact a service.
func runCapturePrepare(output io.Writer, args []string) error {
	flags := flag.NewFlagSet("capture-prepare", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	directory := flags.String("output", "", "")
	active := flags.String("active-mmdb", "", "")
	activeHash := flags.String("active-sha256", "", "")
	candidate := flags.String("candidate-mmdb", "", "")
	candidateHash := flags.String("candidate-sha256", "", "")
	activeResource := flags.String("active-resource", "arindb/arin.mmdb", "")
	candidateResource := flags.String("candidate-resource", "arindb-shadow/policy-two/arin.mmdb", "")
	capacity := flags.Int("capacity", 250000, "")
	expiresText := flags.String("expires-at", "", "")
	if flags.Parse(args) != nil || flags.NArg() != 0 || !filepath.IsAbs(*directory) || filepath.Clean(*directory) != *directory || *activeResource == *candidateResource || *capacity < 1 || *capacity > server.ArinShadowCapturePopulationLimit {
		return invalid
	}
	expires, err := time.Parse(time.RFC3339Nano, *expiresText)
	if err != nil || !expires.After(time.Now()) || expires.After(time.Now().Add(4*time.Hour)) {
		return invalid
	}
	for _, resource := range []string{*activeResource, *candidateResource} {
		if !filepath.IsLocal(resource) || filepath.Clean(resource) != resource {
			return invalid
		}
	}
	recorder, err := server.OpenArinShadowCaptureRecorder(*active, *activeHash, *candidate, *candidateHash, server.NowUtc(), 1)
	if err != nil {
		return invalid
	}
	defer recorder.Close()
	if os.Mkdir(*directory, 0700) != nil {
		return invalid
	}
	key := make([]byte, 32)
	if _, err = rand.Read(key); err != nil {
		return invalid
	}
	run := server.NewId()
	runtime := server.ArinShadowRuntimeConfig{RunId: run, KeyHex: hex.EncodeToString(key), Directory: "/tmp/arin-shadow", ExpiresAt: expires, ActiveSHA256: *activeHash, CandidateSHA256: *candidateHash, ActiveResource: *activeResource, CandidateResource: *candidateResource, Capacity: *capacity}
	buckets := []string{"all"}
	for a := 'a'; a <= 'z'; a++ {
		for b := 'a'; b <= 'z'; b++ {
			buckets = append(buckets, string([]rune{a, b}))
		}
	}
	template := captureConfig{RunId: run, KeyFile: filepath.Join(*directory, "capture.key"), NotBefore: server.NowUtc(), NotAfter: expires, InventoryComplete: false, Native: []captureEndpoint{}, Connect: []captureEndpoint{}, Bridges: []captureBridge{}, ActivePath: *active, ActiveSHA256: *activeHash, CandidatePath: *candidate, CandidateSHA256: *candidateHash, RequiredBuckets: buckets}
	files := map[string][]byte{"capture.key": key}
	files["arin-shadow-capture.json"], _ = json.MarshalIndent(runtime, "", "  ")
	files["operator-template.json"], _ = json.MarshalIndent(template, "", "  ")
	for name, data := range files {
		file, err := os.OpenFile(filepath.Join(*directory, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return invalid
		}
		_, writeErr := file.Write(data)
		syncErr := file.Sync()
		closeErr := file.Close()
		if writeErr != nil || syncErr != nil || closeErr != nil {
			return invalid
		}
	}
	dir, err := os.Open(*directory)
	if err != nil {
		return invalid
	}
	syncErr := dir.Sync()
	dir.Close()
	if syncErr != nil {
		return invalid
	}
	return json.NewEncoder(output).Encode(map[string]any{"prepared": true, "files": 3, "policy_activated": false, "inventory_complete": false, "credential_output": false, "expires_at": expires})
}
