package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Public evidence snapshots are pinned by this command so a catalog revision
// never hand-copies a hash or a date. Each download is HTTPS without
// redirects, bounded, parsed by the same reader the build uses, and hashed as
// stored. The output is a complete catalog when reviewed operators are
// supplied, otherwise a YAML fragment of source stanzas; neither is reviewed
// subscriber evidence, and the relay geofeeds are opt-in because their
// category is a reviewer decision.
const (
	subscriberEvidenceFreshness     = 48 * time.Hour
	maximumSubscriberEvidenceBytes  = 512 << 20
	subscriberEvidenceDirectory     = "sources"
	subscriberEvidenceRisIPv4Url    = "https://www.ris.ripe.net/dumps/riswhoisdump.IPv4.gz"
	subscriberEvidenceRisIPv6Url    = "https://www.ris.ripe.net/dumps/riswhoisdump.IPv6.gz"
	subscriberEvidenceRpkiUrl       = "https://rpki.cloudflare.com/rpki.json"
	subscriberEvidenceTorUrl        = "https://check.torproject.org/exit-addresses"
	subscriberEvidenceRegistryUrl   = "https://ftp.ripe.net/pub/stats/ripencc/nro-stats/latest/nro-delegated-stats"
	subscriberEvidenceAppleRelayUrl = "https://mask-api.icloud.com/egress-ip-ranges.csv"
	subscriberEvidenceWarpUrl       = "https://api.cloudflare.com/local-ip-ranges.csv"
	subscriberEvidenceBgpToolsAsns  = "https://bgp.tools/asns.csv"
	subscriberEvidenceBgpToolsTag   = "https://bgp.tools/tags/%s.csv"
	subscriberEvidenceApnicAspop    = "https://stats.labs.apnic.net/cgi-bin/aspop?aa=0&ww=60&rr=1&ff=2&xx=t"
	subscriberEvidenceAwsRanges     = "https://ip-ranges.amazonaws.com/ip-ranges.json"
	subscriberEvidenceGcpRanges     = "https://www.gstatic.com/ipranges/cloud.json"
	subscriberEvidenceAzurePage     = "https://www.microsoft.com/en-us/download/details.aspx?id=56519"
	subscriberEvidenceOracleRanges  = "https://docs.oracle.com/en-us/iaas/tools/public_ip_ranges.json"
	subscriberEvidenceDoGeofeed     = "https://www.digitalocean.com/geo/google.csv"
	subscriberEvidenceLinodeGeofeed = "https://geoip.linode.com/"
	subscriberEvidenceVultrGeofeed  = "https://geofeed.constant.com/"
)

// A local write failure is never a skippable upstream problem.
var errEvidenceWrite = errors.New("evidence snapshot could not be written")

// Azure publishes its weekly service-tag file under a dated name linked from
// a stable page; only a download.microsoft.com JSON link is accepted.
var azureServiceTagsLink = regexp.MustCompile(`https://download\.microsoft\.com/download/[0-9a-f/-]+/ServiceTags_Public_[0-9]{8}\.json`)

// bgp.tools tags read by the label verdicts: eyeball and contrary meanings.
var subscriberEvidenceLabelTags = []string{"dsl", "mobile", "satnet", "biznet", "vpsh", "vpn", "cdn"}

// Linnaeus is pinned to the commit that published release 202506, so its
// predictions, hand labels and splits always agree with each other.
const subscriberEvidenceLinnaeus = "https://raw.githubusercontent.com/NU-AquaLab/linnaeus/faad33a325502ac80d0392a2c19e682a45db25d1/data/released/202506/"

const (
	subscriberEvidenceCaidaAs2org = "https://publicdata.caida.org/datasets/as-organizations/latest.as-org2info.jsonl.gz"
	subscriberEvidenceAsdb        = "https://asdb.stanford.edu/static-website/data/2026-03_categorized_ases.csv"
	subscriberEvidenceIpverse     = "https://raw.githubusercontent.com/ipverse/as-metadata/master/as.json"
	subscriberEvidenceMullvad     = "https://api.mullvad.net/www/relays/all/"
	subscriberEvidenceNordvpn     = "https://api.nordvpn.com/v1/servers?limit=100000"
	subscriberEvidencePia         = "https://serverlist.piaservers.net/vpninfo/servers/v6"
	subscriberEvidenceWindscribe  = "https://assets.windscribe.com/serverlist/mob-v2/1/0"
	subscriberEvidenceAtlasProbes = "https://ftp.ripe.net/ripe/atlas/probes/archive/meta-latest"
)

