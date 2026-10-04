package main

import (
	"context"
	"encoding/csv"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// Independent per-ASN labels validate the reviewed catalog; they never
// classify an address. bgp.tools publishes a curated class per ASN and tag
// membership lists; APNIC Labs estimates users per ASN and country from ad
// impressions. Each is a different method from our identity review, so
// agreement corroborates an entry and disagreement queues it for review.
// The audit is their only reader.
// https://bgp.tools/kb/api https://stats.labs.apnic.net/aspop
type labelSource struct {
	countryEvidenceSource `yaml:",inline" json:",inline"`
	Format                string `yaml:"format" json:"format"`
	Tag                   string `yaml:"tag,omitempty" json:"tag,omitempty"`
}

const (
	labelFormatBgpToolsAsns   = "bgp-tools-asns-csv"
	labelFormatBgpToolsTag    = "bgp-tools-tag-csv"
	labelFormatApnicAspop     = "apnic-aspop-csv"
	maxLabelDecompressedBytes = 256 << 20
	maxLabelRecords           = 4 << 20
)

var labelFormats = []string{labelFormatBgpToolsAsns, labelFormatBgpToolsTag, labelFormatApnicAspop}

// Tags whose meaning contradicts a subscriber identity, and tags or classes
// that corroborate one. "tor" means an ASN hosts exits, which residential
// ISPs do too, so it is neither.
var (
	labelContraryTags   = []string{"vpsh", "vpn", "cdn", "anycast"}
	labelEyeballTags    = []string{"dsl", "mobile", "satnet"}
	labelEyeballClasses = []string{"Eyeball"}
	labelContraryClass  = []string{"Content"}
)

type asnLabels struct {
	names          map[uint32]string
	class          map[uint32]string
	tags           map[uint32][]string
	users          map[uint32]uint64
	usersByCountry map[uint32]map[string]uint64
	hasClass       bool
	hasTags        map[string]bool
	hasUsers       bool
	records        int
}

func (self *asnLabels) count() error {
	self.records++
	if self.records > maxLabelRecords {
		return errors.New("label sources exceed their record bound")
	}
	return nil
}

func parseLabelASN(text string) (uint32, bool) {
	text = strings.TrimSpace(strings.Trim(strings.TrimSpace(text), "\""))
	text = strings.TrimPrefix(strings.TrimPrefix(text, "AS"), "as")
	asn, err := strconv.ParseUint(text, 10, 32)
	return uint32(asn), err == nil && asn != 0
}

// asn,name,class,cc with an AS-prefixed ASN.
func readBgpToolsAsns(ctx context.Context, reader io.Reader, into *asnLabels) error {
	records := csv.NewReader(reader)
	records.FieldsPerRecord = -1
	header, err := records.Read()
	if err != nil || len(header) < 3 || strings.ToLower(header[0]) != "asn" || strings.ToLower(header[2]) != "class" {
		return errors.New("bgp.tools ASN list lacks its asn,name,class header")
	}
	rows := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		record, err := records.Read()
		if err == io.EOF {
			break
		}
		if err != nil || len(record) < 3 {
			return errors.New("bgp.tools ASN list row is malformed")
		}
		asn, ok := parseLabelASN(record[0])
		if !ok {
			return errors.New("bgp.tools ASN list has an invalid ASN")
		}
		into.class[asn] = strings.TrimSpace(record[2])
		if into.names[asn] == "" {
			into.names[asn] = strings.TrimSpace(record[1])
		}
		if err := into.count(); err != nil {
			return err
		}
		rows++
	}
	if rows == 0 {
		return errors.New("bgp.tools ASN list has no rows")
	}
	into.hasClass = true
	return nil
}

