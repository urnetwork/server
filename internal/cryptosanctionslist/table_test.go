package cryptosanctionslist

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testManifest() Manifest {
	return Manifest{
		Generator: "cryptosanctionslist test",
		Source: Source{
			Url:         "https://sanctionslistservice.ofac.treas.gov/api/PublicationPreview/exports/SDN.XML",
			PublishDate: "2026-11-09",
			RecordCount: 3,
			Sha256:      strings.Repeat("ab", 32),
			Bytes:       1234,
			RetrievedAt: time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC),
		},
		OfacDigitalCurrencyIds: map[string]int{"XBT": 2, "ETH": 1, "USDT": 1, "TRX": 4},
	}
}

func testEntries() map[string][]Entry {
	return map[string][]Entry{
		AssetBtc: {
			{Address: "12QtD5BFwRsdNsAZY76UVE1xyCGNTojH9h", OfacCurrencies: []string{"XBT"}, Sdn: []SdnRef{{Uid: 1, Name: "ONE & CO"}}},
			{Address: "bc1qv7k70u2zynvem59u88ctdlaw7hc735d8xep9rq", OfacCurrencies: []string{"USDT", "XBT"}, Sdn: []SdnRef{{Uid: 1, Name: "ONE & CO"}, {Uid: 2, Name: "TWO, Person"}}},
		},
		AssetEth: {
			{Address: "0x1cab8177ace78b1b6b1c393371f4f2dcae40cbeb", OfacCurrencies: []string{"ETH"}, Sdn: []SdnRef{{Uid: 3, Name: "THREE"}}},
		},
	}
}

const testAddressesJson = `{
  "btc": [
    {"address":"12QtD5BFwRsdNsAZY76UVE1xyCGNTojH9h","ofac_currencies":["XBT"],"sdn":[{"uid":1,"name":"ONE & CO"}]},
    {"address":"bc1qv7k70u2zynvem59u88ctdlaw7hc735d8xep9rq","ofac_currencies":["USDT","XBT"],"sdn":[{"uid":1,"name":"ONE & CO"},{"uid":2,"name":"TWO, Person"}]}
  ],
  "eth": [
    {"address":"0x1cab8177ace78b1b6b1c393371f4f2dcae40cbeb","ofac_currencies":["ETH"],"sdn":[{"uid":3,"name":"THREE"}]}
  ],
  "sol": [],
  "usdc_solana": [],
  "xmr": [],
  "zec": [],
  "tao": []
}
`

func TestWriteTableLayout(t *testing.T) {
	dir := t.TempDir()
	if err := WriteTable(dir, testManifest(), testEntries()); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(dir, AddressesName))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != testAddressesJson {
		t.Fatalf("addresses.json:\n%s\nwant:\n%s", content, testAddressesJson)
	}
	var manifest Manifest
	manifestContent, err := os.ReadFile(filepath.Join(dir, ManifestName))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(manifestContent, &manifest); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	if manifest.Format != Format || manifest.Addresses.Name != AddressesName ||
		manifest.Addresses.Sha256 != hex.EncodeToString(sum[:]) || manifest.Addresses.Bytes != int64(len(content)) {
		t.Fatalf("manifest format and digest: %+v", manifest)
	}
	if btc := manifest.Assets[AssetBtc]; btc.Count != 2 || btc.OfacCurrencies["XBT"] != 2 || btc.OfacCurrencies["USDT"] != 1 {
		t.Fatalf("btc summary: %+v", btc)
	}
	if tao, ok := manifest.Assets[AssetTao]; !ok || tao.Count != 0 {
		t.Fatalf("tao summary: %+v", tao)
	}
	if !strings.HasSuffix(string(manifestContent), "}\n") || !strings.Contains(string(manifestContent), "\n  \"source\": {") {
		t.Fatalf("manifest layout:\n%s", manifestContent)
	}
	// the same input writes the same bytes
	again := t.TempDir()
	if err := WriteTable(again, testManifest(), testEntries()); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{AddressesName, ManifestName} {
		first, _ := os.ReadFile(filepath.Join(dir, name))
		second, _ := os.ReadFile(filepath.Join(again, name))
		if string(first) != string(second) {
			t.Fatalf("%s differs between writes", name)
		}
	}
}

func TestReadTableRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if err := WriteTable(dir, testManifest(), testEntries()); err != nil {
		t.Fatal(err)
	}
	table, err := ReadTable(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !table.Contains(AssetBtc, "12QtD5BFwRsdNsAZY76UVE1xyCGNTojH9h") || !table.Contains(AssetBtc, "bc1qv7k70u2zynvem59u88ctdlaw7hc735d8xep9rq") ||
		!table.Contains(AssetEth, "0x1cab8177ace78b1b6b1c393371f4f2dcae40cbeb") {
		t.Fatal("listed addresses are missing")
	}
	if table.Contains(AssetEth, "12QtD5BFwRsdNsAZY76UVE1xyCGNTojH9h") || table.Contains(AssetSol, "0x1cab8177ace78b1b6b1c393371f4f2dcae40cbeb") {
		t.Fatal("an address matched another asset")
	}
	if table.Count(AssetBtc) != 2 || table.Count(AssetEth) != 1 || table.Count(AssetTao) != 0 || len(table.Skipped) != 0 {
		t.Fatalf("counts %d %d %d, skipped %v", table.Count(AssetBtc), table.Count(AssetEth), table.Count(AssetTao), table.Skipped)
	}
	if table.Manifest.Source.PublishDate != "2026-11-09" {
		t.Fatalf("manifest %+v", table.Manifest)
	}
}

func TestWriteTableRefusesBadEntries(t *testing.T) {
	cases := map[string]map[string][]Entry{
		"not canonical": {AssetEth: {{Address: "0x1CAb8177ACe78b1B6B1c393371F4f2dCAE40CbEB", OfacCurrencies: []string{"ETH"}, Sdn: []SdnRef{{Uid: 1}}}}},
		"invalid":       {AssetBtc: {{Address: "TUCsTq7TofTCJRRoHk6RvhMoS2mJLm5Yzq", OfacCurrencies: []string{"XBT"}, Sdn: []SdnRef{{Uid: 1}}}}},
		"unsorted": {AssetBtc: {
			{Address: "bc1qv7k70u2zynvem59u88ctdlaw7hc735d8xep9rq", OfacCurrencies: []string{"XBT"}, Sdn: []SdnRef{{Uid: 1}}},
			{Address: "12QtD5BFwRsdNsAZY76UVE1xyCGNTojH9h", OfacCurrencies: []string{"XBT"}, Sdn: []SdnRef{{Uid: 1}}},
		}},
		"duplicate": {AssetBtc: {
			{Address: "12QtD5BFwRsdNsAZY76UVE1xyCGNTojH9h", OfacCurrencies: []string{"XBT"}, Sdn: []SdnRef{{Uid: 1}}},
			{Address: "12QtD5BFwRsdNsAZY76UVE1xyCGNTojH9h", OfacCurrencies: []string{"XBT"}, Sdn: []SdnRef{{Uid: 2}}},
		}},
		"unlisted":      {AssetBtc: {{Address: "12QtD5BFwRsdNsAZY76UVE1xyCGNTojH9h"}}},
		"unknown asset": {"ltc": {{Address: "LaizKtS5DUhPuP1nTQcc83MS7HwK6vk85z", OfacCurrencies: []string{"LTC"}, Sdn: []SdnRef{{Uid: 1}}}}},
	}
	for name, entries := range cases {
		dir := t.TempDir()
		if err := WriteTable(dir, testManifest(), entries); err == nil {
			t.Errorf("%s: written", name)
		}
		if _, err := os.Stat(filepath.Join(dir, ManifestName)); err == nil {
			t.Errorf("%s: a manifest was written", name)
		}
	}
}

