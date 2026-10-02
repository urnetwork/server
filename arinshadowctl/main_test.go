package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"github.com/maxmind/mmdbwriter"
	"github.com/maxmind/mmdbwriter/mmdbtype"
	mmdb "github.com/oschwald/maxminddb-golang/v2"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestShadowDryRunRealDecoderAggregateOnly(t *testing.T) {
	w, err := mmdbwriter.New(mmdbwriter.Options{DatabaseType: "urnetwork arindb", IncludeReservedNetworks: true, Description: map[string]string{"en": "synthetic shadow fixture"}})
	if err != nil {
		t.Fatal(err)
	}
	_, network, _ := net.ParseCIDR("192.0.2.0/24")
	if err = w.Insert(network, mmdbtype.Map{"classifier_version": mmdbtype.Uint32(1), "quality_policy_version": mmdbtype.Uint32(2), "quality_state": mmdbtype.String("subscriber"), "non_quality": mmdbtype.Bool(false), "risk": mmdbtype.Bool(false)}); err != nil {
		t.Fatal(err)
	}
	var data bytes.Buffer
	if _, err = w.WriteTo(&data); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "test.mmdb")
	if err = os.WriteFile(path, data.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(data.Bytes())
	pin := hex.EncodeToString(hash[:])
	db, err := mmdb.OpenBytes(data.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	epoch := db.Metadata.BuildTime().Unix()
	db.Close()
	source := []byte(`{"complete":true,"required_buckets":["us","ca"],"providers":[{"token":"secret-provider","connections":["secret-connection"],"buckets":["us"],"base_quality":true,"base_speed":true,"active_quality":false,"active_speed":true}]}`)
	var censusData map[string]any
	if err := json.Unmarshal(source, &censusData); err != nil {
		t.Fatal(err)
	}
	lookupAt := time.Now().UTC()
	facts := map[string]any{"Epoch": epoch, "At": lookupAt, "Risk": false, "NonQuality": false, "Verified": true}
	provider := censusData["providers"].([]any)[0].(map[string]any)
	provider["lookups"] = map[string]any{"secret-connection": facts}
	source, err = json.Marshal(censusData)
	if err != nil {
		t.Fatal(err)
	}
	censusPath := filepath.Join(dir, "census.json")
	os.WriteFile(censusPath, source, 0600)
	hash = sha256.Sum256(source)
	args := []string{"--active-mmdb", path, "--active-sha256", pin, "--candidate-mmdb", path, "--candidate-sha256", pin, "--census", censusPath, "--census-sha256", hex.EncodeToString(hash[:]), "--cutover", time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano)}
	input, _ := json.Marshal(map[string]any{"connection": "secret-connection", "address": "192.0.2.1", "active": facts})
	var output bytes.Buffer
	if err := run(bytes.NewReader(input), &output, args); err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if json.Unmarshal(output.Bytes(), &result) != nil || result["actual_main_coverage"] != false || result["observation_complete"] != true {
		t.Fatal("dry-run qualification wrong")
	}
	for _, value := range []string{"secret-provider", "secret-connection", "192.0.2.1"} {
		if strings.Contains(output.String(), value) {
			t.Fatal("private value escaped")
		}
	}
	output.Reset()
	args[3] = strings.Repeat("0", 64)
	if run(bytes.NewReader(input), &output, args) == nil || output.Len() != 0 {
		t.Fatal("wrong immutable pin accepted or output escaped")
	}
}
func TestShadowDryRunRejectsPrivateMalformedInput(t *testing.T) {
	var out bytes.Buffer
	err := run(strings.NewReader(`{"address":"secret-address"}`), &out, []string{"--private-secret-invalid"})
	if err == nil || out.Len() != 0 || strings.Contains(err.Error(), "secret-address") {
		t.Fatal("malformed input privacy failure")
	}
}

func TestShadowCensusRequiresExplicitServingFacts(t *testing.T) {
	source := []byte(`{"complete":true,"required_buckets":["us"],"providers":[{"token":"p","connections":["c"],"buckets":["us"],"base_quality":false,"base_speed":false,"active_quality":false,"active_speed":false}]}`)
	for _, field := range []string{"base_quality", "base_speed", "active_quality", "active_speed"} {
		for _, useNull := range []bool{false, true} {
			var data map[string]any
			if err := json.Unmarshal(source, &data); err != nil {
				t.Fatal(err)
			}
			provider := data["providers"].([]any)[0].(map[string]any)
			delete(provider, field)
			if useNull {
				provider[field] = nil
			}
			encoded, err := json.Marshal(data)
			if err != nil {
				t.Fatal(err)
			}
			var parsed census
			if decode(encoded, &parsed) != nil {
				t.Fatal("typed source document failed before presence check")
			}
			if _, err := parsed.providers(); err == nil {
				t.Fatal("missing serving evidence became false")
			}
		}
	}
	var parsed census
	if decode(source, &parsed) != nil {
		t.Fatal("complete source rejected")
	}
	if _, err := parsed.providers(); err != nil {
		t.Fatal("explicit false source facts rejected")
	}
}
