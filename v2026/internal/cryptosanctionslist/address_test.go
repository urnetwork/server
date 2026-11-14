package cryptosanctionslist

import (
	"errors"
	"strings"
	"testing"
)

type normalizeCase struct {
	asset     string
	address   string
	canonical string
}

// Valid addresses and their canonical forms. The OFAC ones are from SDN.XML
// of 2026-11-09; the rest are the BIPs' and the chains' published vectors.
var validAddresses = []normalizeCase{
	// btc: base58check P2PKH and P2SH as given
	{AssetBtc, "1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa", "1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa"},
	{AssetBtc, "12QtD5BFwRsdNsAZY76UVE1xyCGNTojH9h", "12QtD5BFwRsdNsAZY76UVE1xyCGNTojH9h"},
	{AssetBtc, "33Kja69SQVc8kozpoP7Qw6HFtGxHkiWzTz", "33Kja69SQVc8kozpoP7Qw6HFtGxHkiWzTz"},
	// OFAC lists this one under BCH; it is the same string on Bitcoin
	{AssetBtc, "18M8bJWMzWHDBMxoLqjHHAffdRy4SrzkfB", "18M8bJWMzWHDBMxoLqjHHAffdRy4SrzkfB"},
	{AssetBtc, " 1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa\n", "1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa"},
	// btc: segwit in lowercase (BIP 173, BIP 350)
	{AssetBtc, "bc1qv7k70u2zynvem59u88ctdlaw7hc735d8xep9rq", "bc1qv7k70u2zynvem59u88ctdlaw7hc735d8xep9rq"},
	{AssetBtc, "BC1QW508D6QEJXTDG4Y5R3ZARVARY0C5XW7KV8F3T4", "bc1qw508d6qejxtdg4y5r3zarvary0c5xw7kv8f3t4"},
	{AssetBtc, "tb1qrp33g0q5c5txsp9arysrx4k6zdkfs4nce4xj0gdcccefvpysxf3q0sl5k7", "tb1qrp33g0q5c5txsp9arysrx4k6zdkfs4nce4xj0gdcccefvpysxf3q0sl5k7"},
	{AssetBtc, "bc1p0xlxvlhemja6c4dqv22uapctqupfhlxm9h8z3k2e72q4k9hcz7vqzk5jj0", "bc1p0xlxvlhemja6c4dqv22uapctqupfhlxm9h8z3k2e72q4k9hcz7vqzk5jj0"},
	{AssetBtc, "BC1SW50QGDZ25J", "bc1sw50qgdz25j"},
	{AssetBtc, "bc1zw508d6qejxtdg4y5r3zarvaryvaxxpcs", "bc1zw508d6qejxtdg4y5r3zarvaryvaxxpcs"},
	// eth: lowercase, EIP-55 case dropped
	{AssetEth, "0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAed", "0x5aaeb6053f3e94c9b9a09f33669435e7ef1beaed"},
	{AssetEth, "0X5AAEB6053F3E94C9B9A09F33669435E7EF1BEAED", "0x5aaeb6053f3e94c9b9a09f33669435e7ef1beaed"},
	{AssetEth, "5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAed", "0x5aaeb6053f3e94c9b9a09f33669435e7ef1beaed"},
	{AssetEth, "0x1CAb8177ACe78b1B6B1c393371F4f2dCAE40CbEB", "0x1cab8177ace78b1b6b1c393371f4f2dcae40cbeb"},
	// sol and usdc_solana: 32-byte keys as given
	{AssetSol, "11111111111111111111111111111111", "11111111111111111111111111111111"},
	{AssetSol, "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v", "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"},
	{AssetSol, "42RLPACwZPx3vYYmxSueqsogfynBDqXK298EDsNoyoHi", "42RLPACwZPx3vYYmxSueqsogfynBDqXK298EDsNoyoHi"},
	{AssetUsdcSolana, "42RLPACwZPx3vYYmxSueqsogfynBDqXK298EDsNoyoHi", "42RLPACwZPx3vYYmxSueqsogfynBDqXK298EDsNoyoHi"},
	// xmr: standard and subaddresses as given
	{AssetXmr, "44dZUJ7w1T3fKAvFW8XyXUVoAGSbFvXef2wcbnsjNKGWYorpLJBjth5VKSFhLGkpYKJb2J341tdZHBnbpv72WL7e8zuxfR2", "44dZUJ7w1T3fKAvFW8XyXUVoAGSbFvXef2wcbnsjNKGWYorpLJBjth5VKSFhLGkpYKJb2J341tdZHBnbpv72WL7e8zuxfR2"},
	{AssetXmr, "884Bz8UH63aYsjVdkfWfScRYWZGGNbjFL7pztqvWNSrtYT4reFSwyvkCj9KEGUtheHhhMUj87ciTBFyzoesrMJ4L1FvSoxL", "884Bz8UH63aYsjVdkfWfScRYWZGGNbjFL7pztqvWNSrtYT4reFSwyvkCj9KEGUtheHhhMUj87ciTBFyzoesrMJ4L1FvSoxL"},
	// zec: transparent as given, Sapling in lowercase
	{AssetZec, "t1g7wowvQ8gn2v8jrU1biyJ26sieNqNsBJy", "t1g7wowvQ8gn2v8jrU1biyJ26sieNqNsBJy"},
	// ZIP 320's example, and its TEX form below
	{AssetZec, "t1VmmGiyjVNeCjxDZzg7vZmd99WyzVby9yC", "t1VmmGiyjVNeCjxDZzg7vZmd99WyzVby9yC"},
	{AssetZec, "tex1s2rt77ggv6q989lr49rkgzmh5slsksa9khdgte", "tex1s2rt77ggv6q989lr49rkgzmh5slsksa9khdgte"},
	// librustzcash's Sapling encoding vectors (zcash_keys/src/encoding.rs)
	{AssetZec, "zs1qqqqqqqqqqqqqqqqqqcguyvaw2vjk4sdyeg0lc970u659lvhqq7t0np6hlup5lusxle75c8v35z", "zs1qqqqqqqqqqqqqqqqqqcguyvaw2vjk4sdyeg0lc970u659lvhqq7t0np6hlup5lusxle75c8v35z"},
	{AssetZec, "ZS1QYQSZQGPQYQSZQGPQYCGUYVAW2VJK4SDYEG0LC970U659LVHQQ7T0NP6HLUP5LUSXLE75UGUM9P", "zs1qyqszqgpqyqszqgpqycguyvaw2vjk4sdyeg0lc970u659lvhqq7t0np6hlup5lusxle75ugum9p"},
	{AssetZec, "ztestsapling1qqqqqqqqqqqqqqqqqqcguyvaw2vjk4sdyeg0lc970u659lvhqq7t0np6hlup5lusxle75ss7jnk", "ztestsapling1qqqqqqqqqqqqqqqqqqcguyvaw2vjk4sdyeg0lc970u659lvhqq7t0np6hlup5lusxle75ss7jnk"},
	// a ZIP 316 unified address test vector (zcash_address)
	{AssetZec, "u1l8xunezsvhq8fgzfl7404m450nwnd76zshscn6nfys7vyz2ywyh4cc5daaq0c7q2su5lqfh23sp7fkf3kt27ve5948mzpfdvckzaect2jtte308mkwlycj2u0eac077wu70vqcetkxf", "u1l8xunezsvhq8fgzfl7404m450nwnd76zshscn6nfys7vyz2ywyh4cc5daaq0c7q2su5lqfh23sp7fkf3kt27ve5948mzpfdvckzaect2jtte308mkwlycj2u0eac077wu70vqcetkxf"},
	// tao: the account, under prefix 42, whichever prefix or form names it
	{AssetTao, "5GrwvaEF5zXb26Fz9rcQpDWS57CtERHpNehXCPcNoHGKutQY", "5GrwvaEF5zXb26Fz9rcQpDWS57CtERHpNehXCPcNoHGKutQY"},
	{AssetTao, "15oF4uVJwmo4TdGW7VfQxNLavjCXviqxT9S1MgbjMNHr6Sp5", "5GrwvaEF5zXb26Fz9rcQpDWS57CtERHpNehXCPcNoHGKutQY"},
	{AssetTao, "0xd43593c715fdd31c61141abd04a99fd6822c8558854ccde39a5684e7a56da27d", "5GrwvaEF5zXb26Fz9rcQpDWS57CtERHpNehXCPcNoHGKutQY"},
	{AssetTao, "0XD43593C715FDD31C61141ABD04A99FD6822C8558854CCDE39A5684E7A56DA27D", "5GrwvaEF5zXb26Fz9rcQpDWS57CtERHpNehXCPcNoHGKutQY"},
}

