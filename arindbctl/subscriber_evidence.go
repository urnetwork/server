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

// Azure publishes its weekly service-tag file under a dated name linked from
// a stable page; only a download.microsoft.com JSON link is accepted.
var azureServiceTagsLink = regexp.MustCompile(`https://download\.microsoft\.com/download/[0-9a-f/-]+/ServiceTags_Public_[0-9]{8}\.json`)

// bgp.tools tags read by the label verdicts: eyeball and contrary meanings.
var subscriberEvidenceLabelTags = []string{"dsl", "mobile", "satnet", "vpsh", "vpn", "cdn", "anycast"}

type subscriberEvidenceDownload struct {
	id       string
	url      string
	file     string
	compress bool
	validate func(context.Context, io.Reader, time.Time) (time.Time, error)
	label    *labelSource
	hosting  *hostingPrefixSource
	resolve  func(context.Context, http.Client) (string, error)
}

// The stanzas written for a refreshed catalog. Field order matches the
// documented catalog shape so the output reads like a hand-written file.
type subscriberEvidenceCatalog struct {
	Version              int                     `yaml:"version"`
	Policy               string                  `yaml:"policy"`
	MinimumOriginPeers   *uint32                 `yaml:"minimum_origin_peers,omitempty"`
	OriginCountryPolicy  string                  `yaml:"origin_country_policy,omitempty"`
	OriginSources        []countryEvidenceSource `yaml:"origin_sources"`
	RpkiSources          []rpkiSource            `yaml:"rpki_sources,omitempty"`
	AddressRiskSources   []addressRiskSource     `yaml:"address_risk_sources,omitempty"`
	RegistrySources      []registrySource        `yaml:"registry_sources,omitempty"`
	LabelSources         []labelSource           `yaml:"label_sources,omitempty"`
	HostingPrefixSources []hostingPrefixSource   `yaml:"hosting_prefix_sources,omitempty"`
	Operators            []subscriberOperator    `yaml:"operators,omitempty"`
}