// RIR whois dumps carrying named inetnum and inet6num objects. LACNIC's dump
// has no names, and ARIN's bulk data is the registration builder's input.
var subscriberEvidenceRegistryAssignments = []struct{ id, url, file string }{
	{"ripe-inetnum", "https://ftp.ripe.net/ripe/dbase/split/ripe.db.inetnum.gz", "ripe.db.inetnum.gz"},
	{"ripe-inet6num", "https://ftp.ripe.net/ripe/dbase/split/ripe.db.inet6num.gz", "ripe.db.inet6num.gz"},
	{"apnic-inetnum", "https://ftp.apnic.net/apnic/whois/apnic.db.inetnum.gz", "apnic.db.inetnum.gz"},
	{"apnic-inet6num", "https://ftp.apnic.net/apnic/whois/apnic.db.inet6num.gz", "apnic.db.inet6num.gz"},
	{"afrinic-db", "https://ftp.afrinic.net/pub/dbase/afrinic.db.gz", "afrinic.db.gz"},
}

// Optional evidence families; the routing, RPKI, Tor and registry-holder
// snapshots are always pinned.
type subscriberEvidenceOptions struct {
	RelayGeofeeds       bool
	LabelSources        bool
	HostingPrefixes     bool
	RegistryAssignments bool
	VpnServers          bool
}

type subscriberEvidenceDownload struct {
	id       string
	url      string
	file     string
	compress bool
	validate func(context.Context, io.Reader, time.Time) (time.Time, error)
	place    func(*subscriberEvidenceCatalog, countryEvidenceSource)
	resolve  func(context.Context, http.Client) (string, error)
}

// The stanzas written for a refreshed catalog. Field order matches the
// documented catalog shape so the output reads like a hand-written file.
type subscriberEvidenceCatalog struct {
	Version                   int                        `yaml:"version"`
	Policy                    string                     `yaml:"policy"`
	MinimumOriginPeers        *uint32                    `yaml:"minimum_origin_peers,omitempty"`
	OriginCountryPolicy       string                     `yaml:"origin_country_policy,omitempty"`
	OriginSources             []countryEvidenceSource    `yaml:"origin_sources"`
	RpkiSources               []rpkiSource               `yaml:"rpki_sources,omitempty"`
	HostingPrefixSources      []hostingPrefixSource      `yaml:"hosting_prefix_sources,omitempty"`
	RegistryAssignmentSources []registryAssignmentSource `yaml:"registry_assignment_sources,omitempty"`
	AddressRiskSources        []addressRiskSource        `yaml:"address_risk_sources,omitempty"`
	RegistrySources           []registrySource           `yaml:"registry_sources,omitempty"`
	LabelSources              []labelSource              `yaml:"label_sources,omitempty"`
	Operators                 []subscriberOperator       `yaml:"operators,omitempty"`
}

func (self subscriberEvidenceCatalog) originCatalog() subscriberOriginCatalog {
	return subscriberOriginCatalog{OriginSources: self.OriginSources, RpkiSources: self.RpkiSources, AddressRiskSources: self.AddressRiskSources,
		RegistrySources: self.RegistrySources, LabelSources: self.LabelSources, HostingPrefixSources: self.HostingPrefixSources,
		RegistryAssignmentSources: self.RegistryAssignmentSources}
}