// Addresses no chain rule accepts for the asset.
var invalidAddresses = []normalizeCase{
	{asset: AssetBtc, address: ""},
	{asset: AssetBtc, address: "   "},
	// a changed character breaks the checksum
	{asset: AssetBtc, address: "1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNb"},
	// OFAC lists this TRON address under XBT
	{asset: AssetBtc, address: "TUCsTq7TofTCJRRoHk6RvhMoS2mJLm5Yzq"},
	// Litecoin's versions
	{asset: AssetBtc, address: "LaizKtS5DUhPuP1nTQcc83MS7HwK6vk85z"},
	{asset: AssetBtc, address: "ltc1qr8ntsedq8tv0svmxqhzvdcdl5k7kntdmnhwep7"},
	// BIP 350's invalid addresses
	{asset: AssetBtc, address: "tc1p0xlxvlhemja6c4dqv22uapctqupfhlxm9h8z3k2e72q4k9hcz7vq5zuyut"},
	{asset: AssetBtc, address: "bc1p0xlxvlhemja6c4dqv22uapctqupfhlxm9h8z3k2e72q4k9hcz7vqh2y7hd"},
	{asset: AssetBtc, address: "tb1z0xlxvlhemja6c4dqv22uapctqupfhlxm9h8z3k2e72q4k9hcz7vqglt7rf"},
	{asset: AssetBtc, address: "BC1S0XLXVLHEMJA6C4DQV22UAPCTQUPFHLXM9H8Z3K2E72Q4K9HCZ7VQ54WELL"},
	{asset: AssetBtc, address: "bc1qw508d6qejxtdg4y5r3zarvary0c5xw7kemeawh"},
	{asset: AssetBtc, address: "tb1q0xlxvlhemja6c4dqv22uapctqupfhlxm9h8z3k2e72q4k9hcz7vq24jc47"},
	{asset: AssetBtc, address: "bc1p38j9r5y49hruaue7wxjce0updqjuyyx0kh56v8s25huc6995vvpql3jow4"},
	{asset: AssetBtc, address: "BC130XLXVLHEMJA6C4DQV22UAPCTQUPFHLXM9H8Z3K2E72Q4K9HCZ7VQ7ZWS8R"},
	{asset: AssetBtc, address: "bc1pw5dgrnzv"},
	{asset: AssetBtc, address: "bc1p0xlxvlhemja6c4dqv22uapctqupfhlxm9h8z3k2e72q4k9hcz7v8n0nx0muaewav253zgeav"},
	{asset: AssetBtc, address: "BC1QR508D6QEJXTDG4Y5R3ZARVARYV98GJ9P"},
	{asset: AssetBtc, address: "tb1p0xlxvlhemja6c4dqv22uapctqupfhlxm9h8z3k2e72q4k9hcz7vq47Zagq"},
	{asset: AssetBtc, address: "bc1p0xlxvlhemja6c4dqv22uapctqupfhlxm9h8z3k2e72q4k9hcz7v07qwwzcrf"},
	{asset: AssetBtc, address: "tb1p0xlxvlhemja6c4dqv22uapctqupfhlxm9h8z3k2e72q4k9hcz7vpggkg4j"},
	{asset: AssetBtc, address: "bc1gmk9yu"},
	{asset: AssetBtc, address: "0x5aaeb6053f3e94c9b9a09f33669435e7ef1beaed"},
	{asset: AssetEth, address: "0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAe"},
	{asset: AssetEth, address: "0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAedd"},
	{asset: AssetEth, address: "0xgaAeb6053F3E94C9b9A09f33669435E7Ef1BeAed"},
	{asset: AssetEth, address: "0x"},
	{asset: AssetEth, address: "1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa"},
	{asset: AssetSol, address: "0x5aaeb6053f3e94c9b9a09f33669435e7ef1beaed"},
	// 25 bytes, not a 32-byte key
	{asset: AssetSol, address: "TUCsTq7TofTCJRRoHk6RvhMoS2mJLm5Yzq"},
	{asset: AssetSol, address: "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt10"},
	{asset: AssetUsdcSolana, address: "1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa"},
	{asset: AssetXmr, address: "44dZUJ7w1T3fKAvFW8XyXUVoAGSbFvXef2wcbnsjNKGWYorpLJBjth5VKSFhLGkpYKJb2J341tdZHBnbpv72WL7e8zuxfR3"},
	{asset: AssetXmr, address: "44dZUJ7w1T3fKAvFW8XyXUVoAGSbFvXef2wcbnsjNKGWYorpLJBjth5VKSFhLGkpYKJb2J341tdZHBnbpv72WL7e8zuxfR"},
	// OFAC lists this 64-digit hex value under XMR
	{asset: AssetXmr, address: "5be5543ff73456ab9f2d207887e2af87322c651ea1a873c5b25b7ffae456c320"},
	{asset: AssetZec, address: "t1g7wowvQ8gn2v8jrU1biyJ26sieNqNsBJz"},
	{asset: AssetZec, address: "1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa"},
	{asset: AssetZec, address: "zs1qqqqqqqqqqqqqqqqqqcguyvaw2vjk4sdyeg0lc970u659lvhqq7t0np6hlup5lusxle75c8v35y"},
	{asset: AssetZec, address: "tex1s2rt77ggv6q989lr49rkgzmh5slsksa9khdgtf"},
	{asset: AssetZec, address: "u1l8xunezsvhq8fgzfl7404m450nwnd76zshscn6nfys7vyz2ywyh4cc5daaq0c7q2su5lqfh23sp7fkf3kt27ve5948mzpfdvckzaect2jtte308mkwlycj2u0eac077wu70vqcetkxg"},
	// a Sapling payload with Bitcoin's human-readable part
	{asset: AssetZec, address: "bc1qw508d6qejxtdg4y5r3zarvary0c5xw7kv8f3t4"},
	{asset: AssetTao, address: "5GrwvaEF5zXb26Fz9rcQpDWS57CtERHpNehXCPcNoHGKutQZ"},
	{asset: AssetTao, address: "0xd43593c715fdd31c61141abd04a99fd6822c8558854ccde39a5684e7a56da2"},
	{asset: AssetTao, address: "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"},
}

