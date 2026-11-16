package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/server/cryptosanctions"
	"github.com/urnetwork/server/internal/cryptosanctionslist"
)

// testdata/SDN.XML is OFAC's SDN.XML of 2026-11-09 cut to 15 of its entries,
// verbatim, with Record_Count set to 15. It keeps an entry without digital
// currency identifiers, entities and individuals, Omni USDT, BCH and BSV
// addresses on Bitcoin, EVM addresses under several codes, Solana, Monero and
// Zcash addresses, and the two identifiers OFAC lists under a code their
// chain does not accept: a TRON address under XBT and a hex value under XMR.
const recordedSource = "testdata/SDN.XML"

var testClock = time.Date(2026, 11, 10, 23, 0, 0, 0, time.UTC)

func testDependencies() commandDependencies {
	return commandDependencies{
		now:        func() time.Time { return testClock },
		retryDelay: func(int) time.Duration { return 0 },
	}
}

func runUpdateCommand(t *testing.T, dependencies commandDependencies, args ...string) (string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	err := runCommand(context.Background(), append([]string{"update"}, args...), &stdout, &stderr, dependencies)
	return stdout.String(), err
}

func readRecordedSource(t *testing.T) []byte {
	t.Helper()
	content, err := os.ReadFile(recordedSource)
	if err != nil {
		t.Fatal(err)
	}
	return content
}

// The recorded identifiers, read with a pattern rather than the XML decoder.
type recordedId struct {
	currency string
	address  string
}

var recordedIdPattern = regexp.MustCompile(`<idType>Digital Currency Address - ([A-Z]+)</idType>\s*<idNumber>([^<]*)</idNumber>`)

func recordedIds(t *testing.T) []recordedId {
	ids := []recordedId{}
	for _, match := range recordedIdPattern.FindAllStringSubmatch(string(readRecordedSource(t)), -1) {
		ids = append(ids, recordedId{currency: match[1], address: strings.TrimSpace(match[2])})
	}
	if len(ids) != 271 {
		t.Fatalf("%d recorded identifiers, want 271", len(ids))
	}
	return ids
}

func entryOf(table map[string][]cryptosanctionslist.Entry, asset string, address string) (cryptosanctionslist.Entry, bool) {
	for _, entry := range table[asset] {
		if entry.Address == address {
			return entry, true
		}
	}
	return cryptosanctionslist.Entry{}, false
}

func readEntries(t *testing.T, dir string) map[string][]cryptosanctionslist.Entry {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(dir, cryptosanctionslist.AddressesName))
	if err != nil {
		t.Fatal(err)
	}
	entries := map[string][]cryptosanctionslist.Entry{}
	if err := json.Unmarshal(content, &entries); err != nil {
		t.Fatal(err)
	}
	return entries
}

