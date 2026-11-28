package cryptosanctions

import (
	"flag"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026/internal/cryptosanctionslist"
)

var updateFixture = flag.Bool("update", false, "rewrite testdata/config from the fixture entries")

// The checked-in table's release directory under testdata/config/all/crypto.
const fixtureVersion = "2026.11.9+1"

// The fixture: SDN.XML of 2026-11-09, trimmed to a few addresses per asset,
// plus one synthetic TAO entry since OFAC lists no TAO address.
func fixtureManifest() cryptosanctionslist.Manifest {
	return cryptosanctionslist.Manifest{
		Generator: "cryptosanctions test fixture",
		Source: cryptosanctionslist.Source{
			Url:         "https://sanctionslistservice.ofac.treas.gov/api/PublicationPreview/exports/SDN.XML",
			PublishDate: "2026-11-09",
			RecordCount: 19563,
			Sha256:      "329a9b9c6e554518639e979c28c6867d00b72b4175352dd8869766092bb3ed0c",
			Bytes:       29555843,
			RetrievedAt: time.Date(2026, 11, 10, 22, 53, 35, 0, time.UTC),
		},
		OfacDigitalCurrencyIds: map[string]int{
			"ARB": 1, "BCH": 1, "BSC": 1, "ETH": 3, "SOL": 2, "TRX": 1,
			"USDC": 1, "USDT": 2, "XBT": 4, "XMR": 3, "ZEC": 1,
		},
		Unrecognized: []cryptosanctionslist.Unrecognized{{
			Asset:        cryptosanctionslist.AssetBtc,
			OfacCurrency: "XBT",
			Address:      "TUCsTq7TofTCJRRoHk6RvhMoS2mJLm5Yzq",
			SdnUid:       165924,
			Reason:       "a TRON address",
		}},
	}
}

func entry(address string, uid int, name string, currencies ...string) cryptosanctionslist.Entry {
	return cryptosanctionslist.Entry{
		Address:        address,
		OfacCurrencies: currencies,
		Sdn:            []cryptosanctionslist.SdnRef{{Uid: uid, Name: name}},
	}
}

func fixtureEntries() map[string][]cryptosanctionslist.Entry {
	solana := []cryptosanctionslist.Entry{
		entry("42RLPACwZPx3vYYmxSueqsogfynBDqXK298EDsNoyoHi", 56360, "SOKOLOVSKI, Rolan", "SOL"),
		entry("Fc1EwQUZyTEagaDvA1utHXCcZNyG1x2PLt2DfNu1cJdH", 57978, "RASHEVSKYI, Dmytro", "SOL"),
	}
	return map[string][]cryptosanctionslist.Entry{
		cryptosanctionslist.AssetBtc: {
			entry("12QtD5BFwRsdNsAZY76UVE1xyCGNTojH9h", 25308, "YAN, Xiaobing", "XBT"),
			// Omni USDT, on Bitcoin
			entry("16iWn2J1McqjToYLHSsAyS6En3QA8YQ91H", 33151, "SUEX OTC, S.R.O.", "USDT"),
			// listed under BCH, the same string on Bitcoin
			entry("18M8bJWMzWHDBMxoLqjHHAffdRy4SrzkfB", 30518, "SECONDEYE SOLUTION", "BCH"),
			entry("33Kja69SQVc8kozpoP7Qw6HFtGxHkiWzTz", 26323, "ZHENG, Guanghua", "XBT"),
			entry("bc1qv7k70u2zynvem59u88ctdlaw7hc735d8xep9rq", 31212, "SOUTHFRONT", "XBT"),
		},
		cryptosanctionslist.AssetEth: {
			entry("0x1cab8177ace78b1b6b1c393371f4f2dcae40cbeb", 24003, "MESRI, Behzad", "ETH"),
			entry("0x4f47bc496083c727c5fbe3ce9cdf2b0f6496270c", 42498, "SIM, Hyon Sop", "ARB", "BSC", "ETH"),
			entry("0x983a81ca6fb1e441266d2fbcb7d8e530ac2e05a2", 45314, "VALERIAN LABS, INC.", "ETH", "USDC", "USDT"),
		},
		cryptosanctionslist.AssetSol:        solana,
		cryptosanctionslist.AssetUsdcSolana: solana,
		cryptosanctionslist.AssetXmr: {
			entry("44dZUJ7w1T3fKAvFW8XyXUVoAGSbFvXef2wcbnsjNKGWYorpLJBjth5VKSFhLGkpYKJb2J341tdZHBnbpv72WL7e8zuxfR2", 18647, "ISIL KHORASAN", "XMR"),
			entry("884Bz8UH63aYsjVdkfWfScRYWZGGNbjFL7pztqvWNSrtYT4reFSwyvkCj9KEGUtheHhhMUj87ciTBFyzoesrMJ4L1FvSoxL", 31212, "SOUTHFRONT", "XMR"),
		},
		cryptosanctionslist.AssetZec: {
			entry("t1g7wowvQ8gn2v8jrU1biyJ26sieNqNsBJy", 29585, "KARASAVIDI, Dmitrii", "ZEC"),
		},
		cryptosanctionslist.AssetTao: {
			// Substrate's development key "Alice"; not an OFAC listing
			entry("5GrwvaEF5zXb26Fz9rcQpDWS57CtERHpNehXCPcNoHGKutQY", 0, "synthetic fixture entry", "TAO"),
		},
	}
}