func TestNormalizeValidAddresses(t *testing.T) {
	for _, c := range validAddresses {
		canonical, err := Normalize(c.asset, c.address)
		if err != nil {
			t.Errorf("Normalize(%s, %q): %v", c.asset, c.address, err)
			continue
		}
		if canonical != c.canonical {
			t.Errorf("Normalize(%s, %q) = %q, want %q", c.asset, c.address, canonical, c.canonical)
		}
		// idempotent
		if again, err := Normalize(c.asset, canonical); err != nil || again != canonical {
			t.Errorf("Normalize(%s, %q) = %q, %v; not idempotent", c.asset, canonical, again, err)
		}
	}
}

func TestNormalizeInvalidAddresses(t *testing.T) {
	for _, c := range invalidAddresses {
		canonical, err := Normalize(c.asset, c.address)
		if err == nil {
			t.Errorf("Normalize(%s, %q) = %q, want an error", c.asset, c.address, canonical)
			continue
		}
		if !errors.Is(err, ErrInvalidAddress) {
			t.Errorf("Normalize(%s, %q): %v is not ErrInvalidAddress", c.asset, c.address, err)
		}
	}
}

func TestNormalizeUnknownAsset(t *testing.T) {
	for _, asset := range []string{"", "BTC", "usdc", "usdt", "ltc"} {
		if _, err := Normalize(asset, "1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa"); !errors.Is(err, ErrUnknownAsset) {
			t.Errorf("Normalize(%q): %v, want ErrUnknownAsset", asset, err)
		}
		if KnownAsset(asset) {
			t.Errorf("KnownAsset(%q)", asset)
		}
	}
	for _, asset := range Assets() {
		if !KnownAsset(asset) {
			t.Errorf("KnownAsset(%q) = false", asset)
		}
		if _, err := OfacCurrency(asset); err != nil {
			t.Errorf("OfacCurrency(%q): %v", asset, err)
		}
	}
}