func subscriberEvidenceDownloads(options subscriberEvidenceOptions) []subscriberEvidenceDownload {
	origin := func(ctx context.Context, reader io.Reader, at time.Time) (time.Time, error) {
		_, generated, err := readSubscriberOrigins(ctx, reader, at, map[netip.Prefix]subscriberOriginRoute{})
		return generated, err
	}
	addressRisk := func(id, url, file string, compress bool, format, category, reason string) subscriberEvidenceDownload {
		return subscriberEvidenceDownload{id: id, url: url, file: file, compress: compress,
			validate: func(ctx context.Context, reader io.Reader, at time.Time) (time.Time, error) {
				_, err := readAddressRiskList(ctx, format, reader)
				return at, err
			},
			place: func(catalog *subscriberEvidenceCatalog, source countryEvidenceSource) {
				catalog.AddressRiskSources = append(catalog.AddressRiskSources, addressRiskSource{countryEvidenceSource: source, Format: format, Category: category, Reason: reason})
			}}
	}
	registry := func(id, url, file, format string, read func(context.Context, io.Reader, *registryHolders) error) subscriberEvidenceDownload {
		return subscriberEvidenceDownload{id: id, url: url, file: file, compress: !strings.HasSuffix(url, ".gz"),
			validate: func(ctx context.Context, reader io.Reader, at time.Time) (time.Time, error) {
				return at, read(ctx, reader, &registryHolders{})
			},
			place: func(catalog *subscriberEvidenceCatalog, source countryEvidenceSource) {
				catalog.RegistrySources = append(catalog.RegistrySources, registrySource{countryEvidenceSource: source, Format: format})
			}}
	}
	downloads := []subscriberEvidenceDownload{}
	for _, d := range []struct{ id, url, file string }{{"ris-ipv4", subscriberEvidenceRisIPv4Url, "riswhoisdump.IPv4.gz"}, {"ris-ipv6", subscriberEvidenceRisIPv6Url, "riswhoisdump.IPv6.gz"}} {
		downloads = append(downloads, subscriberEvidenceDownload{id: d.id, url: d.url, file: d.file, validate: origin,
			place: func(catalog *subscriberEvidenceCatalog, source countryEvidenceSource) {
				catalog.OriginSources = append(catalog.OriginSources, source)
			}})
	}
	downloads = append(downloads,
		subscriberEvidenceDownload{id: "rpki-client", url: subscriberEvidenceRpkiUrl, file: "rpki.json.gz", compress: true,
			validate: func(ctx context.Context, reader io.Reader, at time.Time) (time.Time, error) {
				bounded, err := openBoundedEvidenceReader(reader, maxRpkiDecompressedBytes)
				if err != nil {
					return time.Time{}, err
				}
				return at, readRpkiClientJson(ctx, bounded, &rpkiAuthorizations{})
			},
			place: func(catalog *subscriberEvidenceCatalog, source countryEvidenceSource) {
				catalog.RpkiSources = append(catalog.RpkiSources, rpkiSource{countryEvidenceSource: source, Format: rpkiFormatClientJson})
			}},
		addressRisk("tor-exit-addresses", subscriberEvidenceTorUrl, "exit-addresses", false, "tor-exit-addresses", "tor", "measured Tor exit egress addresses from the official TorDNSEL export"),
		registry("nro-delegated-stats", subscriberEvidenceRegistryUrl, "nro-delegated-stats.gz", registryFormatNroDelegatedStats, readNroDelegatedStats),
		registry("caida-as2org", subscriberEvidenceCaidaAs2org, "caida-as2org.jsonl.gz", registryFormatCaidaAs2org, readCaidaAs2org),
	)
	if options.RelayGeofeeds {
		downloads = append(downloads,
			addressRisk("apple-private-relay", subscriberEvidenceAppleRelayUrl, "apple-egress-ip-ranges.csv.gz", true, "rfc8805-geofeed", "vpn", "Apple-published iCloud Private Relay egress ranges"),
			addressRisk("cloudflare-warp", subscriberEvidenceWarpUrl, "cloudflare-local-ip-ranges.csv.gz", true, "rfc8805-geofeed", "vpn", "Cloudflare-published egress ranges used by WARP and Zero Trust clients"),
		)
	}
	if options.VpnServers {
		downloads = append(downloads,
			addressRisk("mullvad-relays", subscriberEvidenceMullvad, "mullvad-relays.json.gz", true, "mullvad-relays-json", "vpn", "Mullvad-published relay list"),
			addressRisk("nordvpn-servers", subscriberEvidenceNordvpn, "nordvpn-servers.json.gz", true, "nordvpn-servers-json", "vpn", "NordVPN-published server list"),
			addressRisk("pia-servers", subscriberEvidencePia, "pia-servers.json.gz", true, "pia-servers-json", "vpn", "Private Internet Access-published server list"),
			addressRisk("windscribe-servers", subscriberEvidenceWindscribe, "windscribe-servers.json.gz", true, "windscribe-serverlist-json", "vpn", "Windscribe-published server list"),
		)
	}
	if options.LabelSources {
		label := func(id, url, file, format, tag string) subscriberEvidenceDownload {
			source := labelSource{Format: format, Tag: tag}
			return subscriberEvidenceDownload{id: id, url: url, file: file, compress: true,
				validate: func(ctx context.Context, reader io.Reader, at time.Time) (time.Time, error) {
					return at, readLabelSource(ctx, source, reader, newAsnLabels())
				},
				place: func(catalog *subscriberEvidenceCatalog, pinned countryEvidenceSource) {
					value := source
					value.countryEvidenceSource = pinned
					catalog.LabelSources = append(catalog.LabelSources, value)
				}}
		}
		downloads = append(downloads, label("bgp-tools-asns", subscriberEvidenceBgpToolsAsns, "bgp-tools-asns.csv.gz", labelFormatBgpToolsAsns, ""))
		for _, tag := range subscriberEvidenceLabelTags {
			downloads = append(downloads, label("bgp-tools-tag-"+tag, fmt.Sprintf(subscriberEvidenceBgpToolsTag, tag), "bgp-tools-tag-"+tag+".csv.gz", labelFormatBgpToolsTag, tag))
		}
		downloads = append(downloads,
			label("apnic-aspop", subscriberEvidenceApnicAspop, "apnic-aspop.csv.gz", labelFormatApnicAspop, ""),
			label("asdb", subscriberEvidenceAsdb, "asdb-categorized-ases.csv.gz", labelFormatAsdb, ""),
			label("ipverse", subscriberEvidenceIpverse, "ipverse-as-metadata.json.gz", labelFormatIpverse, ""),
			label("linnaeus-predictions", subscriberEvidenceLinnaeus+"predictions/sublevel/complete.csv", "linnaeus-sublevel-predictions.csv.gz", labelFormatLinnaeusPred, ""),
			label("linnaeus-labels", subscriberEvidenceLinnaeus+"labels/sublevel.csv", "linnaeus-sublevel-labels.csv.gz", labelFormatLinnaeusLabels, ""),
			label("linnaeus-splits", subscriberEvidenceLinnaeus+"splits/assignments.csv", "linnaeus-splits.csv.gz", labelFormatLinnaeusSplits, ""),
		)
	}
	if options.HostingPrefixes {
		hosting := func(id, url, file, format string, services []string, reason string) subscriberEvidenceDownload {
			source := hostingPrefixSource{Format: format, Services: services, Reason: reason}
			return subscriberEvidenceDownload{id: id, url: url, file: file, compress: true,
				validate: func(ctx context.Context, reader io.Reader, at time.Time) (time.Time, error) {
					_, _, err := readHostingPrefixes(ctx, source, reader)
					return at, err
				},
				place: func(catalog *subscriberEvidenceCatalog, pinned countryEvidenceSource) {
					value := source
					value.countryEvidenceSource = pinned
					catalog.HostingPrefixSources = append(catalog.HostingPrefixSources, value)
				}}
		}
		azure := hosting("azure-cloud", subscriberEvidenceAzurePage, "azure-service-tags.json.gz", hostingFormatAzureTags, []string{"AzureCloud"}, "Microsoft-published AzureCloud service tag: Azure datacenter address space")
		azure.resolve = resolveAzureServiceTags
		downloads = append(downloads,
			hosting("aws-ec2", subscriberEvidenceAwsRanges, "aws-ip-ranges.json.gz", hostingFormatAwsIpRanges, []string{"EC2"}, "AWS-published EC2 ranges, including Wavelength zones inside carrier networks"),
			hosting("google-cloud", subscriberEvidenceGcpRanges, "google-cloud.json.gz", hostingFormatGcpCloud, nil, "Google-published Google Cloud customer ranges"),
			azure,
			hosting("oracle-cloud", subscriberEvidenceOracleRanges, "oracle-public-ip-ranges.json.gz", hostingFormatOracleRanges, nil, "Oracle-published OCI public ranges"),
			hosting("digitalocean", subscriberEvidenceDoGeofeed, "digitalocean-geofeed.csv.gz", hostingFormatGeofeed, nil, "DigitalOcean-published geofeed of its droplet ranges"),
			hosting("linode", subscriberEvidenceLinodeGeofeed, "linode-geofeed.csv.gz", hostingFormatGeofeed, nil, "Akamai/Linode-published geofeed of its compute ranges"),
			hosting("vultr", subscriberEvidenceVultrGeofeed, "vultr-geofeed.csv.gz", hostingFormatGeofeed, nil, "Vultr-published geofeed of its compute ranges"),
		)
	}
	if options.RegistryAssignments {
		for _, d := range subscriberEvidenceRegistryAssignments {
			id := d.id
			downloads = append(downloads, subscriberEvidenceDownload{id: id, url: d.url, file: d.file,
				validate: func(ctx context.Context, reader io.Reader, at time.Time) (time.Time, error) {
					_, err := scanRpslAssignments(ctx, reader, id, func(registryAssignment) error { return nil })
					return at, err
				},
				place: func(catalog *subscriberEvidenceCatalog, source countryEvidenceSource) {
					catalog.RegistryAssignmentSources = append(catalog.RegistryAssignmentSources, registryAssignmentSource{countryEvidenceSource: source, Format: registryAssignmentFormatRpsl})
				}})
		}
	}
	return downloads
}