func TestUpdateFromRecordedSource(t *testing.T) {
	output := filepath.Join(t.TempDir(), "all", "crypto", "2026.11.10+1")
	summary, err := runUpdateCommand(t, testDependencies(), "--source", recordedSource, "--output", output)
	if err != nil {
		t.Fatal(err)
	}
	source := readRecordedSource(t)
	sum := sha256.Sum256(source)
	sourceSha256 := hex.EncodeToString(sum[:])
	wantSummary := "OFAC SDN list of 2026-11-09 (sha256 " + sourceSha256[:12] + ", 271 digital currency identifiers): " +
		"btc 51, eth 36, sol 3, usdc_solana 3, xmr 5, zec 2, tao 0; 2 not valid on their chain and left out: " +
		"btc XBT TUCsTq7TofTCJRRoHk6RvhMoS2mJLm5Yzq (SDN 45404), " +
		"xmr XMR 5be5543ff73456ab9f2d207887e2af87322c651ea1a873c5b25b7ffae456c320 (SDN 29585)\n"
	if summary != wantSummary {
		t.Fatalf("summary:\n%q\nwant:\n%q", summary, wantSummary)
	}

	table, err := cryptosanctionslist.ReadTable(output)
	if err != nil {
		t.Fatal(err)
	}
	manifest := table.Manifest
	if manifest.Generator != "cryptoctl development" || manifest.Source.PublishDate != "2026-11-09" || manifest.Source.RecordCount != 15 ||
		manifest.Source.Sha256 != sourceSha256 || manifest.Source.Bytes != int64(len(source)) || !manifest.Source.RetrievedAt.Equal(testClock) ||
		!strings.HasPrefix(manifest.Source.Url, "file://") || !strings.HasSuffix(manifest.Source.Url, "/cryptoctl/testdata/SDN.XML") {
		t.Fatalf("manifest source: %+v, generator %q", manifest.Source, manifest.Generator)
	}

	// every identifier is counted under its code, and each one a chain
	// accepts is in that chain's assets; the rest under an asset's own code
	// are the unrecognized ones
	ids := recordedIds(t)
	codes := map[string]int{}
	for _, id := range ids {
		codes[id.currency] += 1
	}
	if len(codes) != len(manifest.OfacDigitalCurrencyIds) {
		t.Fatalf("codes %v, manifest %v", codes, manifest.OfacDigitalCurrencyIds)
	}
	for code, count := range codes {
		if manifest.OfacDigitalCurrencyIds[code] != count {
			t.Fatalf("%s: %d identifiers, manifest says %d", code, count, manifest.OfacDigitalCurrencyIds[code])
		}
	}
	unrecognized := 0
	for _, id := range ids {
		for _, asset := range cryptosanctionslist.Assets() {
			canonical, err := cryptosanctionslist.Normalize(asset, id.address)
			own, _ := cryptosanctionslist.OfacCurrency(asset)
			switch {
			case err == nil && !table.Contains(asset, canonical):
				t.Errorf("%s %s is not in %s", id.currency, id.address, asset)
			case err != nil && own == id.currency:
				found := false
				for _, item := range manifest.Unrecognized {
					found = found || (item.Asset == asset && item.Address == id.address && item.OfacCurrency == id.currency)
				}
				if !found {
					t.Errorf("%s %s is neither in %s nor unrecognized", id.currency, id.address, asset)
				}
				unrecognized += 1
			}
		}
	}
	if unrecognized != 2 || len(manifest.Unrecognized) != 2 {
		t.Fatalf("%d unrecognized identifiers, manifest lists %v", unrecognized, manifest.Unrecognized)
	}

	entries := readEntries(t, output)
	expect := func(asset string, address string, currencies string, uid int, name string) {
		t.Helper()
		entry, ok := entryOf(entries, asset, address)
		if !ok {
			t.Fatalf("%s %s missing", asset, address)
		}
		if strings.Join(entry.OfacCurrencies, ",") != currencies || len(entry.Sdn) != 1 || entry.Sdn[0].Uid != uid || entry.Sdn[0].Name != name {
			t.Fatalf("%s %s: %+v", asset, address, entry)
		}
	}
	expect(cryptosanctionslist.AssetBtc, "12QtD5BFwRsdNsAZY76UVE1xyCGNTojH9h", "XBT", 25308, "YAN, Xiaobing")
	expect(cryptosanctionslist.AssetBtc, "bc1qv7k70u2zynvem59u88ctdlaw7hc735d8xep9rq", "XBT", 31212, "SOUTHFRONT")
	// Omni USDT, and BCH and BSV legacy addresses, are Bitcoin addresses
	expect(cryptosanctionslist.AssetBtc, "16iWn2J1McqjToYLHSsAyS6En3QA8YQ91H", "USDT", 33151, "SUEX OTC, S.R.O.")
	expect(cryptosanctionslist.AssetBtc, "18M8bJWMzWHDBMxoLqjHHAffdRy4SrzkfB", "BCH", 30518, "SECONDEYE SOLUTION")
	// one EVM account under several codes, in lowercase
	expect(cryptosanctionslist.AssetEth, "0x983a81ca6fb1e441266d2fbcb7d8e530ac2e05a2", "ETH,USDC,USDT", 45314, "VALERIAN LABS, INC.")
	expect(cryptosanctionslist.AssetEth, "0x4f47bc496083c727c5fbe3ce9cdf2b0f6496270c", "ARB,BSC,ETH", 42498, "SIM, Hyon Sop")
	expect(cryptosanctionslist.AssetSol, "42RLPACwZPx3vYYmxSueqsogfynBDqXK298EDsNoyoHi", "SOL", 56360, "SOKOLOVSKI, Rolan")
	expect(cryptosanctionslist.AssetXmr, "884Bz8UH63aYsjVdkfWfScRYWZGGNbjFL7pztqvWNSrtYT4reFSwyvkCj9KEGUtheHhhMUj87ciTBFyzoesrMJ4L1FvSoxL", "XMR", 31212, "SOUTHFRONT")
	expect(cryptosanctionslist.AssetZec, "t1g7wowvQ8gn2v8jrU1biyJ26sieNqNsBJy", "ZEC", 29585, "KARASAVIDI, Dmitrii")
	if _, ok := entryOf(entries, cryptosanctionslist.AssetBtc, "TUCsTq7TofTCJRRoHk6RvhMoS2mJLm5Yzq"); ok {
		t.Fatal("the TRON address listed under XBT is in btc")
	}
	// USDC on Solana is paid from Solana wallets
	if len(entries[cryptosanctionslist.AssetSol]) != 3 || !sameEntries(entries[cryptosanctionslist.AssetSol], entries[cryptosanctionslist.AssetUsdcSolana]) {
		t.Fatalf("sol %v, usdc_solana %v", entries[cryptosanctionslist.AssetSol], entries[cryptosanctionslist.AssetUsdcSolana])
	}
	if list, ok := entries[cryptosanctionslist.AssetTao]; !ok || len(list) != 0 {
		t.Fatalf("tao %v", list)
	}
	// nothing is left beside the table
	leftovers, err := filepath.Glob(filepath.Join(filepath.Dir(output), ".cryptoctl-*"))
	if err != nil || len(leftovers) != 0 {
		t.Fatalf("stages left behind: %v %v", leftovers, err)
	}
}