// Every address of one chain is rejected by every other chain, so a table can
// file OFAC's identifiers by format without crossing chains.
func TestNormalizeChainsAreDisjoint(t *testing.T) {
	for _, c := range validAddresses {
		for _, asset := range Assets() {
			if assetChains[asset] == assetChains[c.asset] {
				continue
			}
			// a hex account id is TAO's alternate form only
			if c.asset == AssetTao && strings.HasPrefix(strings.ToLower(c.address), "0x") {
				continue
			}
			if canonical, err := Normalize(asset, c.address); err == nil {
				t.Errorf("%s address %q is also a %s address (%q)", c.asset, c.address, asset, canonical)
			}
		}
	}
}

// bech32 strings built here, so unified and TEX addresses, which have no
// short published vectors, are covered by construction.
func bech32EncodeForTest(hrp string, data []byte, variant int) string {
	constant := uint32(bech32Constant)
	if variant == bech32mVariant {
		constant = bech32mConstant
	}
	values := append(bech32HrpExpand(hrp), data...)
	polymod := bech32Polymod(append(values, 0, 0, 0, 0, 0, 0)) ^ constant
	out := hrp + "1"
	for _, value := range data {
		out += string(bech32Charset[value])
	}
	for i := 0; i < 6; i += 1 {
		out += string(bech32Charset[(polymod>>uint(5*(5-i)))&31])
	}
	return out
}