// Reads the stable download page and returns the current dated file URL.
func resolveAzureServiceTags(ctx context.Context, client http.Client) (string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, subscriberEvidenceAzurePage, nil)
	if err != nil {
		return "", err
	}
	request.Header.Set("User-Agent", "urnetwork-arindbctl/"+Version+" (subscriber evidence refresh)")
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(request)
	if err != nil {
		return "", errors.New("Azure service tags page download failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("Azure service tags page returned HTTP %d", response.StatusCode)
	}
	page, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return "", err
	}
	link := azureServiceTagsLink.Find(page)
	if link == nil {
		return "", errors.New("Azure service tags page has no download link")
	}
	return string(link), nil
}

// Downloads one snapshot into the staging directory and returns its stored
// hash and observation time. Redirects are refused and the response is bounded
// before it is parsed; a snapshot that fails its own reader is not kept.
func fetchSubscriberEvidence(ctx context.Context, client http.Client, download subscriberEvidenceDownload, directory string, at time.Time) (string, time.Time, error) {
	if !strings.HasPrefix(download.url, "https://") {
		return "", time.Time{}, errors.New("evidence downloads require HTTPS")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, download.url, nil)
	if err != nil {
		return "", time.Time{}, errors.New("cannot construct evidence download request")
	}
	request.Header.Set("User-Agent", "urnetwork-arindbctl/"+Version+" (subscriber evidence refresh)")
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return "", time.Time{}, ctx.Err()
		}
		return "", time.Time{}, fmt.Errorf("evidence download %s failed", download.id)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", time.Time{}, fmt.Errorf("evidence download %s returned HTTP %d", download.id, response.StatusCode)
	}
	var content bytes.Buffer
	n, err := io.Copy(&content, io.LimitReader(response.Body, maximumSubscriberEvidenceBytes+1))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("evidence download %s was interrupted", download.id)
	}
	if n > maximumSubscriberEvidenceBytes || n == 0 {
		return "", time.Time{}, fmt.Errorf("evidence download %s is empty or exceeds its size bound", download.id)
	}
	observed, err := download.validate(ctx, bytes.NewReader(content.Bytes()), at)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("evidence download %s failed validation: %w", download.id, err)
	}
	stored := content.Bytes()
	if download.compress {
		var compressed bytes.Buffer
		zip := gzip.NewWriter(&compressed)
		if _, err := zip.Write(stored); err != nil {
			return "", time.Time{}, err
		}
		if err := zip.Close(); err != nil {
			return "", time.Time{}, err
		}
		stored = compressed.Bytes()
	}
	if err := writeSyncedFile(filepath.Join(directory, download.file), stored); err != nil {
		return "", time.Time{}, fmt.Errorf("%w: %v", errEvidenceWrite, err)
	}
	digest := sha256.Sum256(stored)
	return hex.EncodeToString(digest[:]), observed, nil
}