// Writes the given addresses.json with a manifest whose digest matches it, so
// each case tests one defect.
func writeRawTable(t *testing.T, addresses string, edit func(*Manifest)) string {
	t.Helper()
	dir := t.TempDir()
	if err := WriteTable(dir, testManifest(), testEntries()); err != nil {
		t.Fatal(err)
	}
	manifestContent, err := os.ReadFile(filepath.Join(dir, ManifestName))
	if err != nil {
		t.Fatal(err)
	}
	var manifest Manifest
	if err := json.Unmarshal(manifestContent, &manifest); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(addresses))
	manifest.Addresses.Sha256 = hex.EncodeToString(sum[:])
	manifest.Addresses.Bytes = int64(len(addresses))
	if edit != nil {
		edit(&manifest)
	}
	manifestContent, err = json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, AddressesName), []byte(addresses), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ManifestName), manifestContent, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestReadTableRefusesDefects(t *testing.T) {
	corrupt := strings.Replace(testAddressesJson, "THREE", "THREF", 1)
	cases := map[string]func() string{
		"missing directory": func() string { return filepath.Join(t.TempDir(), "absent") },
		"missing addresses": func() string {
			dir := writeRawTable(t, testAddressesJson, nil)
			os.Remove(filepath.Join(dir, AddressesName))
			return dir
		},
		"changed addresses": func() string {
			dir := writeRawTable(t, testAddressesJson, nil)
			os.WriteFile(filepath.Join(dir, AddressesName), []byte(corrupt), 0o644)
			return dir
		},
		"truncated addresses": func() string {
			dir := writeRawTable(t, testAddressesJson, nil)
			os.WriteFile(filepath.Join(dir, AddressesName), []byte(testAddressesJson[:len(testAddressesJson)-10]), 0o644)
			return dir
		},
		"unreadable manifest": func() string {
			dir := writeRawTable(t, testAddressesJson, nil)
			os.WriteFile(filepath.Join(dir, ManifestName), []byte("{"), 0o644)
			return dir
		},
		"other format": func() string {
			return writeRawTable(t, testAddressesJson, func(manifest *Manifest) { manifest.Format = "urnetwork-crypto-sanctions-2" })
		},
		"other file name": func() string {
			return writeRawTable(t, testAddressesJson, func(manifest *Manifest) { manifest.Addresses.Name = "../addresses.json" })
		},
		"missing asset list": func() string {
			return writeRawTable(t, strings.Replace(testAddressesJson, "  \"tao\": []\n", "  \"ltc\": []\n", 1), nil)
		},
		"missing asset summary": func() string {
			return writeRawTable(t, testAddressesJson, func(manifest *Manifest) { delete(manifest.Assets, AssetXmr) })
		},
		"count mismatch": func() string {
			return writeRawTable(t, testAddressesJson, func(manifest *Manifest) {
				summary := manifest.Assets[AssetBtc]
				summary.Count = 3
				manifest.Assets[AssetBtc] = summary
			})
		},
		"not json": func() string {
			return writeRawTable(t, "btc 12QtD5BFwRsdNsAZY76UVE1xyCGNTojH9h\n", nil)
		},
	}
	for name, build := range cases {
		if table, err := ReadTable(build()); err == nil {
			t.Errorf("%s: read %+v", name, table.Manifest)
		}
	}
}

// An entry the current rules reject is skipped and reported, and the rest of
// the table serves: no address the rules accept can equal it.
func TestReadTableSkipsEntriesTheRulesReject(t *testing.T) {
	addresses := strings.Replace(testAddressesJson, `"eth": [
    {"address":"0x1cab8177ace78b1b6b1c393371f4f2dcae40cbeb"`, `"eth": [
    {"address":"0x1cab8177ace78b1b6b1c393371f4f2dcae40cbe"`, 1)
	dir := writeRawTable(t, addresses, nil)
	table, err := ReadTable(dir)
	if err != nil {
		t.Fatal(err)
	}
	if table.Count(AssetEth) != 0 || len(table.Skipped) != 1 || !strings.HasPrefix(table.Skipped[0], "eth 0x1cab8177ace78b1b6b1c393371f4f2dcae40cbe: ") {
		t.Fatalf("eth count %d, skipped %v", table.Count(AssetEth), table.Skipped)
	}
	if !table.Contains(AssetBtc, "12QtD5BFwRsdNsAZY76UVE1xyCGNTojH9h") {
		t.Fatal("the other entries are missing")
	}
}
