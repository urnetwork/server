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

// Cloud operators publish the prefixes their tenant compute uses. That is
// hosting evidence at prefix scope, and it matters exactly where a cloud
// prefix is originated by an access network: on 2026-10-04, 52 AWS EC2
// Wavelength prefixes were originated by Verizon Wireless AS6167 and an
// Oracle block by Cox AS22773, so the identified-ISP inference alone would
// approve rented compute. Hosting excludes Quality; it adds no network risk.
type hostingPrefixSource struct {
	countryEvidenceSource `yaml:",inline" json:",inline"`
	Format                string   `yaml:"format" json:"format"`
	Services              []string `yaml:"services,omitempty" json:"services,omitempty"`
	Reason                string   `yaml:"reason" json:"reason"`
}

const (
	hostingFormatAwsIpRanges    = "aws-ip-ranges-json"
	hostingFormatGcpCloud       = "gcp-cloud-json"
	hostingFormatAzureTags      = "azure-service-tags-json"
	hostingFormatOracleRanges   = "oracle-public-ip-ranges-json"
	hostingFormatGeofeed        = "rfc8805-geofeed"
	maxHostingDecompressedBytes = 256 << 20
)

var hostingFormats = []string{hostingFormatAwsIpRanges, hostingFormatGcpCloud, hostingFormatAzureTags, hostingFormatOracleRanges, hostingFormatGeofeed}

// AWS and Azure lists mix tenant compute with provider services, so they
// require an explicit reviewed service selection; the others are tenant lists.
func validHostingPrefixSource(source hostingPrefixSource) bool {
	if !slices.Contains(hostingFormats, source.Format) || strings.TrimSpace(source.Reason) == "" {
		return false
	}
	needsServices := source.Format == hostingFormatAwsIpRanges || source.Format == hostingFormatAzureTags
	if needsServices != (len(source.Services) != 0) {
		return false
	}
	for _, service := range source.Services {
		if strings.TrimSpace(service) == "" || strings.TrimSpace(service) != service {
			return false
		}
	}
	return true
}

// Published lists contain non-global space: Vultr's feed lists 6to4 and
// Teredo, and providers list ORCHID or private ranges. Those entries can
// neither be routed nor be inserted beside the MMDB's IPv4 aliases, so they
// are skipped and counted rather than failing the whole publication.
var nonGlobalPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("169.254.0.0/16"), netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"), netip.MustParsePrefix("198.18.0.0/15"), netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("240.0.0.0/4"), netip.MustParsePrefix("2001:2::/48"), netip.MustParsePrefix("2001:10::/28"),
	netip.MustParsePrefix("2001:20::/28"), netip.MustParsePrefix("fc00::/7"), netip.MustParsePrefix("fe80::/10"),
	netip.MustParsePrefix("ff00::/8"),
}

func nonGlobalPrefix(prefix netip.Prefix) bool {
	if prefix.Bits() == 0 || prefix.Addr().Is4In6() || subscriberOriginAliasesIPv4(prefix) {
		return true
	}
	for _, special := range nonGlobalPrefixes {
		if special.Overlaps(prefix) {
			return true
		}
	}
	return false
}

// Collects canonical prefixes, skipping non-global ones. A malformed entry
// fails the source: a list we cannot parse is not evidence.
type hostingPrefixCollector struct {
	prefixes []netip.Prefix
	seen     map[netip.Prefix]bool
	skipped  int
}

func (self *hostingPrefixCollector) add(text string) error {
	prefix, err := netip.ParsePrefix(strings.TrimSpace(text))
	if err != nil {
		address, addressErr := netip.ParseAddr(strings.TrimSpace(text))
		if addressErr != nil {
			return errors.New("hosting prefix list has a malformed prefix")
		}
		prefix = netip.PrefixFrom(address, address.BitLen())
	}
	prefix = prefix.Masked()
	if nonGlobalPrefix(prefix) {
		self.skipped++
		return nil
	}
	if self.seen == nil {
		self.seen = map[netip.Prefix]bool{}
	}
	if !self.seen[prefix] {
		self.seen[prefix] = true
		self.prefixes = append(self.prefixes, prefix)
		if len(self.prefixes) > maxAddressRiskEntries {
			return errors.New("hosting prefix list exceeds its entry bound")
		}
	}
	return nil
}

