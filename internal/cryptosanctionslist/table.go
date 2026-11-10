package cryptosanctionslist

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"time"
)

// A table directory holds two files. addresses.json lists each asset's
// canonical addresses, one entry per line with the OFAC currency codes and SDN
// entries that list it, every asset present (an asset with none as []).
// manifest.json says where the addresses came from and carries the SHA-256 of
// addresses.json, which a reader verifies before using any address.
const (
	ManifestName  = "manifest.json"
	AddressesName = "addresses.json"

	// A reader refuses a table of any other format.
	Format = "urnetwork-crypto-sanctions-1"
)

// Bounds what a reader reads, far above a real table (about 100 KB).
const (
	maxManifestBytes  = 16 * 1024 * 1024
	maxAddressesBytes = 256 * 1024 * 1024
)

type Manifest struct {
	Format string `json:"format"`
	// the tool and version that wrote the table
	Generator string `json:"generator"`
	Source    Source `json:"source"`
	// addresses.json, as written
	Addresses FileDigest `json:"addresses"`
	// every asset: its number of addresses, and how many of them OFAC lists
	// under each currency code (an address under two codes counts in both)
	Assets map[string]AssetSummary `json:"assets"`
	// every Digital Currency Address identifier in the source, by OFAC's
	// currency code, including the codes of coins no table covers
	OfacDigitalCurrencyIds map[string]int `json:"ofac_digital_currency_ids"`
	// identifiers under an asset's own currency code that are not valid
	// addresses on its chain; they are reported here and never in
	// addresses.json, since no valid address can equal them
	Unrecognized []Unrecognized `json:"unrecognized"`
}

type Source struct {
	// the export read
	Url string `json:"url"`
	// OFAC's Publish_Date, as YYYY-MM-DD
	PublishDate string `json:"publish_date"`
	// OFAC's Record_Count, the number of SDN entries
	RecordCount int `json:"record_count"`
	// of the bytes read, lowercase hex
	Sha256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
	// when the source was read
	RetrievedAt time.Time `json:"retrieved_at"`
}

