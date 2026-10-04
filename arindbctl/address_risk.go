package main

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/maxmind/mmdbwriter"
	"github.com/maxmind/mmdbwriter/mmdbtype"
)

// Address-level findings cover proxies, VPN and Tor exits that live inside
// otherwise legitimate access networks, where neither registration nor origin
// ASN can see them. Each list is a pinned reviewed snapshot with one reviewed
// category; the official Tor exit list is the first intended input.
type addressRiskSource struct {
	countryEvidenceSource `yaml:",inline" json:",inline"`
	Format                string `yaml:"format" json:"format"`
	Category              string `yaml:"category" json:"category"`
	Reason                string `yaml:"reason" json:"reason"`
}

const (
	maxAddressRiskEntries           = 1 << 20
	maxAddressRiskDecompressedBytes = 256 << 20
)

// address-list: one address or prefix per line. tor-exit-addresses: the
// TorDNSEL export, whose ExitAddress lines are measured egress addresses.
// rfc8805-geofeed: operator self-published CSV whose first column is a prefix,
// as used by relay/VPN egress publications.
var addressRiskFormats = []string{"address-list", "tor-exit-addresses", "rfc8805-geofeed"}

func addressRiskEntryText(format string, line string) (string, bool) {
	switch format {
	case "tor-exit-addresses":
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "ExitAddress" {
			return "", false
		}
		return fields[1], true
	case "rfc8805-geofeed":
		entry, _, _ := strings.Cut(line, ",")
		return strings.TrimSpace(entry), true
	default:
		return line, true
	}
}

// Comments and blank lines are skipped; IPv4 aliases inside IPv6 are rejected
// rather than silently remapped. Gzip input is accepted and bounded.
func readAddressRiskList(ctx context.Context, format string, raw io.Reader) ([]netip.Prefix, error) {
	reader, err := openBoundedEvidenceReader(raw, maxAddressRiskDecompressedBytes)
	if err != nil {
		return nil, err
	}
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), 4096)
	entries := []netip.Prefix{}
	seen := map[netip.Prefix]bool{}
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line, ok := addressRiskEntryText(format, line)
		if !ok {
			continue
		}
		prefix, err := netip.ParsePrefix(line)
		if err != nil {
			address, addressErr := netip.ParseAddr(line)
			if addressErr != nil {
				return nil, errors.New("address risk list has a malformed entry")
			}
			prefix = netip.PrefixFrom(address, address.BitLen())
		}
		if prefix != prefix.Masked() || prefix.Bits() == 0 || prefix.Addr().Is4In6() || prefix.Addr().IsUnspecified() || subscriberOriginAliasesIPv4(prefix) {
			return nil, errors.New("address risk list entry is not a canonical native network")
		}
		if seen[prefix] {
			continue
		}
		seen[prefix] = true
		entries = append(entries, prefix)
		if len(entries) > maxAddressRiskEntries {
			return nil, errors.New("address risk list exceeds its entry bound")
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, errors.New("address risk list is truncated or unreadable")
	}
	if len(entries) == 0 {
		return nil, errors.New("address risk list has no entries")
	}
	return entries, nil
}

// Applies one reviewed finding on top of whatever classification already
// covers the address. Risk is independent and cumulative; the address also
// loses any subscriber default or approval because its observed use is contrary.
func addressRiskRecord(existing mmdbtype.Map, source addressRiskSource) mmdbtype.Map {
	data := make(mmdbtype.Map, len(existing)+3)
	for key, value := range existing {
		data[key] = value
	}
	evidence, _ := data["network_risk_evidence"].(mmdbtype.Slice)
	data["network_risk_evidence"] = append(slices.Clone(evidence), mmdbtype.Map{
		"rule": mmdbtype.String("address-risk-" + source.Id), "category": mmdbtype.String(source.Category),
		"source": mmdbtype.String(source.Url), "reason": mmdbtype.String(source.Reason),
	})
	data["network_risk"], data["risk"] = mmdbtype.Bool(true), mmdbtype.Bool(true)
	ids, _ := data["address_risk_source_ids"].(mmdbtype.Slice)
	data["address_risk_source_ids"] = append(slices.Clone(ids), mmdbtype.String(source.Id))
	if state, _ := data["quality_state"].(mmdbtype.String); state == "subscriber" || state == "unknown" {
		data["quality_state"], data["non_quality"] = mmdbtype.String("excluded"), mmdbtype.Bool(true)
		data["reason"] = mmdbtype.String("reviewed address-level network-use evidence excludes subscriber use")
		delete(data, "subscriber_evidence_kind")
	}
	return data
}

func applyAddressRiskSources(ctx context.Context, writer *mmdbwriter.Tree, catalog subscriberOriginCatalog, catalogPath string) (int, error) {
	if len(catalog.AddressRiskSources) == 0 {
		return 0, nil
	}
	root, err := os.OpenRoot(filepath.Dir(catalogPath))
	if err != nil {
		return 0, err
	}
	defer root.Close()
	applied := 0
	for _, source := range catalog.AddressRiskSources {
		f, err := root.Open(source.File)
		if err != nil {
			return 0, err
		}
		entries, parseErr := readAddressRiskList(ctx, source.Format, f)
		closeErr := f.Close()
		if parseErr != nil {
			return 0, parseErr
		}
		if closeErr != nil {
			return 0, closeErr
		}
		for _, prefix := range entries {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
			_, network, _ := net.ParseCIDR(prefix.String())
			if err := writer.InsertFunc(network, func(existing mmdbtype.DataType) (mmdbtype.DataType, error) {
				base, ok := existing.(mmdbtype.Map)
				if !ok {
					return nil, errors.New("address risk requires an existing policy-two classification record")
				}
				return addressRiskRecord(base, source), nil
			}); err != nil {
				return 0, err
			}
			applied++
		}
	}
	return applied, nil
}