// ASN,Name per line without a header; an empty tag list is valid.
func readBgpToolsTag(ctx context.Context, reader io.Reader, tag string, into *asnLabels) error {
	records := csv.NewReader(reader)
	records.FieldsPerRecord = -1
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		record, err := records.Read()
		if err == io.EOF {
			break
		}
		if err != nil || len(record) < 1 {
			return errors.New("bgp.tools tag list row is malformed")
		}
		asn, ok := parseLabelASN(record[0])
		if !ok {
			return errors.New("bgp.tools tag list has an invalid ASN")
		}
		if !slices.Contains(into.tags[asn], tag) {
			into.tags[asn] = append(into.tags[asn], tag)
			slices.Sort(into.tags[asn])
		}
		if len(record) > 1 && into.names[asn] == "" {
			into.names[asn] = strings.TrimSpace(record[1])
		}
		if err := into.count(); err != nil {
			return err
		}
	}
	into.hasTags[tag] = true
	return nil
}

// #Rank,AS,"AS Name",CC,"Users (est.)",... with "#" comment lines. One ASN
// can appear under several countries; users accumulate per country.
func readApnicAspop(ctx context.Context, reader io.Reader, into *asnLabels) error {
	records := csv.NewReader(reader)
	records.FieldsPerRecord = -1
	records.Comment = '#'
	records.LazyQuotes = true
	rows := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		record, err := records.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return errors.New("APNIC user estimates row is malformed")
		}
		if len(record) > 0 && strings.HasPrefix(strings.TrimLeft(strings.TrimSpace(record[0]), "\""), "#") {
			continue
		}
		if len(record) < 5 {
			return errors.New("APNIC user estimates row lacks its columns")
		}
		asn, ok := parseLabelASN(record[1])
		country := strings.ToLower(strings.TrimSpace(record[3]))
		users, usersErr := strconv.ParseUint(strings.TrimSpace(record[4]), 10, 64)
		if !ok || usersErr != nil || len(country) != 2 {
			return errors.New("APNIC user estimates row has an invalid ASN, country or count")
		}
		if into.usersByCountry[asn] == nil {
			into.usersByCountry[asn] = map[string]uint64{}
		}
		into.usersByCountry[asn][country] += users
		into.users[asn] += users
		if name := strings.TrimSpace(record[2]); into.names[asn] == "" && name != "" {
			into.names[asn] = name
		}
		if err := into.count(); err != nil {
			return err
		}
		rows++
	}
	if rows == 0 {
		return errors.New("APNIC user estimates have no rows")
	}
	into.hasUsers = true
	return nil
}

func loadAsnLabels(ctx context.Context, catalogPath string, sources []labelSource) (*asnLabels, error) {
	if len(sources) == 0 {
		return nil, nil
	}
	root, err := os.OpenRoot(filepath.Dir(catalogPath))
	if err != nil {
		return nil, err
	}
	defer root.Close()
	labels := &asnLabels{names: map[uint32]string{}, class: map[uint32]string{}, tags: map[uint32][]string{}, users: map[uint32]uint64{}, usersByCountry: map[uint32]map[string]uint64{}, hasTags: map[string]bool{}}
	for _, source := range sources {
		f, err := root.Open(source.File)
		if err != nil {
			return nil, err
		}
		parseErr := func() error {
			reader, err := openBoundedEvidenceReader(f, maxLabelDecompressedBytes)
			if err != nil {
				return err
			}
			switch source.Format {
			case labelFormatBgpToolsAsns:
				return readBgpToolsAsns(ctx, reader, labels)
			case labelFormatBgpToolsTag:
				return readBgpToolsTag(ctx, reader, source.Tag, labels)
			case labelFormatApnicAspop:
				return readApnicAspop(ctx, reader, labels)
			default:
				return errors.New("unsupported label source format")
			}
		}()
		closeErr := f.Close()
		if parseErr != nil {
			return nil, parseErr
		}
		if closeErr != nil {
			return nil, closeErr
		}
	}
	return labels, nil
}

func validLabelSource(source labelSource) bool {
	if !slices.Contains(labelFormats, source.Format) {
		return false
	}
	if source.Format == labelFormatBgpToolsTag {
		return source.Tag != "" && strings.TrimSpace(source.Tag) == source.Tag && !strings.ContainsAny(source.Tag, ",/\\ ")
	}
	return source.Tag == ""
}