type FileDigest struct {
	Name string `json:"name"`
	// lowercase hex
	Sha256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

type AssetSummary struct {
	Count          int            `json:"count"`
	OfacCurrencies map[string]int `json:"ofac_currencies"`
}

type Unrecognized struct {
	Asset        string `json:"asset"`
	OfacCurrency string `json:"ofac_currency"`
	Address      string `json:"address"`
	SdnUid       int    `json:"sdn_uid"`
	Reason       string `json:"reason"`
}

// One listed address of an asset.
type Entry struct {
	// canonical (Normalize)
	Address string `json:"address"`
	// the OFAC currency codes that list it, sorted
	OfacCurrencies []string `json:"ofac_currencies"`
	// the SDN entries that list it, by uid
	Sdn []SdnRef `json:"sdn"`
}

// An SDN entry. Its page is https://sanctionssearch.ofac.treas.gov/Details.aspx?id=<uid>.
type SdnRef struct {
	Uid  int    `json:"uid"`
	Name string `json:"name"`
}

// Writes addresses.json and then manifest.json into the existing directory
// dir. Entries must be canonical, unique and sorted by address within each
// asset; assets missing from entries are written empty. The manifest's
// Format, Addresses and Assets are set here, the rest is the caller's.
func WriteTable(dir string, manifest Manifest, entries map[string][]Entry) error {
	assets := Assets()
	for asset := range entries {
		if !slices.Contains(assets, asset) {
			return fmt.Errorf("%w %q", ErrUnknownAsset, asset)
		}
	}
	manifest.Format = Format
	manifest.Assets = map[string]AssetSummary{}
	var content bytes.Buffer
	content.WriteString("{\n")
	for i, asset := range assets {
		list := entries[asset]
		summary := AssetSummary{Count: len(list), OfacCurrencies: map[string]int{}}
		fmt.Fprintf(&content, "  %q: [", asset)
		for j, entry := range list {
			canonical, err := Normalize(asset, entry.Address)
			if err != nil {
				return err
			}
			if canonical != entry.Address {
				return fmt.Errorf("%s address %q is not canonical (%q)", asset, entry.Address, canonical)
			}
			if 0 < j && entry.Address <= list[j-1].Address {
				return fmt.Errorf("%s addresses are not sorted and unique at %q", asset, entry.Address)
			}
			if len(entry.OfacCurrencies) == 0 || len(entry.Sdn) == 0 {
				return fmt.Errorf("%s address %q has no OFAC listing", asset, entry.Address)
			}
			for _, currency := range entry.OfacCurrencies {
				summary.OfacCurrencies[currency] += 1
			}
			line, err := encodeJson(entry, "")
			if err != nil {
				return err
			}
			content.WriteString("\n    ")
			content.Write(line)
			if j < len(list)-1 {
				content.WriteString(",")
			}
		}
		if 0 < len(list) {
			content.WriteString("\n  ")
		}
		content.WriteString("]")
		if i < len(assets)-1 {
			content.WriteString(",")
		}
		content.WriteString("\n")
		manifest.Assets[asset] = summary
	}
	content.WriteString("}\n")
	sum := sha256.Sum256(content.Bytes())
	manifest.Addresses = FileDigest{
		Name:   AddressesName,
		Sha256: hex.EncodeToString(sum[:]),
		Bytes:  int64(content.Len()),
	}
	if manifest.OfacDigitalCurrencyIds == nil {
		manifest.OfacDigitalCurrencyIds = map[string]int{}
	}
	if manifest.Unrecognized == nil {
		manifest.Unrecognized = []Unrecognized{}
	}
	manifestContent, err := encodeJson(manifest, "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, AddressesName), content.Bytes(), 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, ManifestName), append(manifestContent, '\n'), 0o644)
}

// JSON without HTML escaping, so names keep their "&", and without the
// encoder's trailing newline.
func encodeJson(value any, indent string) ([]byte, error) {
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	if indent != "" {
		encoder.SetIndent("", indent)
	}
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(out.Bytes(), []byte("\n")), nil
}

// A table as read: its manifest and each asset's canonical addresses.
// Immutable, so safe for concurrent use.
type Table struct {
	Manifest Manifest
	// entries this reader's rules no longer accept, as "asset address:
	// reason"; none can equal an address the rules accept
	Skipped   []string
	addresses map[string]map[string]struct{}
}

// Whether the table lists the canonical address for asset.
func (self *Table) Contains(asset string, canonical string) bool {
	_, ok := self.addresses[asset][canonical]
	return ok
}

// The number of addresses the table lists for asset.
func (self *Table) Count(asset string) int {
	return len(self.addresses[asset])
}

var sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Reads and verifies the table in dir. The manifest must be of this Format and
// cover every asset, and addresses.json must have the manifest's size and
// SHA-256 and, per asset, its count of entries. Addresses are normalized again
// under this reader's rules.
func ReadTable(dir string) (*Table, error) {
	manifestContent, err := readBounded(filepath.Join(dir, ManifestName), maxManifestBytes)
	if err != nil {
		return nil, err
	}
	var manifest Manifest
	if err := json.Unmarshal(manifestContent, &manifest); err != nil {
		return nil, fmt.Errorf("unreadable %s: %w", ManifestName, err)
	}
	if manifest.Format != Format {
		return nil, fmt.Errorf("%s has format %q, want %q", ManifestName, manifest.Format, Format)
	}
	if manifest.Addresses.Name != AddressesName || !sha256Pattern.MatchString(manifest.Addresses.Sha256) ||
		manifest.Addresses.Bytes <= 0 || maxAddressesBytes < manifest.Addresses.Bytes {
		return nil, fmt.Errorf("%s names no valid %s digest", ManifestName, AddressesName)
	}
	content, err := readBounded(filepath.Join(dir, AddressesName), maxAddressesBytes)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(content)
	if int64(len(content)) != manifest.Addresses.Bytes || hex.EncodeToString(sum[:]) != manifest.Addresses.Sha256 {
		return nil, fmt.Errorf("%s does not match the digest in %s", AddressesName, ManifestName)
	}
	var lists map[string][]Entry
	if err := json.Unmarshal(content, &lists); err != nil {
		return nil, fmt.Errorf("unreadable %s: %w", AddressesName, err)
	}
	table := &Table{
		Manifest:  manifest,
		Skipped:   []string{},
		addresses: map[string]map[string]struct{}{},
	}
	for _, asset := range Assets() {
		list, ok := lists[asset]
		if !ok {
			return nil, fmt.Errorf("%s has no %s list", AddressesName, asset)
		}
		summary, ok := manifest.Assets[asset]
		if !ok {
			return nil, fmt.Errorf("%s has no %s summary", ManifestName, asset)
		}
		if len(list) != summary.Count {
			return nil, fmt.Errorf("%s lists %d %s addresses, %s says %d", AddressesName, len(list), asset, ManifestName, summary.Count)
		}
		addresses := map[string]struct{}{}
		for _, entry := range list {
			canonical, err := Normalize(asset, entry.Address)
			if err != nil {
				table.Skipped = append(table.Skipped, fmt.Sprintf("%s %s: %s", asset, entry.Address, err.Error()))
				continue
			}
			addresses[canonical] = struct{}{}
		}
		table.addresses[asset] = addresses
	}
	return table, nil
}

func readBounded(path string, limit int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	content, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if limit < int64(len(content)) {
		return nil, fmt.Errorf("%s exceeds %d bytes", path, limit)
	}
	return content, nil
}