func sameEntries(a []cryptosanctionslist.Entry, b []cryptosanctionslist.Entry) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Address != b[i].Address {
			return false
		}
	}
	return true
}

func TestUpdateIsDeterministic(t *testing.T) {
	first := filepath.Join(t.TempDir(), "2026.11.10+1")
	second := filepath.Join(t.TempDir(), "2026.11.10+1")
	for _, output := range []string{first, second} {
		if _, err := runUpdateCommand(t, testDependencies(), "--source", recordedSource, "--output", output); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{cryptosanctionslist.AddressesName, cryptosanctionslist.ManifestName} {
		a, _ := os.ReadFile(filepath.Join(first, name))
		b, _ := os.ReadFile(filepath.Join(second, name))
		if len(a) == 0 || !bytes.Equal(a, b) {
			t.Fatalf("%s differs between runs", name)
		}
	}
}

// Serves the recorded source the way the Sanctions List Service does: the
// export path redirects to the publication's storage URL.
func newSourceServer(t *testing.T, serve func(http.ResponseWriter, *http.Request)) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	requests := &atomic.Int64{}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/PublicationPreview/exports/SDN.XML", func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("User-Agent") != "urnetwork-cryptoctl/development" {
			http.Error(w, "no user agent", http.StatusBadRequest)
			return
		}
		http.Redirect(w, r, "/Published/2026-11-09/SDN.XML?X-Amz-Expires=3600", http.StatusFound)
	})
	mux.HandleFunc("/Published/2026-11-09/SDN.XML", serve)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server, requests
}

func TestUpdateDownloads(t *testing.T) {
	source := readRecordedSource(t)
	server, requests := newSourceServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/xml")
		w.Write(source)
	})
	exportUrl := server.URL + "/api/PublicationPreview/exports/SDN.XML"
	downloaded := filepath.Join(t.TempDir(), "2026.11.10+1")
	if _, err := runUpdateCommand(t, testDependencies(), "--url", exportUrl, "--output", downloaded); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 1 {
		t.Fatalf("%d requests", requests.Load())
	}
	read := filepath.Join(t.TempDir(), "2026.11.10+1")
	if _, err := runUpdateCommand(t, testDependencies(), "--source", recordedSource, "--output", read); err != nil {
		t.Fatal(err)
	}
	a, _ := os.ReadFile(filepath.Join(downloaded, cryptosanctionslist.AddressesName))
	b, _ := os.ReadFile(filepath.Join(read, cryptosanctionslist.AddressesName))
	if len(a) == 0 || !bytes.Equal(a, b) {
		t.Fatal("the downloaded table differs from the one read from the file")
	}
	table, err := cryptosanctionslist.ReadTable(downloaded)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(source)
	if table.Manifest.Source.Url != exportUrl || table.Manifest.Source.Sha256 != hex.EncodeToString(sum[:]) || table.Manifest.Source.Bytes != int64(len(source)) {
		t.Fatalf("manifest source %+v", table.Manifest.Source)
	}
}

