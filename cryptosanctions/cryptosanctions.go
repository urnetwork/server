// Package cryptosanctions screens crypto addresses against OFAC's SDN list
// (mmm/BTCPAY.md §17.3, §17.5): Bitcoin inputs, ETH and SOL senders, and every
// refund destination are checked before credit, and a match is held for review.
//
// The table is generated from OFAC's own SDN data by cryptoctl, which keeps
// the "Digital Currency Address" identifiers valid on the rail's chains. Every
// release build writes it to config/all/crypto/<release>/ (build/all/run.sh),
// the directory named by the release version, and removes the older ones.
// Processes read the newest release directory holding a manifest.json, found
// the way the arindb release directories are (server arindbctl/ARINDB.md,
// "Release directories"): Config resolves crypto/manifest.json in descending
// version order. addresses.json is read only beside that manifest, never
// resolved on its own, so a table never mixes two releases.
//
// The table is cached and read again once its resolution changes: a new or
// removed release directory, or a manifest written in place. A table that is
// missing or fails verification (cryptosanctionslist.ReadTable) is an error on
// every call until a readable one is in place, and so is an address that is not
// valid on the asset's chain: a caller holds the payment for review on any
// error. A table is never stale-served in place of an unreadable newer one.
package cryptosanctions

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/internal/cryptosanctionslist"
)

// The manifest of the newest release directory, config/all/crypto/<release>/.
const tableResource = "crypto/" + cryptosanctionslist.ManifestName

// Match reports whether address is on OFAC's list for asset (btc, eth, sol,
// usdc_solana, xmr, zec or tao), comparing each chain's canonical form: EVM
// addresses without case, bech32 in lowercase, base58 exactly, and TAO by its
// account under any SS58 prefix. listVersion names the table that answered,
// its release directory (as "2026.11.10+1234567890"), and is set whenever a
// table was read. An unknown asset, an address that is not valid on the
// asset's chain, and a missing or unreadable table are errors: hold the
// payment for review.
func Match(asset string, address string) (matched bool, listVersion string, err error) {
	if !cryptosanctionslist.KnownAsset(asset) {
		return false, "", fmt.Errorf("cryptosanctions: %w %q", cryptosanctionslist.ErrUnknownAsset, asset)
	}
	table, err := currentTable()
	if err != nil {
		return false, "", err
	}
	canonical, err := cryptosanctionslist.Normalize(asset, address)
	if err != nil {
		return false, table.version, fmt.Errorf("cryptosanctions: %w", err)
	}
	return table.list.Contains(asset, canonical), table.version, nil
}

// One table read from a release directory.
type servedTable struct {
	version string
	dir     string
	list    *cryptosanctionslist.Table
	// addresses.json as stat'ed before it was read; a rewrite reads again
	addressesInfo os.FileInfo
}

// A read is cached only when it succeeded.
type tableLoad struct {
	table *servedTable
	err   error
}

var tableCache server.ResourceValueCache[server.ResourceVersion, tableLoad]

// Counts table reads, for tests.
var tableReads atomic.Int64

func currentTable() (*servedTable, error) {
	resource, err := server.Config.SimpleResource(tableResource)
	if err != nil {
		return nil, fmt.Errorf("cryptosanctions: no OFAC address table (%s): %w", tableResource, err)
	}
	// The version is taken before the path is resolved again in readTable, so
	// a table read from a newer resolution is at worst read once more.
	load := tableCache.Get(resource.Version(), currentRead, readTable)
	return load.table, load.err
}

func currentRead(load tableLoad) bool {
	if load.table == nil {
		return false
	}
	info, err := os.Stat(filepath.Join(load.table.dir, cryptosanctionslist.AddressesName))
	return err == nil && os.SameFile(info, load.table.addressesInfo) &&
		info.Size() == load.table.addressesInfo.Size() && info.ModTime().Equal(load.table.addressesInfo.ModTime())
}

func readTable() (tableLoad, bool) {
	tableReads.Add(1)
	path, err := server.Config.ResourcePath(tableResource)
	if err != nil {
		return tableLoad{err: fmt.Errorf("cryptosanctions: no OFAC address table (%s): %w", tableResource, err)}, false
	}
	dir := filepath.Dir(path)
	version := filepath.Base(dir)
	addressesInfo, err := os.Stat(filepath.Join(dir, cryptosanctionslist.AddressesName))
	if err != nil {
		glog.Errorf("[cryptosanctions]OFAC address table %s is unreadable: %s\n", dir, err)
		return tableLoad{err: fmt.Errorf("cryptosanctions: unreadable OFAC address table %s: %w", version, err)}, false
	}
	list, err := cryptosanctionslist.ReadTable(dir)
	if err != nil {
		glog.Errorf("[cryptosanctions]OFAC address table %s is unreadable: %s\n", dir, err)
		return tableLoad{err: fmt.Errorf("cryptosanctions: unreadable OFAC address table %s: %w", version, err)}, false
	}
	for _, skipped := range list.Skipped {
		glog.Errorf("[cryptosanctions]%s: skipped %s\n", version, skipped)
	}
	counts := []string{}
	for _, asset := range cryptosanctionslist.Assets() {
		counts = append(counts, fmt.Sprintf("%s %d", asset, list.Count(asset)))
	}
	glog.Infof(
		"[cryptosanctions]serving OFAC address table %s (OFAC %s, sha256 %s): %s\n",
		version,
		list.Manifest.Source.PublishDate,
		list.Manifest.Source.Sha256,
		strings.Join(counts, ", "),
	)
	return tableLoad{table: &servedTable{
		version:       version,
		dir:           dir,
		list:          list,
		addressesInfo: addressesInfo,
	}}, true
}