func subscriberEvidenceDownloads(relayGeofeeds bool, labelSources bool, hostingPrefixes bool) []subscriberEvidenceDownload {
	origin := func(ctx context.Context, reader io.Reader, at time.Time) (time.Time, error) {
		_, generated, err := readSubscriberOrigins(ctx, reader, at, map[netip.Prefix]subscriberOriginRoute{})
		return generated, err
	}
	addressList := func(format string) func(context.Context, io.Reader, time.Time) (time.Time, error) {
		return func(ctx context.Context, reader io.Reader, at time.Time) (time.Time, error) {
			_, err := readAddressRiskList(ctx, format, reader)
			return at, err
		}
	}
	downloads := []subscriberEvidenceDownload{
		{id: "ris-ipv4", url: subscriberEvidenceRisIPv4Url, file: "riswhoisdump.IPv4.gz", validate: origin},
		{id: "ris-ipv6", url: subscriberEvidenceRisIPv6Url, file: "riswhoisdump.IPv6.gz", validate: origin},
		{id: "rpki-client", url: subscriberEvidenceRpkiUrl, file: "rpki.json.gz", compress: true, validate: func(ctx context.Context, reader io.Reader, at time.Time) (time.Time, error) {
			bounded, err := openBoundedEvidenceReader(reader, maxRpkiDecompressedBytes)
			if err != nil {
				return time.Time{}, err
			}
			return at, readRpkiClientJson(ctx, bounded, &rpkiAuthorizations{})
		}},
		{id: "tor-exit-addresses", url: subscriberEvidenceTorUrl, file: "exit-addresses", validate: addressList("tor-exit-addresses")},
		{id: "nro-delegated-stats", url: subscriberEvidenceRegistryUrl, file: "nro-delegated-stats.gz", compress: true, validate: func(ctx context.Context, reader io.Reader, at time.Time) (time.Time, error) {
			return at, readNroDelegatedStats(ctx, reader, &registryHolders{})
		}},
	}
	if relayGeofeeds {
		downloads = append(downloads,
			subscriberEvidenceDownload{id: "apple-private-relay", url: subscriberEvidenceAppleRelayUrl, file: "apple-egress-ip-ranges.csv.gz", compress: true, validate: addressList("rfc8805-geofeed")},
			subscriberEvidenceDownload{id: "cloudflare-warp", url: subscriberEvidenceWarpUrl, file: "cloudflare-local-ip-ranges.csv.gz", compress: true, validate: addressList("rfc8805-geofeed")},
		)
	}
	if labelSources {
		labelDownload := func(id, url, file, format, tag string) subscriberEvidenceDownload {
			return subscriberEvidenceDownload{id: id, url: url, file: file, compress: true, label: &labelSource{Format: format, Tag: tag}, validate: func(ctx context.Context, reader io.Reader, at time.Time) (time.Time, error) {
				labels := &asnLabels{names: map[uint32]string{}, class: map[uint32]string{}, tags: map[uint32][]string{}, users: map[uint32]uint64{}, usersByCountry: map[uint32]map[string]uint64{}, hasTags: map[string]bool{}}
				switch format {
				case labelFormatBgpToolsAsns:
					return at, readBgpToolsAsns(ctx, reader, labels)
				case labelFormatBgpToolsTag:
					return at, readBgpToolsTag(ctx, reader, tag, labels)
				default:
					return at, readApnicAspop(ctx, reader, labels)
				}
			}}
		}
		downloads = append(downloads, labelDownload("bgp-tools-asns", subscriberEvidenceBgpToolsAsns, "bgp-tools-asns.csv.gz", labelFormatBgpToolsAsns, ""))
		for _, tag := range subscriberEvidenceLabelTags {
			downloads = append(downloads, labelDownload("bgp-tools-tag-"+tag, fmt.Sprintf(subscriberEvidenceBgpToolsTag, tag), "bgp-tools-tag-"+tag+".csv.gz", labelFormatBgpToolsTag, tag))
		}
		downloads = append(downloads, labelDownload("apnic-aspop", subscriberEvidenceApnicAspop, "apnic-aspop.csv.gz", labelFormatApnicAspop, ""))
	}
	if hostingPrefixes {
		hostingDownload := func(id, url, file, format string, services []string, reason string) subscriberEvidenceDownload {
			source := hostingPrefixSource{Format: format, Services: services, Reason: reason}
			return subscriberEvidenceDownload{id: id, url: url, file: file, compress: true, hosting: &source, validate: func(ctx context.Context, reader io.Reader, at time.Time) (time.Time, error) {
				_, _, err := readHostingPrefixes(ctx, source, reader)
				return at, err
			}}
		}
		azure := hostingDownload("azure-cloud", subscriberEvidenceAzurePage, "azure-service-tags.json.gz", hostingFormatAzureTags, []string{"AzureCloud"}, "Microsoft-published AzureCloud service tag: Azure datacenter address space")
		azure.resolve = resolveAzureServiceTags
		downloads = append(downloads,
			hostingDownload("aws-ec2", subscriberEvidenceAwsRanges, "aws-ip-ranges.json.gz", hostingFormatAwsIpRanges, []string{"EC2"}, "AWS-published EC2 ranges, including Wavelength zones inside carrier networks"),
			hostingDownload("google-cloud", subscriberEvidenceGcpRanges, "google-cloud.json.gz", hostingFormatGcpCloud, nil, "Google-published Google Cloud customer ranges"),
			azure,
			hostingDownload("oracle-cloud", subscriberEvidenceOracleRanges, "oracle-public-ip-ranges.json.gz", hostingFormatOracleRanges, nil, "Oracle-published OCI public ranges"),
			hostingDownload("digitalocean", subscriberEvidenceDoGeofeed, "digitalocean-geofeed.csv.gz", hostingFormatGeofeed, nil, "DigitalOcean-published geofeed of its droplet ranges"),
			hostingDownload("linode", subscriberEvidenceLinodeGeofeed, "linode-geofeed.csv.gz", hostingFormatGeofeed, nil, "Akamai/Linode-published geofeed of its compute ranges"),
			hostingDownload("vultr", subscriberEvidenceVultrGeofeed, "vultr-geofeed.csv.gz", hostingFormatGeofeed, nil, "Vultr-published geofeed of its compute ranges"),
		)
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
		return "", time.Time{}, err
	}
	digest := sha256.Sum256(stored)
	return hex.EncodeToString(digest[:]), observed, nil
}