func TestUpdateRetriesDownloads(t *testing.T) {
	source := readRecordedSource(t)
	failures := &atomic.Int64{}
	failures.Store(2)
	server, requests := newSourceServer(t, func(w http.ResponseWriter, r *http.Request) {
		if 0 <= failures.Add(-1) {
			http.Error(w, "busy", http.StatusServiceUnavailable)
			return
		}
		w.Write(source)
	})
	exportUrl := server.URL + "/api/PublicationPreview/exports/SDN.XML"
	failed := filepath.Join(t.TempDir(), "2026.11.10+1")
	if _, err := runUpdateCommand(t, testDependencies(), "--url", exportUrl, "--attempts", "2", "--output", failed); err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("two attempts against two failures: %v", err)
	}
	if _, err := os.Lstat(failed); err == nil {
		t.Fatal("a failed update wrote its output")
	}
	failures.Store(2)
	output := filepath.Join(t.TempDir(), "2026.11.10+1")
	if _, err := runUpdateCommand(t, testDependencies(), "--url", exportUrl, "--attempts", "3", "--output", output); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 5 {
		t.Fatalf("%d requests, want 5", requests.Load())
	}
}

// Every refusal writes nothing: the last published table stays the newest.
func TestUpdateRefusesBadSources(t *testing.T) {
	source := string(readRecordedSource(t))
	onlyEthereum := `<?xml version="1.0" standalone="yes"?>
<sdnList xmlns="https://sanctionslistservice.ofac.treas.gov/api/PublicationPreview/exports/XML">
  <publshInformation><Publish_Date>11/09/2026</Publish_Date><Record_Count>1</Record_Count></publshInformation>
  <sdnEntry><uid>1</uid><lastName>ONE</lastName><sdnType>Entity</sdnType>
    <idList><id><uid>2</uid><idType>Digital Currency Address - ETH</idType><idNumber>0x1cab8177ace78b1b6b1c393371f4f2dcae40cbeb</idNumber></id></idList>
  </sdnEntry>
</sdnList>`
	files := map[string]string{
		"truncated":             source[:len(source)/2],
		"truncated after entry": source[:strings.LastIndex(source, "</sdnEntry>")+len("</sdnEntry>")],
		"record count":          strings.Replace(source, "<Record_Count>15</Record_Count>", "<Record_Count>16</Record_Count>", 1),
		"no publish date":       regexp.MustCompile(`(?s)<publshInformation>.*?</publshInformation>`).ReplaceAllString(source, ""),
		"publish date format":   strings.Replace(source, "<Publish_Date>11/09/2026</Publish_Date>", "<Publish_Date>2026-11-09</Publish_Date>", 1),
		"no identifiers":        strings.ReplaceAll(source, "Digital Currency Address - ", "Digital Currency Adress - "),
		"no bitcoin":            onlyEthereum,
		"not an sdn list":       "<html><body>Service Unavailable</body></html>",
		"empty":                 "",
	}
	for name, content := range files {
		dir := t.TempDir()
		path := filepath.Join(dir, "SDN.XML")
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		output := filepath.Join(dir, "crypto", "2026.11.10+1")
		if _, err := runUpdateCommand(t, testDependencies(), "--source", path, "--output", output); err == nil {
			t.Errorf("%s: updated", name)
		}
		if entries, _ := os.ReadDir(filepath.Join(dir, "crypto")); len(entries) != 0 {
			t.Errorf("%s: wrote %v", name, entries)
		}
	}

	served := map[string]func(http.ResponseWriter, *http.Request){
		"not found": func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) },
		"short body": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Length", "100000")
			w.Write([]byte(source[:1000]))
		},
		"truncated stream": func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(source[:len(source)/2]))
		},
	}
	for name, serve := range served {
		server, _ := newSourceServer(t, serve)
		dir := t.TempDir()
		output := filepath.Join(dir, "crypto", "2026.11.10+1")
		if _, err := runUpdateCommand(t, testDependencies(), "--url", server.URL+"/api/PublicationPreview/exports/SDN.XML", "--output", output); err == nil {
			t.Errorf("%s: updated", name)
		}
		if entries, _ := os.ReadDir(filepath.Join(dir, "crypto")); len(entries) != 0 {
			t.Errorf("%s: wrote %v", name, entries)
		}
	}
}