func fixtureDir() string {
	return filepath.Join("testdata", "config", "all", "crypto", fixtureVersion)
}

// The checked-in table is exactly what the writer produces from the fixture,
// so it pins the format a release's reader must keep reading. With -update it
// is rewritten.
func TestFixtureTableIsCurrent(t *testing.T) {
	generated := t.TempDir()
	if err := cryptosanctionslist.WriteTable(generated, fixtureManifest(), fixtureEntries()); err != nil {
		t.Fatal(err)
	}
	if *updateFixture {
		if err := os.RemoveAll(filepath.Join("testdata", "config")); err != nil {
			t.Fatal(err)
		}
		copyTree(t, generated, fixtureDir())
	}
	for _, name := range []string{cryptosanctionslist.ManifestName, cryptosanctionslist.AddressesName} {
		want, err := os.ReadFile(filepath.Join(generated, name))
		if err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(filepath.Join(fixtureDir(), name))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(want) {
			t.Errorf("%s differs from the writer's output; run go test ./cryptosanctions -run TestFixtureTableIsCurrent -update", name)
		}
	}
}

func copyTree(t *testing.T, from string, to string) {
	t.Helper()
	err := filepath.WalkDir(from, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(from, path)
		if err != nil {
			return err
		}
		target := filepath.Join(to, rel)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, content, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// A config root holding a copy of the fixture tree, selected as this process's
// config mount.
func useFixtureConfig(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	copyTree(t, filepath.Join("testdata", "config"), root)
	t.Setenv("WARP_CONFIG_HOME", root)
	t.Setenv("WARP_ENV", "test")
	return root
}

// Writes a release directory with the fixture plus extra btc addresses.
func writeRelease(t *testing.T, root string, version string, btc ...string) string {
	t.Helper()
	entries := fixtureEntries()
	list := entries[cryptosanctionslist.AssetBtc]
	for _, address := range btc {
		list = append(list, entry(address, 1, "TEST", "XBT"))
	}
	sortEntries(list)
	entries[cryptosanctionslist.AssetBtc] = list
	stage := filepath.Join(root, "all", "crypto", "."+version)
	if err := os.MkdirAll(stage, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := cryptosanctionslist.WriteTable(stage, fixtureManifest(), entries); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "all", "crypto", version)
	if err := os.Rename(stage, dir); err != nil {
		t.Fatal(err)
	}
	return dir
}

func sortEntries(list []cryptosanctionslist.Entry) {
	for i := 1; i < len(list); i += 1 {
		for j := i; 0 < j && list[j].Address < list[j-1].Address; j -= 1 {
			list[j], list[j-1] = list[j-1], list[j]
		}
	}
}

func requireMatch(t *testing.T, asset string, address string, want bool, wantVersion string) {
	t.Helper()
	matched, version, err := Match(asset, address)
	if err != nil {
		t.Fatalf("Match(%s, %q): %v", asset, address, err)
	}
	if matched != want || version != wantVersion {
		t.Fatalf("Match(%s, %q) = %t, %q; want %t, %q", asset, address, matched, version, want, wantVersion)
	}
}

func TestMatchListedAddresses(t *testing.T) {
	useFixtureConfig(t)
	listed := map[string][]string{
		cryptosanctionslist.AssetBtc: {
			"12QtD5BFwRsdNsAZY76UVE1xyCGNTojH9h",
			"33Kja69SQVc8kozpoP7Qw6HFtGxHkiWzTz",
			" bc1qv7k70u2zynvem59u88ctdlaw7hc735d8xep9rq\n",
			"BC1QV7K70U2ZYNVEM59U88CTDLAW7HC735D8XEP9RQ",
			"16iWn2J1McqjToYLHSsAyS6En3QA8YQ91H",
			"18M8bJWMzWHDBMxoLqjHHAffdRy4SrzkfB",
		},
		cryptosanctionslist.AssetEth: {
			"0x1CAb8177ACe78b1B6B1c393371F4f2dCAE40CbEB",
			"0x1cab8177ace78b1b6b1c393371f4f2dcae40cbeb",
			"0X983A81CA6FB1E441266D2FBCB7D8E530AC2E05A2",
			"4f47bc496083c727c5fbe3ce9cdf2b0f6496270c",
		},
		cryptosanctionslist.AssetSol:        {"42RLPACwZPx3vYYmxSueqsogfynBDqXK298EDsNoyoHi", "Fc1EwQUZyTEagaDvA1utHXCcZNyG1x2PLt2DfNu1cJdH"},
		cryptosanctionslist.AssetUsdcSolana: {"42RLPACwZPx3vYYmxSueqsogfynBDqXK298EDsNoyoHi"},
		cryptosanctionslist.AssetXmr:        {"884Bz8UH63aYsjVdkfWfScRYWZGGNbjFL7pztqvWNSrtYT4reFSwyvkCj9KEGUtheHhhMUj87ciTBFyzoesrMJ4L1FvSoxL"},
		cryptosanctionslist.AssetZec:        {"t1g7wowvQ8gn2v8jrU1biyJ26sieNqNsBJy"},
		cryptosanctionslist.AssetTao: {
			"5GrwvaEF5zXb26Fz9rcQpDWS57CtERHpNehXCPcNoHGKutQY",
			// the same account under Polkadot's prefix, and as hex
			"15oF4uVJwmo4TdGW7VfQxNLavjCXviqxT9S1MgbjMNHr6Sp5",
			"0xd43593c715fdd31c61141abd04a99fd6822c8558854ccde39a5684e7a56da27d",
		},
	}
	for asset, addresses := range listed {
		for _, address := range addresses {
			requireMatch(t, asset, address, true, fixtureVersion)
		}
	}
}

func TestMatchUnlistedAddresses(t *testing.T) {
	useFixtureConfig(t)
	unlisted := map[string][]string{
		cryptosanctionslist.AssetBtc:        {"1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa", "bc1p0xlxvlhemja6c4dqv22uapctqupfhlxm9h8z3k2e72q4k9hcz7vqzk5jj0"},
		cryptosanctionslist.AssetEth:        {"0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAed"},
		cryptosanctionslist.AssetSol:        {"11111111111111111111111111111111"},
		cryptosanctionslist.AssetUsdcSolana: {"EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"},
		cryptosanctionslist.AssetXmr:        {"45ezvPejNsKYntjhyyeb7fbfzi1sxYQozc5Ffseh8jPGW4JZQQVXykNTfK5uGFiij7Hp3qPv3T5HCR7F3Fnmz2mzE1aKQwo"},
		cryptosanctionslist.AssetZec:        {"t1WSKwCDL1QYRRUrCCknEs5tDLhtGVYu9KM"},
		// Substrate's development key "Bob"
		cryptosanctionslist.AssetTao: {"5FHneW46xGXgs5mUiveU4sbTyGBzmstUspZC92UhjJM694ty"},
	}
	for asset, addresses := range unlisted {
		for _, address := range addresses {
			requireMatch(t, asset, address, false, fixtureVersion)
		}
	}
}

func TestMatchErrors(t *testing.T) {
	useFixtureConfig(t)
	for _, asset := range []string{"", "usdt", "BTC", "ltc"} {
		matched, version, err := Match(asset, "12QtD5BFwRsdNsAZY76UVE1xyCGNTojH9h")
		if err == nil || matched || version != "" {
			t.Errorf("Match(%q) = %t, %q, %v; want an error", asset, matched, version, err)
		}
	}
	invalid := map[string]string{
		// OFAC lists this TRON address under XBT: it cannot be screened as Bitcoin
		cryptosanctionslist.AssetBtc:        "TUCsTq7TofTCJRRoHk6RvhMoS2mJLm5Yzq",
		cryptosanctionslist.AssetEth:        "12QtD5BFwRsdNsAZY76UVE1xyCGNTojH9h",
		cryptosanctionslist.AssetSol:        "0x1cab8177ace78b1b6b1c393371f4f2dcae40cbeb",
		cryptosanctionslist.AssetUsdcSolana: "",
		cryptosanctionslist.AssetXmr:        "5be5543ff73456ab9f2d207887e2af87322c651ea1a873c5b25b7ffae456c320",
		cryptosanctionslist.AssetZec:        "t1g7wowvQ8gn2v8jrU1biyJ26sieNqNsBJz",
		cryptosanctionslist.AssetTao:        "5GrwvaEF5zXb26Fz9rcQpDWS57CtERHpNehXCPcNoHGKutQZ",
	}
	for asset, address := range invalid {
		matched, version, err := Match(asset, address)
		if err == nil || matched {
			t.Errorf("Match(%s, %q) = %t, %v; want an error", asset, address, matched, err)
		}
		// the table was read, so the error still names it
		if version != fixtureVersion {
			t.Errorf("Match(%s, %q) version %q", asset, address, version)
		}
	}
}

func TestMatchWithoutTable(t *testing.T) {
	root := t.TempDir()
	t.Setenv("WARP_CONFIG_HOME", root)
	t.Setenv("WARP_ENV", "test")
	check := func() {
		t.Helper()
		matched, version, err := Match(cryptosanctionslist.AssetBtc, "12QtD5BFwRsdNsAZY76UVE1xyCGNTojH9h")
		if err == nil || matched || version != "" {
			t.Fatalf("Match = %t, %q, %v; want an error", matched, version, err)
		}
	}
	check()
	// a crypto directory with no release in it
	if err := os.MkdirAll(filepath.Join(root, "all", "crypto"), 0o755); err != nil {
		t.Fatal(err)
	}
	check()
	// a release directory without a manifest
	if err := os.MkdirAll(filepath.Join(root, "all", "crypto", "2026.11.10+1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "all", "crypto", "2026.11.10+1", cryptosanctionslist.AddressesName), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	check()
}

// The newest release directory answers; a new one is read at the next call,
// and removing it returns to the one before.
func TestMatchFollowsNewestReleaseDirectory(t *testing.T) {
	root := useFixtureConfig(t)
	older := writeRelease(t, root, "2026.11.8+1", "1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa")
	requireMatch(t, cryptosanctionslist.AssetBtc, "1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa", false, fixtureVersion)

	newer := writeRelease(t, root, "2026.11.10+1", "1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa")
	requireMatch(t, cryptosanctionslist.AssetBtc, "1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa", true, "2026.11.10+1")
	requireMatch(t, cryptosanctionslist.AssetBtc, "12QtD5BFwRsdNsAZY76UVE1xyCGNTojH9h", true, "2026.11.10+1")

	// a later build of the same day sorts after it
	writeRelease(t, root, "2026.11.10+2")
	requireMatch(t, cryptosanctionslist.AssetBtc, "1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa", false, "2026.11.10+2")

	// the release build removes the older directories
	for _, dir := range []string{older, newer, filepath.Join(root, "all", "crypto", fixtureVersion)} {
		if err := os.RemoveAll(dir); err != nil {
			t.Fatal(err)
		}
	}
	requireMatch(t, cryptosanctionslist.AssetBtc, "12QtD5BFwRsdNsAZY76UVE1xyCGNTojH9h", true, "2026.11.10+2")

	if err := os.RemoveAll(filepath.Join(root, "all", "crypto", "2026.11.10+2")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Match(cryptosanctionslist.AssetBtc, "12QtD5BFwRsdNsAZY76UVE1xyCGNTojH9h"); err == nil {
		t.Fatal("Match with no table left succeeded")
	}
}

// An unreadable newest table is an error, never the older table in its place.
func TestMatchRefusesUnreadableNewestTable(t *testing.T) {
	root := useFixtureConfig(t)
	newest := writeRelease(t, root, "2026.11.10+1")
	addressesPath := filepath.Join(newest, cryptosanctionslist.AddressesName)
	content, err := os.ReadFile(addressesPath)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(){
		"changed": func() {
			os.WriteFile(addressesPath, []byte(strings.Replace(string(content), "YAN, Xiaobing", "YAN, Xiaobinh", 1)), 0o644)
		},
		"missing": func() { os.Remove(addressesPath) },
		"truncated": func() {
			os.WriteFile(addressesPath, content[:len(content)/2], 0o644)
		},
		"other format": func() {
			manifestPath := filepath.Join(newest, cryptosanctionslist.ManifestName)
			manifest, _ := os.ReadFile(manifestPath)
			os.WriteFile(manifestPath, []byte(strings.Replace(string(manifest), cryptosanctionslist.Format, "urnetwork-crypto-sanctions-0", 1)), 0o644)
			os.WriteFile(addressesPath, content, 0o644)
		},
	}
	for name, damage := range cases {
		damage()
		matched, version, err := Match(cryptosanctionslist.AssetBtc, "12QtD5BFwRsdNsAZY76UVE1xyCGNTojH9h")
		if err == nil || matched || version != "" {
			t.Errorf("%s: Match = %t, %q, %v; want an error", name, matched, version, err)
		}
		// restore a good table
		os.RemoveAll(newest)
		writeRelease(t, root, "2026.11.10+1")
		requireMatch(t, cryptosanctionslist.AssetBtc, "12QtD5BFwRsdNsAZY76UVE1xyCGNTojH9h", true, "2026.11.10+1")
	}
}

// Once its resolution is settled, a table is read once and served from the
// cache; a new release directory or a rewritten addresses.json reads again.
func TestMatchCachesTable(t *testing.T) {
	root := useFixtureConfig(t)
	// the resolver caches a resolution only once every entry it observed has
	// been unchanged for its racy interval (2 s)
	time.Sleep(2500 * time.Millisecond)
	before := tableReads.Load()
	for i := 0; i < 5; i += 1 {
		requireMatch(t, cryptosanctionslist.AssetEth, "0x1cab8177ace78b1b6b1c393371f4f2dcae40cbeb", true, fixtureVersion)
	}
	if reads := tableReads.Load() - before; reads != 1 {
		t.Fatalf("%d table reads for five calls, want 1", reads)
	}

	// rewritten in place: read again, and refused since the digest no longer matches
	addressesPath := filepath.Join(root, "all", "crypto", fixtureVersion, cryptosanctionslist.AddressesName)
	content, err := os.ReadFile(addressesPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(addressesPath, append(content, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Match(cryptosanctionslist.AssetEth, "0x1cab8177ace78b1b6b1c393371f4f2dcae40cbeb"); err == nil {
		t.Fatal("Match served a table whose addresses.json changed")
	}
	if err := os.WriteFile(addressesPath, content, 0o644); err != nil {
		t.Fatal(err)
	}
	requireMatch(t, cryptosanctionslist.AssetEth, "0x1cab8177ace78b1b6b1c393371f4f2dcae40cbeb", true, fixtureVersion)

	before = tableReads.Load()
	writeRelease(t, root, "2026.11.10+1", "1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa")
	requireMatch(t, cryptosanctionslist.AssetBtc, "1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa", true, "2026.11.10+1")
	if reads := tableReads.Load() - before; reads < 1 {
		t.Fatal("a new release directory was not read")
	}
}