func refreshSubscriberEvidence(ctx context.Context, existingCatalog string, relayGeofeeds bool, labelSources bool, hostingPrefixes bool, output string, supplied *http.Client, at time.Time) error {
	catalog := subscriberEvidenceCatalog{Version: 1, Policy: "identified-subscriber-default"}
	if existingCatalog != "" {
		existing, err := loadSubscriberOriginCatalog(existingCatalog)
		if err != nil {
			return err
		}
		catalog.MinimumOriginPeers, catalog.OriginCountryPolicy, catalog.Operators = existing.MinimumOriginPeers, existing.OriginCountryPolicy, existing.Operators
	}
	client := http.Client{Timeout: 30 * time.Minute}
	if supplied != nil {
		client = *supplied
	}
	directory := filepath.Join(output, subscriberEvidenceDirectory)
	if err := os.Mkdir(directory, 0o755); err != nil {
		return err
	}
	manifest := map[string]any{"source": "pinned public subscriber evidence", "fetched_at": at, "snapshots": map[string]any{}}
	for _, download := range subscriberEvidenceDownloads(relayGeofeeds, labelSources, hostingPrefixes) {
		if err := ctx.Err(); err != nil {
			return err
		}
		if download.resolve != nil {
			resolved, err := download.resolve(ctx, client)
			if err != nil {
				return err
			}
			download.url = resolved
		}
		digest, observed, err := fetchSubscriberEvidence(ctx, client, download, directory, at)
		if err != nil {
			return err
		}
		source := countryEvidenceSource{Id: download.id, Url: download.url, File: filepath.ToSlash(filepath.Join(subscriberEvidenceDirectory, download.file)), Sha256: digest, ObservedAt: observed.UTC(), ExpiresAt: observed.UTC().Add(subscriberEvidenceFreshness)}
		manifest["snapshots"].(map[string]any)[download.id] = map[string]any{"url": download.url, "file": source.File, "sha256": digest, "observed_at": source.ObservedAt}
		if download.hosting != nil {
			hosting := *download.hosting
			hosting.countryEvidenceSource = source
			catalog.HostingPrefixSources = append(catalog.HostingPrefixSources, hosting)
			continue
		}
		if download.label != nil {
			label := *download.label
			label.countryEvidenceSource = source
			catalog.LabelSources = append(catalog.LabelSources, label)
			continue
		}
		switch download.id {
		case "ris-ipv4", "ris-ipv6":
			catalog.OriginSources = append(catalog.OriginSources, source)
		case "rpki-client":
			catalog.RpkiSources = append(catalog.RpkiSources, rpkiSource{countryEvidenceSource: source, Format: rpkiFormatClientJson})
		case "tor-exit-addresses":
			catalog.AddressRiskSources = append(catalog.AddressRiskSources, addressRiskSource{countryEvidenceSource: source, Format: "tor-exit-addresses", Category: "tor", Reason: "measured Tor exit egress addresses from the official TorDNSEL export"})
		case "nro-delegated-stats":
			catalog.RegistrySources = append(catalog.RegistrySources, registrySource{countryEvidenceSource: source, Format: registryFormatNroDelegatedStats})
		case "apple-private-relay":
			catalog.AddressRiskSources = append(catalog.AddressRiskSources, addressRiskSource{countryEvidenceSource: source, Format: "rfc8805-geofeed", Category: "vpn", Reason: "Apple-published iCloud Private Relay egress ranges"})
		case "cloudflare-warp":
			catalog.AddressRiskSources = append(catalog.AddressRiskSources, addressRiskSource{countryEvidenceSource: source, Format: "rfc8805-geofeed", Category: "vpn", Reason: "Cloudflare-published egress ranges used by WARP and Zero Trust clients"})
		}
	}
	name := "evidence.yml"
	if len(catalog.Operators) != 0 {
		name = "catalog.yml"
	}
	content, err := yaml.Marshal(catalog)
	if err != nil {
		return err
	}
	header := "# Pinned by arindbctl refresh-subscriber-evidence at " + at.UTC().Format(time.RFC3339) + ".\n# Snapshots are routing, authorization, registry and address-level evidence, not reviewed subscriber identities.\n"
	if err := writeSyncedFile(filepath.Join(output, name), append([]byte(header), content...)); err != nil {
		return err
	}
	// The written file must validate and hash exactly as a build would read it.
	if len(catalog.Operators) != 0 {
		written, err := loadSubscriberOriginCatalog(filepath.Join(output, name))
		if err != nil {
			return fmt.Errorf("refreshed catalog does not validate: %w", err)
		}
		if _, err := written.hashEvidenceSources(ctx, filepath.Join(output, name), at); err != nil {
			return fmt.Errorf("refreshed catalog evidence does not hash: %w", err)
		}
	} else {
		fragment := subscriberOriginCatalog{OriginSources: catalog.OriginSources, RpkiSources: catalog.RpkiSources, AddressRiskSources: catalog.AddressRiskSources, RegistrySources: catalog.RegistrySources, LabelSources: catalog.LabelSources, HostingPrefixSources: catalog.HostingPrefixSources}
		if _, err := fragment.hashEvidenceSources(ctx, filepath.Join(output, name), at); err != nil {
			return fmt.Errorf("refreshed evidence does not hash: %w", err)
		}
	}
	return writeManifest(output, manifest, name)
}