func TestUpdateRefusesBadArguments(t *testing.T) {
	existing := t.TempDir()
	cases := [][]string{
		{},
		{"--source", recordedSource},
		{"--source", recordedSource, "--output", existing},
		{"--source", recordedSource, "--url", DefaultSourceUrl, "--output", filepath.Join(t.TempDir(), "a")},
		{"--source", recordedSource, "--output", filepath.Join(t.TempDir(), "a"), "extra"},
		{"--source", recordedSource, "--output", filepath.Join(t.TempDir(), "a"), "--attempts", "0"},
	}
	for _, args := range cases {
		if _, err := runUpdateCommand(t, testDependencies(), args...); err == nil {
			t.Errorf("update %v succeeded", args)
		}
	}
	var stdout, stderr bytes.Buffer
	if err := runCommand(context.Background(), []string{"refresh"}, &stdout, &stderr, testDependencies()); err == nil {
		t.Error("an unknown command succeeded")
	}
}

// A table update writes is what the loader reads: placed as a release
// directory of a config tree, it answers Match.
func TestUpdateTableAnswersMatch(t *testing.T) {
	root := t.TempDir()
	if _, err := runUpdateCommand(t, testDependencies(), "--source", recordedSource, "--output", filepath.Join(root, "all", "crypto", "2026.11.10+1")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WARP_CONFIG_HOME", root)
	t.Setenv("WARP_ENV", "main")
	cases := []struct {
		asset   string
		address string
		matched bool
	}{
		{cryptosanctionslist.AssetBtc, "12QtD5BFwRsdNsAZY76UVE1xyCGNTojH9h", true},
		{cryptosanctionslist.AssetBtc, "16iWn2J1McqjToYLHSsAyS6En3QA8YQ91H", true},
		{cryptosanctionslist.AssetBtc, "BC1QV7K70U2ZYNVEM59U88CTDLAW7HC735D8XEP9RQ", true},
		{cryptosanctionslist.AssetBtc, "1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa", false},
		{cryptosanctionslist.AssetEth, "0x983A81CA6FB1E441266D2FBCB7D8E530AC2E05A2", true},
		{cryptosanctionslist.AssetEth, "0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAed", false},
		{cryptosanctionslist.AssetSol, "42RLPACwZPx3vYYmxSueqsogfynBDqXK298EDsNoyoHi", true},
		{cryptosanctionslist.AssetUsdcSolana, "42RLPACwZPx3vYYmxSueqsogfynBDqXK298EDsNoyoHi", true},
		{cryptosanctionslist.AssetUsdcSolana, "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v", false},
		{cryptosanctionslist.AssetXmr, "884Bz8UH63aYsjVdkfWfScRYWZGGNbjFL7pztqvWNSrtYT4reFSwyvkCj9KEGUtheHhhMUj87ciTBFyzoesrMJ4L1FvSoxL", true},
		{cryptosanctionslist.AssetZec, "t1g7wowvQ8gn2v8jrU1biyJ26sieNqNsBJy", true},
		{cryptosanctionslist.AssetTao, "5GrwvaEF5zXb26Fz9rcQpDWS57CtERHpNehXCPcNoHGKutQY", false},
	}
	for _, c := range cases {
		matched, version, err := cryptosanctions.Match(c.asset, c.address)
		if err != nil || matched != c.matched || version != "2026.11.10+1" {
			t.Errorf("Match(%s, %q) = %t, %q, %v; want %t", c.asset, c.address, matched, version, err, c.matched)
		}
	}
	// listed under XBT, but no Bitcoin address: it cannot be screened as one
	if _, _, err := cryptosanctions.Match(cryptosanctionslist.AssetBtc, "TUCsTq7TofTCJRRoHk6RvhMoS2mJLm5Yzq"); err == nil {
		t.Error("the TRON address screened as Bitcoin")
	}
}