func TestNormalizeZcashBech32Forms(t *testing.T) {
	groups := func(size int) []byte {
		payload := make([]byte, size)
		for i := range payload {
			payload[i] = byte(i*7 + 3)
		}
		data, err := convertBits(payload, 8, 5, true)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	unified := bech32EncodeForTest("u", groups(80), bech32mVariant)
	if 90 < len(unified) {
		// beyond bech32's limit, as unified addresses are
		t.Logf("unified address of %d characters", len(unified))
	}
	tex := bech32EncodeForTest("tex", groups(20), bech32mVariant)
	valid := []string{unified, strings.ToUpper(unified), tex, bech32EncodeForTest("utest", groups(48), bech32mVariant)}
	for _, address := range valid {
		canonical, err := Normalize(AssetZec, address)
		if err != nil {
			t.Errorf("Normalize(zec, %q): %v", address, err)
			continue
		}
		if canonical != strings.ToLower(address) {
			t.Errorf("Normalize(zec, %q) = %q", address, canonical)
		}
	}
	invalid := []string{
		// the wrong checksum constant for each form
		bech32EncodeForTest("u", groups(80), bech32Variant),
		bech32EncodeForTest("tex", groups(20), bech32Variant),
		bech32EncodeForTest("zs", groups(43), bech32mVariant),
		// the wrong payload sizes
		bech32EncodeForTest("u", groups(40), bech32mVariant),
		bech32EncodeForTest("tex", groups(21), bech32mVariant),
		bech32EncodeForTest("zs", groups(42), bech32Variant),
	}
	for _, address := range invalid {
		if canonical, err := Normalize(AssetZec, address); err == nil {
			t.Errorf("Normalize(zec, %q) = %q, want an error", address, canonical)
		}
	}
	// the published Sapling vector re-encodes to itself here
	sapling := "zs1qyqszqgpqyqszqgpqycguyvaw2vjk4sdyeg0lc970u659lvhqq7t0np6hlup5lusxle75ugum9p"
	decoded, err := bech32Decode(sapling, bech32MaxLength)
	if err != nil {
		t.Fatal(err)
	}
	if again := bech32EncodeForTest(decoded.hrp, decoded.data, decoded.variant); again != sapling {
		t.Errorf("bech32 round trip %q, want %q", again, sapling)
	}
}

func TestConvertBitsRoundTrip(t *testing.T) {
	for size := 0; size < 70; size += 1 {
		payload := make([]byte, size)
		for i := range payload {
			payload[i] = byte(255 - i*13)
		}
		groups, err := convertBits(payload, 8, 5, true)
		if err != nil {
			t.Fatal(err)
		}
		back, err := convertBits(groups, 5, 8, false)
		if err != nil {
			t.Fatalf("size %d: %v", size, err)
		}
		if string(back) != string(payload) {
			t.Fatalf("size %d: round trip %x, want %x", size, back, payload)
		}
	}
}

// BIP 173's generic vectors, as ZIP 173 repeats them.
func TestBech32DecodeVectors(t *testing.T) {
	valid := []string{
		"A12UEL5L",
		"a12uel5l",
		"an83characterlonghumanreadablepartthatcontainsthenumber1andtheexcludedcharactersbio1tt5tgs",
		"abcdef1qpzry9x8gf2tvdw0s3jn54khce6mua7lmqqqxw",
		"11qqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqc8247j",
		"split1checkupstagehandshakeupstreamerranterredcaperred2y9e3w",
		"?1ezyfcl",
	}
	for _, s := range valid {
		decoded, err := bech32Decode(s, bech32MaxLength)
		if err != nil || decoded.variant != bech32Variant {
			t.Errorf("bech32Decode(%q) = %+v, %v", s, decoded, err)
		}
	}
	invalid := []string{
		"\x201nwldj5",
		"\x7f1axkwrx",
		"\x801eym55h",
		"pzry9x0s0muk",
		"1pzry9x0s0muk",
		"x1b4n0q5v",
		"li1dgmt3",
		"de1lg7wt\xff",
		"A1G7SGD8",
		"10a06t8",
		"1qzzfhee",
		// 91 characters
		"an84characterslonghumanreadablepartthatcontainsthenumber1andtheexcludedcharactersbio1569pvx",
	}
	for _, s := range invalid {
		if decoded, err := bech32Decode(s, bech32MaxLength); err == nil {
			t.Errorf("bech32Decode(%q) = %+v, want an error", s, decoded)
		}
	}
}