func refreshSubscriberEvidence(ctx context.Context, existingCatalog string, options subscriberEvidenceOptions, output string, supplied *http.Client, at time.Time) error {
	_, err := pinSubscriberEvidence(ctx, existingCatalog, options, false, output, supplied, at)
	return err
}

// Strict mode fails on any source. Best-effort mode, used by update, leaves out
// a source that cannot be downloaded or fails validation and reports it; the
// RIS origin snapshots stay required because nothing can be inferred without
// them. A local write failure is always fatal.
func pinSubscriberEvidence(ctx context.Context, existingCatalog string, options subscriberEvidenceOptions, bestEffort bool, output string, supplied *http.Client, at time.Time) ([]updateUnavailable, error) {
	catalog := subscriberEvidenceCatalog{Version: 1, Policy: "identified-subscriber-default"}
	if existingCatalog != "" {
		existing, err := loadSubscriberOriginCatalog(existingCatalog)
		if err != nil {
			return nil, err
		}
		catalog.MinimumOriginPeers, catalog.OriginCountryPolicy, catalog.Operators = existing.MinimumOriginPeers, existing.OriginCountryPolicy, existing.Operators
	}
	client := http.Client{Timeout: 30 * time.Minute}
	if supplied != nil {
		client = *supplied
	}
	directory := filepath.Join(output, subscriberEvidenceDirectory)
	if err := os.Mkdir(directory, 0o755); err != nil {
		return nil, err
	}
	unavailable := []updateUnavailable{}
	manifest := map[string]any{"source": "pinned public subscriber evidence", "fetched_at": at, "snapshots": map[string]any{}}
	for _, download := range subscriberEvidenceDownloads(options) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		skip := func(err error) error {
			if !bestEffort || slices.Contains(subscriberEvidenceRequired, download.id) || errors.Is(err, errEvidenceWrite) || ctx.Err() != nil {
				return err
			}
			unavailable = append(unavailable, updateUnavailable{Id: download.id, Url: download.url, Error: err.Error()})
			return nil
		}
		if download.resolve != nil {
			resolved, err := download.resolve(ctx, client)
			if err != nil {
				if err := skip(err); err != nil {
					return nil, err
				}
				continue
			}
			download.url = resolved
		}
		digest, observed, err := fetchSubscriberEvidence(ctx, client, download, directory, at)
		if err != nil {
			if err := skip(err); err != nil {
				return nil, err
			}
			continue
		}
		source := countryEvidenceSource{Id: download.id, Url: download.url, File: filepath.ToSlash(filepath.Join(subscriberEvidenceDirectory, download.file)), Sha256: digest, ObservedAt: observed.UTC(), ExpiresAt: observed.UTC().Add(subscriberEvidenceFreshness)}
		manifest["snapshots"].(map[string]any)[download.id] = map[string]any{"url": download.url, "file": source.File, "sha256": digest, "observed_at": source.ObservedAt}
		download.place(&catalog, source)
	}
	name := "evidence.yml"
	if len(catalog.Operators) != 0 {
		name = "catalog.yml"
	}
	content, err := yaml.Marshal(catalog)
	if err != nil {
		return nil, err
	}
	header := "# Pinned by arindbctl refresh-subscriber-evidence at " + at.UTC().Format(time.RFC3339) + ".\n# Snapshots are routing, authorization, registry and address-level evidence, not reviewed subscriber identities.\n"
	if err := writeSyncedFile(filepath.Join(output, name), append([]byte(header), content...)); err != nil {
		return nil, err
	}
	// The written file must validate and hash exactly as a build would read it.
	if len(catalog.Operators) != 0 {
		written, err := loadSubscriberOriginCatalog(filepath.Join(output, name))
		if err != nil {
			return nil, fmt.Errorf("refreshed catalog does not validate: %w", err)
		}
		if _, err := written.hashEvidenceSources(ctx, filepath.Join(output, name), at); err != nil {
			return nil, fmt.Errorf("refreshed catalog evidence does not hash: %w", err)
		}
	} else {
		if _, err := catalog.originCatalog().hashEvidenceSources(ctx, filepath.Join(output, name), at); err != nil {
			return nil, fmt.Errorf("refreshed evidence does not hash: %w", err)
		}
	}
	manifest["unavailable"] = unavailable
	return unavailable, writeManifest(output, manifest, name)
}