// The independent view of one operator's ASNs, judged against its reviewed use.
type operatorIndependentLabels struct {
	Classes        map[string]int    `json:"bgp_tools_classes,omitempty"`
	Tags           []string          `json:"bgp_tools_tags,omitempty"`
	Users          uint64            `json:"apnic_users"`
	UsersByCountry map[string]uint64 `json:"apnic_users_by_country,omitempty"`
	Verdict        string            `json:"verdict"`
	Agreeing       []string          `json:"agreeing_signals,omitempty"`
	Disagreeing    []string          `json:"disagreeing_signals,omitempty"`
}

// Minimum APNIC user estimate treated as observed eyeball traffic. APNIC
// documents roughly 20% error and noisy small samples; this floor separates
// "some users seen" from noise, not small ISPs from large ones.
const labelMinimumUsers = 1000

// Verdicts compare independent signals with the reviewed use: agrees,
// disagrees, mixed (both), or unlabeled. For a subscriber operator, eyeball
// classes and tags and observed APNIC users agree; hosting, VPN, CDN and
// anycast labels disagree. For an operator reviewed as hosting, transit or an
// anonymizer the roles swap, except that APNIC users never disagree with an
// anonymizer, because APNIC credits VPN and relay egress with users.
func (self *asnLabels) operatorLabels(operator subscriberOperator) operatorIndependentLabels {
	result := operatorIndependentLabels{Classes: map[string]int{}, UsersByCountry: map[string]uint64{}, Tags: []string{}}
	eyeball, contrary := []string{}, []string{}
	for _, asn := range operator.ASNs {
		if class, ok := self.class[asn]; ok {
			result.Classes[class]++
			if slices.Contains(labelEyeballClasses, class) {
				eyeball = append(eyeball, "class-"+strings.ToLower(class))
			}
			if slices.Contains(labelContraryClass, class) {
				contrary = append(contrary, "class-"+strings.ToLower(class))
			}
		}
		for _, tag := range self.tags[asn] {
			if !slices.Contains(result.Tags, tag) {
				result.Tags = append(result.Tags, tag)
			}
			if slices.Contains(labelEyeballTags, tag) {
				eyeball = append(eyeball, "tag-"+tag)
			}
			if slices.Contains(labelContraryTags, tag) {
				contrary = append(contrary, "tag-"+tag)
			}
		}
		result.Users += self.users[asn]
		for country, users := range self.usersByCountry[asn] {
			result.UsersByCountry[country] += users
		}
	}
	observedUsers := self.hasUsers && result.Users >= labelMinimumUsers
	if operator.Usage == "subscriber" {
		if observedUsers {
			eyeball = append(eyeball, "apnic-users")
		}
		result.Agreeing, result.Disagreeing = eyeball, contrary
	} else {
		if observedUsers && !subscriberOriginNetworkUseRisk(operator.Usage) {
			eyeball = append(eyeball, "apnic-users")
		}
		result.Agreeing, result.Disagreeing = contrary, eyeball
	}
	slices.Sort(result.Tags)
	slices.Sort(result.Agreeing)
	slices.Sort(result.Disagreeing)
	result.Agreeing, result.Disagreeing = slices.Compact(result.Agreeing), slices.Compact(result.Disagreeing)
	switch {
	case len(result.Agreeing) != 0 && len(result.Disagreeing) != 0:
		result.Verdict = "mixed"
	case len(result.Agreeing) != 0:
		result.Verdict = "agrees"
	case len(result.Disagreeing) != 0:
		result.Verdict = "disagrees"
	default:
		result.Verdict = "unlabeled"
	}
	if len(result.Classes) == 0 {
		result.Classes = nil
	}
	if len(result.UsersByCountry) == 0 {
		result.UsersByCountry = nil
	}
	return result
}

// An unreviewed ASN that independent sources call eyeball, ranked by APNIC
// users in one country. Contrary labels are reported, not filtered, because
// a reviewer decides.
type labelReviewCandidate struct {
	ASN          uint32   `json:"asn"`
	Name         string   `json:"name,omitempty"`
	Users        uint64   `json:"apnic_users"`
	CountryShare float64  `json:"apnic_country_share"`
	Class        string   `json:"bgp_tools_class,omitempty"`
	Tags         []string `json:"bgp_tools_tags,omitempty"`
	Routed       bool     `json:"routed"`
	Contrary     bool     `json:"contrary_label"`
}