func readHostingPrefixes(ctx context.Context, source hostingPrefixSource, raw io.Reader) ([]netip.Prefix, int, error) {
	reader, err := openBoundedEvidenceReader(raw, maxHostingDecompressedBytes)
	if err != nil {
		return nil, 0, err
	}
	collector := &hostingPrefixCollector{}
	wanted := func(service string) bool { return slices.Contains(source.Services, service) }
	switch source.Format {
	case hostingFormatAwsIpRanges:
		var document struct {
			CreateDate string `json:"createDate"`
			Prefixes   []struct {
				Prefix  string `json:"ip_prefix"`
				Service string `json:"service"`
			} `json:"prefixes"`
			Ipv6Prefixes []struct {
				Prefix  string `json:"ipv6_prefix"`
				Service string `json:"service"`
			} `json:"ipv6_prefixes"`
		}
		if err := json.NewDecoder(reader).Decode(&document); err != nil || document.CreateDate == "" || len(document.Prefixes) == 0 {
			return nil, 0, errors.New("AWS ip-ranges document is malformed")
		}
		for _, entry := range document.Prefixes {
			if wanted(entry.Service) {
				if err := collector.add(entry.Prefix); err != nil {
					return nil, 0, err
				}
			}
		}
		for _, entry := range document.Ipv6Prefixes {
			if wanted(entry.Service) {
				if err := collector.add(entry.Prefix); err != nil {
					return nil, 0, err
				}
			}
		}
	case hostingFormatGcpCloud:
		var document struct {
			CreationTime string `json:"creationTime"`
			Prefixes     []struct {
				Ipv4 string `json:"ipv4Prefix"`
				Ipv6 string `json:"ipv6Prefix"`
			} `json:"prefixes"`
		}
		if err := json.NewDecoder(reader).Decode(&document); err != nil || document.CreationTime == "" || len(document.Prefixes) == 0 {
			return nil, 0, errors.New("Google Cloud ranges document is malformed")
		}
		for _, entry := range document.Prefixes {
			text := entry.Ipv4
			if text == "" {
				text = entry.Ipv6
			}
			if err := collector.add(text); err != nil {
				return nil, 0, err
			}
		}
	case hostingFormatAzureTags:
		content, err := io.ReadAll(reader)
		if err != nil {
			return nil, 0, err
		}
		var document struct {
			ChangeNumber int `json:"changeNumber"`
			Values       []struct {
				Name       string `json:"name"`
				Properties struct {
					AddressPrefixes []string `json:"addressPrefixes"`
				} `json:"properties"`
			} `json:"values"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(string(content), "\ufeff")), &document); err != nil || document.ChangeNumber == 0 || len(document.Values) == 0 {
			return nil, 0, errors.New("Azure service tags document is malformed")
		}
		matched := 0
		for _, value := range document.Values {
			if !wanted(value.Name) {
				continue
			}
			matched++
			for _, text := range value.Properties.AddressPrefixes {
				if err := collector.add(text); err != nil {
					return nil, 0, err
				}
			}
		}
		if matched != len(source.Services) {
			return nil, 0, errors.New("Azure service tags document lacks a selected service tag")
		}
	case hostingFormatOracleRanges:
		var document struct {
			LastUpdated string `json:"last_updated_timestamp"`
			Regions     []struct {
				Cidrs []struct {
					Cidr string `json:"cidr"`
				} `json:"cidrs"`
				Ipv6Cidrs []struct {
					Cidr string `json:"cidr"`
				} `json:"ipv6_cidrs"`
			} `json:"regions"`
		}
		if err := json.NewDecoder(reader).Decode(&document); err != nil || document.LastUpdated == "" || len(document.Regions) == 0 {
			return nil, 0, errors.New("Oracle public IP ranges document is malformed")
		}
		for _, region := range document.Regions {
			for _, entry := range append(region.Cidrs, region.Ipv6Cidrs...) {
				if err := collector.add(entry.Cidr); err != nil {
					return nil, 0, err
				}
			}
		}
	case hostingFormatGeofeed:
		scanner := bufio.NewScanner(reader)
		scanner.Buffer(make([]byte, 4096), 4096)
		for scanner.Scan() {
			if err := ctx.Err(); err != nil {
				return nil, 0, err
			}
			line := strings.TrimSpace(scanner.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			entry, _, _ := strings.Cut(line, ",")
			if err := collector.add(entry); err != nil {
				return nil, 0, err
			}
		}
		if err := scanner.Err(); err != nil {
			return nil, 0, errors.New("hosting geofeed is truncated or unreadable")
		}
	default:
		return nil, 0, errors.New("unsupported hosting prefix format")
	}
	if len(collector.prefixes) == 0 {
		return nil, 0, errors.New("hosting prefix source selects no prefixes")
	}
	return collector.prefixes, collector.skipped, nil
}

// Hosting evidence excludes an inferred or unknown subscriber decision. A
// direct reviewed subscriber approval meets contrary published use, which is
// a conflict, so it becomes ambiguous rather than silently losing either side.
func hostingPrefixRecord(existing mmdbtype.Map, source hostingPrefixSource) mmdbtype.Map {
	data := make(mmdbtype.Map, len(existing)+3)
	for key, value := range existing {
		data[key] = value
	}
	ids, _ := data["hosting_prefix_source_ids"].(mmdbtype.Slice)
	data["hosting_prefix_source_ids"] = append(slices.Clone(ids), mmdbtype.String(source.Id))
	state, _ := data["quality_state"].(mmdbtype.String)
	switch {
	case state == "unknown" || state == "subscriber" && data["subscriber_evidence_kind"] == mmdbtype.String("isp_inferred"):
		data["quality_state"], data["non_quality"] = mmdbtype.String("excluded"), mmdbtype.Bool(true)
		data["reason"] = mmdbtype.String("operator-published hosting prefix excludes subscriber use")
		delete(data, "subscriber_evidence_kind")
	case state == "subscriber":
		data["quality_state"], data["non_quality"] = mmdbtype.String("ambiguous"), mmdbtype.Bool(true)
		data["non_quality_ambiguous"] = mmdbtype.Bool(true)
		data["reason"] = mmdbtype.String("reviewed subscriber approval conflicts with an operator-published hosting prefix")
	}
	return data
}

func applyHostingPrefixSources(ctx context.Context, writer *mmdbwriter.Tree, catalog subscriberOriginCatalog, catalogPath string) (int, int, error) {
	if len(catalog.HostingPrefixSources) == 0 {
		return 0, 0, nil
	}
	root, err := os.OpenRoot(filepath.Dir(catalogPath))
	if err != nil {
		return 0, 0, err
	}
	defer root.Close()
	applied, skipped := 0, 0
	for _, source := range catalog.HostingPrefixSources {
		f, err := root.Open(source.File)
		if err != nil {
			return 0, 0, err
		}
		prefixes, n, parseErr := readHostingPrefixes(ctx, source, f)
		closeErr := f.Close()
		if parseErr != nil {
			return 0, 0, parseErr
		}
		if closeErr != nil {
			return 0, 0, closeErr
		}
		skipped += n
		for _, prefix := range prefixes {
			if err := ctx.Err(); err != nil {
				return 0, 0, err
			}
			_, network, _ := net.ParseCIDR(prefix.String())
			if err := writer.InsertFunc(network, func(existing mmdbtype.DataType) (mmdbtype.DataType, error) {
				base, ok := existing.(mmdbtype.Map)
				if !ok {
					return nil, errors.New("hosting prefix requires an existing policy-two classification record")
				}
				return hostingPrefixRecord(base, source), nil
			}); err != nil {
				return 0, 0, err
			}
			applied++
		}
	}
	return applied, skipped, nil
}
