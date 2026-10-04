package main

import (
	"bufio"
	"compress/bzip2"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	mmdb "github.com/oschwald/maxminddb-golang/v2"
)

// RIPE Atlas probes are the only free evidence of subscriber use below the
// ASN: hosts tag their probe "home", "dsl", "fibre" and so on, or
// "datacentre", and the daily archive publishes each connected public probe's
// address. None of our classification inputs reads Atlas, so it is a held-out
// reference for the finished database. Tags are self-reported and the probe
// population leans towards Europe and technical users, so the result is an
// estimate with an interval, not a gate.
// https://ftp.ripe.net/ripe/atlas/probes/archive/README
var (
	atlasResidentialTags = []string{"home", "dsl", "adsl", "vdsl", "vdsl2", "cable", "docsis", "fibre", "fiber", "ftth", "gpon", "pppoe", "fios", "lte", "4g", "5g", "mobile"}
	atlasDatacentreTags  = []string{"datacentre", "datacenter", "vps", "system-anchor"}
)

const maxAtlasDecompressedBytes = 512 << 20

type atlasProbe struct {
	Id         int      `json:"id"`
	AddressV4  *string  `json:"address_v4"`
	AddressV6  *string  `json:"address_v6"`
	AsnV4      *uint32  `json:"asn_v4"`
	AsnV6      *uint32  `json:"asn_v6"`
	StatusName string   `json:"status_name"`
	IsAnchor   bool     `json:"is_anchor"`
	IsPublic   bool     `json:"is_public"`
	Tags       []string `json:"tags"`
	Country    string   `json:"country_code"`
}

