package main

import (
	"bufio"
	"context"
	"encoding/json"
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
// as used by relay/VPN egress publications. The JSON formats are VPN
// operators' own public server lists; on 2026-10-04 they named 10,861 server
// addresses, 26 of which sat inside identified access ISPs (NordVPN in
// Versatel and BT, Windscribe in LG U+ and SK Broadband).
var addressRiskFormats = []string{"address-list", "tor-exit-addresses", "rfc8805-geofeed", "mullvad-relays-json", "nordvpn-servers-json", "pia-servers-json", "windscribe-serverlist-json"}

// Formats describing operator-published ranges or third-party APIs skip
// non-global entries instead of failing the publication.
func addressRiskFormatSkipsNonGlobal(format string) bool {
	return format != "address-list" && format != "tor-exit-addresses"
}

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

// Server addresses from one VPN operator's JSON server list.
func readVpnServerAddresses(format string, reader io.Reader) ([]string, error) {
	addresses := []string{}
	switch format {
	case "mullvad-relays-json":
		var relays []struct {
			Ipv4 string `json:"ipv4_addr_in"`
			Ipv6 string `json:"ipv6_addr_in"`
		}
		if err := json.NewDecoder(reader).Decode(&relays); err != nil {
			return nil, errors.New("Mullvad relay list is malformed")
		}
		for _, relay := range relays {
			addresses = append(addresses, relay.Ipv4, relay.Ipv6)
		}
	case "nordvpn-servers-json":
		var servers []struct {
			Station     string `json:"station"`
			Ipv6Station string `json:"ipv6_station"`
			Ips         []struct {
				Ip struct {
					Ip string `json:"ip"`
				} `json:"ip"`
			} `json:"ips"`
		}
		if err := json.NewDecoder(reader).Decode(&servers); err != nil {
			return nil, errors.New("NordVPN server list is malformed")
		}
		for _, server := range servers {
			addresses = append(addresses, server.Station, server.Ipv6Station)
			for _, ip := range server.Ips {
				addresses = append(addresses, ip.Ip.Ip)
			}
		}
	case "pia-servers-json":
		// The list is one JSON line followed by its signature.
		line, err := bufio.NewReaderSize(reader, 64*1024).ReadString('\n')
		if err != nil && err != io.EOF {
			return nil, errors.New("PIA server list is unreadable")
		}
		var document struct {
			Regions []struct {
				Servers map[string][]struct {
					Ip string `json:"ip"`
				} `json:"servers"`
			} `json:"regions"`
		}
		if err := json.Unmarshal([]byte(line), &document); err != nil || len(document.Regions) == 0 {
			return nil, errors.New("PIA server list is malformed")
		}
		for _, region := range document.Regions {
			for _, servers := range region.Servers {
				for _, server := range servers {
					addresses = append(addresses, server.Ip)
				}
			}
		}
	case "windscribe-serverlist-json":
		var document struct {
			Data []struct {
				Groups []struct {
					Nodes []struct {
						Ip  string `json:"ip"`
						Ip2 string `json:"ip2"`
						Ip3 string `json:"ip3"`
					} `json:"nodes"`
				} `json:"groups"`
			} `json:"data"`
		}
		if err := json.NewDecoder(reader).Decode(&document); err != nil || len(document.Data) == 0 {
			return nil, errors.New("Windscribe server list is malformed")
		}
		for _, location := range document.Data {
			for _, group := range location.Groups {
				for _, node := range group.Nodes {
					addresses = append(addresses, node.Ip, node.Ip2, node.Ip3)
				}
			}
		}
	default:
		return nil, errors.New("unsupported VPN server list format")
	}
	return addresses, nil
}

// Comments and blank lines are skipped; IPv4 aliases inside IPv6 are rejected
// rather than silently remapped. Gzip input is accepted and bounded.
func readAddressRiskList(ctx context.Context, format string, raw io.Reader) ([]netip.Prefix, error) {
	reader, err := openBoundedEvidenceReader(raw, maxAddressRiskDecompressedBytes)
	if err != nil {
		return nil, err
	}
	entries := []netip.Prefix{}
	seen := map[netip.Prefix]bool{}
	add := func(text string) error {
		text = strings.TrimSpace(text)
		if text == "" {
			return nil
		}
		prefix, err := netip.ParsePrefix(text)
		if err != nil {
			address, addressErr := netip.ParseAddr(text)
			if addressErr != nil {
				return errors.New("address risk list has a malformed entry")
			}
			prefix = netip.PrefixFrom(address, address.BitLen())
		}
		if addressRiskFormatSkipsNonGlobal(format) {
			if nonGlobalPrefix(prefix.Masked()) {
				return nil
			}
			prefix = prefix.Masked()
		}
		if prefix != prefix.Masked() || prefix.Bits() == 0 || prefix.Addr().Is4In6() || prefix.Addr().IsUnspecified() || subscriberOriginAliasesIPv4(prefix) {
			return errors.New("address risk list entry is not a canonical native network")
		}
		if seen[prefix] {
			return nil
		}
		seen[prefix] = true
		entries = append(entries, prefix)
		if len(entries) > maxAddressRiskEntries {
			return errors.New("address risk list exceeds its entry bound")
		}
		return nil
	}
	if strings.HasSuffix(format, "-json") {
		addresses, err := readVpnServerAddresses(format, reader)
		if err != nil {
			return nil, err
		}
		for _, address := range addresses {
			if err := add(address); err != nil {
				return nil, err
			}
		}
	} else {
		scanner := bufio.NewScanner(reader)
		scanner.Buffer(make([]byte, 4096), 4096)
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
			if err := add(line); err != nil {
				return nil, err
			}
		}
		if err := scanner.Err(); err != nil {
			return nil, errors.New("address risk list is truncated or unreadable")
		}
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