// Accepts the archive's bzip2 files, gzip, or plain JSON.
func readAtlasProbes(ctx context.Context, raw io.Reader) ([]atlasProbe, error) {
	buffered := bufio.NewReader(raw)
	magic, _ := buffered.Peek(3)
	var reader io.Reader = buffered
	switch {
	case len(magic) == 3 && string(magic) == "BZh":
		reader = bzip2.NewReader(buffered)
	case len(magic) >= 2 && magic[0] == 0x1f && magic[1] == 0x8b:
		zip, err := gzip.NewReader(buffered)
		if err != nil {
			return nil, errors.New("Atlas probe archive is not valid gzip")
		}
		reader = zip
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var document struct {
		Objects []atlasProbe `json:"objects"`
	}
	if err := json.NewDecoder(io.LimitReader(reader, maxAtlasDecompressedBytes)).Decode(&document); err != nil || len(document.Objects) == 0 {
		return nil, errors.New("Atlas probe archive is malformed or empty")
	}
	return document.Objects, nil
}

// residential, datacentre or "" for probes outside the reference. Anchors are
// datacentre-hosted by RIPE's requirements, whatever their hosts' tags say.
func atlasProbeClass(probe atlasProbe) string {
	if probe.StatusName != "Connected" || !probe.IsPublic {
		return ""
	}
	if probe.IsAnchor {
		return "datacentre"
	}
	residential, datacentre := false, false
	for _, tag := range probe.Tags {
		tag = strings.ToLower(tag)
		residential = residential || slices.Contains(atlasResidentialTags, tag)
		datacentre = datacentre || slices.Contains(atlasDatacentreTags, tag)
	}
	switch {
	case residential && !datacentre:
		return "residential"
	case datacentre && !residential:
		return "datacentre"
	default:
		return ""
	}
}

// A proportion with its Wilson score 95% interval.
type wilsonEstimate struct {
	Successes int     `json:"successes"`
	Trials    int     `json:"trials"`
	Estimate  float64 `json:"estimate"`
	Lower95   float64 `json:"lower_95"`
	Upper95   float64 `json:"upper_95"`
}

func wilson(successes, trials int) wilsonEstimate {
	result := wilsonEstimate{Successes: successes, Trials: trials}
	if trials == 0 {
		return result
	}
	const z = 1.959963984540054
	n, p := float64(trials), float64(successes)/float64(trials)
	center := (p + z*z/(2*n)) / (1 + z*z/n)
	half := z * math.Sqrt(p*(1-p)/n+z*z/(4*n*n)) / (1 + z*z/n)
	round := func(v float64) float64 { return math.Round(10000*v) / 10000 }
	result.Estimate, result.Lower95, result.Upper95 = round(p), round(math.Max(0, center-half)), round(math.Min(1, center+half))
	return result
}

type atlasFalseApproval struct {
	ProbeId   int      `json:"probe_id"`
	Network   string   `json:"network"`
	ASN       uint32   `json:"asn"`
	Operators []string `json:"origin_operator_ids,omitempty"`
	Evidence  string   `json:"subscriber_evidence_kind,omitempty"`
}

// One address family's probe address, its lookup key and the /24 or /48 it
// is deduplicated by.
func atlasProbeAddresses(probe atlasProbe) [][3]string {
	result := [][3]string{}
	for _, entry := range []struct {
		address *string
		bits    int
	}{{probe.AddressV4, 24}, {probe.AddressV6, 48}} {
		if entry.address == nil || *entry.address == "" {
			continue
		}
		address, err := netip.ParseAddr(*entry.address)
		if err != nil || (address.Is4() != (entry.bits == 24)) {
			continue
		}
		network := netip.PrefixFrom(address, entry.bits).Masked()
		result = append(result, [3]string{address.String(), network.String(), fmt.Sprint(entry.bits)})
	}
	return result
}

// Looks up every reference probe in the finished database and reports the
// clean label's precision against Atlas tags, residential recall, the clean
// rate among datacentre probes, and which decision kept each residential
// probe out of the clean label. Units are distinct /24 (IPv4) or /48 (IPv6)
// networks, so one site with several probes counts once.
func validateArinDatabase(ctx context.Context, databasePath, probesPath, output string, at time.Time) error {
	database, err := mmdb.Open(databasePath)
	if err != nil {
		return err
	}
	defer database.Close()
	f, err := os.Open(probesPath)
	if err != nil {
		return err
	}
	probes, err := readAtlasProbes(ctx, f)
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	type unit struct{ class, network string }
	clean := map[unit]bool{}
	seen := map[unit]bool{}
	reasons := map[unit]string{}
	notCleanReasons := map[string]int{}
	cleanByEvidence := map[string]map[string]int{}
	falseApprovals := []atlasFalseApproval{}
	conflicting := map[string]string{}
	for _, probe := range probes {
		class := atlasProbeClass(probe)
		if class == "" {
			continue
		}
		for _, address := range atlasProbeAddresses(probe) {
			// A network tagged both ways by different probes is dropped.
			if previous, ok := conflicting[address[1]]; ok && previous != class {
				conflicting[address[1]] = "conflict"
				continue
			}
			conflicting[address[1]] = class
			key := unit{class, address[1]}
			seen[key] = true
			var record struct {
				QualityState string   `maxminddb:"quality_state"`
				NonQuality   bool     `maxminddb:"non_quality"`
				Risk         bool     `maxminddb:"risk"`
				Evidence     string   `maxminddb:"subscriber_evidence_kind"`
				OriginState  string   `maxminddb:"origin_use_state"`
				Withheld     string   `maxminddb:"origin_withheld_reason"`
				Operators    []string `maxminddb:"origin_operator_ids"`
				HostingIds   []string `maxminddb:"hosting_prefix_source_ids"`
				RiskIds      []string `maxminddb:"address_risk_source_ids"`
			}
			if err := database.Lookup(netip.MustParseAddr(address[0])).Decode(&record); err != nil {
				return err
			}
			// A unit is clean when any of its probe addresses is: generous to
			// residential recall and strict about datacentre false approvals.
			isClean := record.QualityState == "subscriber" && !record.NonQuality && !record.Risk
			previouslyClean := clean[key]
			clean[key] = previouslyClean || isClean
			if previouslyClean {
				continue
			}
			evidence := record.Evidence
			if evidence == "" {
				evidence = "direct"
			}
			if isClean {
				if cleanByEvidence[evidence] == nil {
					cleanByEvidence[evidence] = map[string]int{}
				}
				cleanByEvidence[evidence][class]++
				if class == "datacentre" {
					asn := uint32(0)
					if address[2] == "24" && probe.AsnV4 != nil {
						asn = *probe.AsnV4
					} else if address[2] == "48" && probe.AsnV6 != nil {
						asn = *probe.AsnV6
					}
					falseApprovals = append(falseApprovals, atlasFalseApproval{ProbeId: probe.Id, Network: address[1], ASN: asn, Operators: record.Operators, Evidence: record.Evidence})
				}
				continue
			}
			if class != "residential" {
				continue
			}
			reason := record.QualityState
			switch {
			case record.Risk && record.QualityState == "subscriber":
				reason = "risk"
			case len(record.RiskIds) != 0:
				reason = "address-level-risk"
			case len(record.HostingIds) != 0:
				reason = "hosting-prefix"
			case record.Withheld != "":
				reason = "withheld/" + record.Withheld
			case record.OriginState == "unknown" || record.QualityState == "unknown" && len(record.Operators) == 0:
				reason = "no-identified-operator"
			}
			reasons[key] = reason
		}
	}
	for key, reason := range reasons {
		if !clean[key] && conflicting[key.network] != "conflict" {
			notCleanReasons[reason]++
		}
	}
	// Drop networks that turned out to be conflicting.
	residentialTotal, datacentreTotal, cleanResidential, cleanDatacentre := 0, 0, 0, 0
	for key, isClean := range clean {
		if conflicting[key.network] == "conflict" {
			continue
		}
		if key.class == "residential" {
			residentialTotal++
			if isClean {
				cleanResidential++
			}
		} else {
			datacentreTotal++
			if isClean {
				cleanDatacentre++
			}
		}
	}
	kept := falseApprovals[:0]
	for _, approval := range falseApprovals {
		if conflicting[approval.Network] != "conflict" {
			kept = append(kept, approval)
		}
	}
	falseApprovals = kept
	sort.Slice(falseApprovals, func(i, j int) bool { return falseApprovals[i].Network < falseApprovals[j].Network })
	conflicts := 0
	for _, value := range conflicting {
		if value == "conflict" {
			conflicts++
		}
	}
	report := map[string]any{
		"generated_at": at, "reference": "RIPE Atlas connected public probes, user tags",
		"residential_networks": residentialTotal, "datacentre_networks": datacentreTotal, "conflicting_networks_dropped": conflicts,
		"clean_precision":            wilson(cleanResidential, cleanResidential+cleanDatacentre),
		"residential_recall":         wilson(cleanResidential, residentialTotal),
		"datacentre_clean_rate":      wilson(cleanDatacentre, datacentreTotal),
		"clean_by_evidence":          cleanByEvidence,
		"residential_not_clean":      notCleanReasons,
		"datacentre_false_approvals": falseApprovals,
		"caveats": []string{
			"Atlas tags are set by probe hosts and not validated; the population leans towards Europe and technical users.",
			"Units are distinct /24 or /48 networks; networks tagged both residential and datacentre are dropped.",
			"Residential probes outside reviewed operators are expected to be unknown; recall measures catalog coverage as much as rule cost.",
		},
	}
	content, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	if err := writeSyncedFile(filepath.Join(output, "validation.json"), append(content, '\n')); err != nil {
		return err
	}
	hashes := map[string]string{}
	for name, path := range map[string]string{"arin_mmdb": databasePath, "atlas_probes": probesPath} {
		if hashes[name], err = hashArinBuildInput(ctx, path); err != nil {
			return err
		}
	}
	if err := writeManifest(output, map[string]any{"source": "arindb validation against RIPE Atlas", "generated_at": at, "inputs_sha256": hashes}, "validation.json"); err != nil {
		return err
	}
	return nil
}

// The headline numbers of a written validation report, for update summaries.
func readValidationSummary(output string) (string, error) {
	content, err := os.ReadFile(filepath.Join(output, "validation.json"))
	if err != nil {
		return "", err
	}
	var report struct {
		Precision wilsonEstimate `json:"clean_precision"`
		Recall    wilsonEstimate `json:"residential_recall"`
		Dcentre   wilsonEstimate `json:"datacentre_clean_rate"`
	}
	if err := json.Unmarshal(content, &report); err != nil {
		return "", err
	}
	return fmt.Sprintf("Atlas clean precision %s; residential recall %s; datacentre clean rate %s", report.Precision, report.Recall, report.Dcentre), nil
}

func (self wilsonEstimate) String() string {
	if self.Trials == 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.1f%% [%.1f-%.1f] of %d", 100*self.Estimate, 100*self.Lower95, 100*self.Upper95, self.Trials)
}
