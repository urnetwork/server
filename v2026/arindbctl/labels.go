package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// Independent per-ASN labels validate the reviewed catalog; they never
// classify an address. Each source uses a different method from our identity
// review: bgp.tools curates classes and tags, APNIC Labs estimates users from
// ad impressions, ASdb classifies industries from business data, ipverse
// derives categories from registry and routing signals, and Linnaeus predicts
// categories with a model trained on hand labels. Agreement corroborates an
// entry and disagreement queues it for review. The audit is their only reader.
//
// Measured against the 1,978 Linnaeus hand labels on 2026-10-04 (300 pure
// eyeball, 160 pure hosting ASNs), eyeball precision was bgp.tools 0.961,
// ipverse 0.942, Linnaeus 0.927 on its validation split, ASdb 0.823 and APNIC
// 0.802; two or more agreeing sources with none contrary reached 0.969 and
// three or more 0.988. The audit re-measures this on every run when the hand
// labels are supplied.
type labelSource struct {
	countryEvidenceSource `yaml:",inline" json:",inline"`
	Format                string `yaml:"format" json:"format"`
	Tag                   string `yaml:"tag,omitempty" json:"tag,omitempty"`
}

const (
	labelFormatBgpToolsAsns   = "bgp-tools-asns-csv"
	labelFormatBgpToolsTag    = "bgp-tools-tag-csv"
	labelFormatApnicAspop     = "apnic-aspop-csv"
	labelFormatAsdb           = "asdb-categorized-csv"
	labelFormatIpverse        = "ipverse-as-json"
	labelFormatLinnaeusPred   = "linnaeus-predictions-csv"
	labelFormatLinnaeusLabels = "linnaeus-labels-csv"
	labelFormatLinnaeusSplits = "linnaeus-splits-csv"
	maxLabelDecompressedBytes = 512 << 20
	maxLabelRecords           = 8 << 20
)

var labelFormats = []string{labelFormatBgpToolsAsns, labelFormatBgpToolsTag, labelFormatApnicAspop, labelFormatAsdb, labelFormatIpverse, labelFormatLinnaeusPred, labelFormatLinnaeusLabels, labelFormatLinnaeusSplits}

// Independent signal sources, in report order.
var labelSignalSources = []string{"bgp.tools", "apnic", "asdb", "ipverse", "linnaeus"}

// bgp.tools tags and classes. "biznet" is business broadband, which the
// subscriber policy includes. "tor" means an ASN hosts exits, which access
// ISPs do too, and "anycast" marks incumbents' DNS and CDN services, so
// neither is evidence either way.
var (
	labelContraryTags   = []string{"vpsh", "vpn", "cdn"}
	labelEyeballTags    = []string{"dsl", "mobile", "satnet", "biznet"}
	labelEyeballClasses = []string{"Eyeball"}
	labelContraryClass  = []string{"Content"}
	asdbEyeballLayer2   = []string{"Internet Service Provider (ISP)", "Phone Provider"}
	asdbHostingLayer2   = []string{"Hosting and Cloud Provider"}
	linnaeusEyeballCols = []string{"Access_LargeISP", "Access_SmallISP", "Mobile", "Satellite"}
	linnaeusHostingCols = []string{"ContentProvider_Cloud", "ContentProvider_Hosting", "ContentProvider_CDN", "VPNs"}
)

// Minimum APNIC user estimate treated as observed eyeball traffic. APNIC
// documents roughly 20% error and noisy small samples; this floor separates
// "some users seen" from noise, not small ISPs from large ones.
const labelMinimumUsers = 1000

type labelSignal struct {
	eyeball, hosting bool
}

type asnLabels struct {
	names          map[uint32]string
	class          map[uint32]string
	tags           map[uint32][]string
	users          map[uint32]uint64
	usersByCountry map[uint32]map[string]uint64
	signals        map[string]map[uint32]labelSignal
	truth          map[uint32]labelSignal
	split          map[uint32]string
	hasClass       bool
	hasTags        map[string]bool
	hasUsers       bool
	records        int
}

func newAsnLabels() *asnLabels {
	return &asnLabels{names: map[uint32]string{}, class: map[uint32]string{}, tags: map[uint32][]string{}, users: map[uint32]uint64{},
		usersByCountry: map[uint32]map[string]uint64{}, signals: map[string]map[uint32]labelSignal{}, hasTags: map[string]bool{}}
}

func (self *asnLabels) count() error {
	self.records++
	if self.records > maxLabelRecords {
		return errors.New("label sources exceed their record bound")
	}
	return nil
}

func (self *asnLabels) signal(source string, asn uint32, eyeball, hosting bool) {
	if !eyeball && !hosting {
		return
	}
	if self.signals[source] == nil {
		self.signals[source] = map[uint32]labelSignal{}
	}
	current := self.signals[source][asn]
	self.signals[source][asn] = labelSignal{current.eyeball || eyeball, current.hosting || hosting}
}

// A numeric ASN with or without the AS prefix. AS0 parses but names no
// operator, so readers skip it: ASdb lists it as a reserved row.
func parseLabelASN(text string) (uint32, bool) {
	text = strings.TrimSpace(strings.Trim(strings.TrimSpace(text), "\""))
	text = strings.TrimPrefix(strings.TrimPrefix(text, "AS"), "as")
	asn, err := strconv.ParseUint(text, 10, 32)
	return uint32(asn), err == nil
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
		if asn == 0 {
			continue
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
		if asn == 0 {
			continue
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
		if asn == 0 {
			continue
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

// ASN,"Category 1 - Layer 1","Category 1 - Layer 2",... with AS-prefixed ASNs.
// Every listed category applies; layer-2 names decide the signal.
func readAsdb(ctx context.Context, reader io.Reader, into *asnLabels) error {
	records := csv.NewReader(reader)
	records.FieldsPerRecord = -1
	header, err := records.Read()
	if err != nil || len(header) < 3 || strings.TrimSpace(header[0]) != "ASN" || !strings.Contains(header[2], "Layer 2") {
		return errors.New("ASdb file lacks its ASN and category header")
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
		if err != nil || len(record) < 2 {
			return errors.New("ASdb row is malformed")
		}
		asn, ok := parseLabelASN(record[0])
		if !ok {
			return errors.New("ASdb row has an invalid ASN")
		}
		if asn == 0 {
			continue
		}
		eyeball, hosting := false, false
		for i := 2; i < len(record); i += 2 {
			eyeball = eyeball || slices.Contains(asdbEyeballLayer2, record[i])
			hosting = hosting || slices.Contains(asdbHostingLayer2, record[i])
		}
		into.signal("asdb", asn, eyeball, hosting)
		if err := into.count(); err != nil {
			return err
		}
		rows++
	}
	if rows == 0 {
		return errors.New("ASdb file has no rows")
	}
	return nil
}

// A JSON array of {"asn":N,"metadata":{"category":...,"description":...}}.
func readIpverse(ctx context.Context, reader io.Reader, into *asnLabels) error {
	decoder := json.NewDecoder(reader)
	if token, err := decoder.Token(); err != nil || token != json.Delim('[') {
		return errors.New("ipverse metadata is not a JSON array")
	}
	rows := 0
	for decoder.More() {
		if rows%4096 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		var entry struct {
			ASN      *uint32 `json:"asn"`
			Metadata *struct {
				Category    *string `json:"category"`
				Description string  `json:"description"`
			} `json:"metadata"`
		}
		if err := decoder.Decode(&entry); err != nil || entry.ASN == nil {
			return errors.New("ipverse metadata entry is malformed")
		}
		if entry.Metadata != nil && entry.Metadata.Category != nil && *entry.ASN != 0 {
			into.signal("ipverse", *entry.ASN, *entry.Metadata.Category == "isp", *entry.Metadata.Category == "hosting")
			if into.names[*entry.ASN] == "" {
				into.names[*entry.ASN] = entry.Metadata.Description
			}
		}
		if err := into.count(); err != nil {
			return err
		}
		rows++
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim(']') || rows == 0 {
		return errors.New("ipverse metadata is truncated or empty")
	}
	return nil
}

// The Linnaeus sublevel schema: asn then one 0/1 column per category.
func readLinnaeusMatrix(ctx context.Context, reader io.Reader, visit func(uint32, labelSignal)) error {
	records := csv.NewReader(reader)
	header, err := records.Read()
	if err != nil || len(header) < 2 || header[0] != "asn" {
		return errors.New("Linnaeus file lacks its asn header")
	}
	eyeballIndex, hostingIndex := []int{}, []int{}
	for i, name := range header {
		if slices.Contains(linnaeusEyeballCols, name) {
			eyeballIndex = append(eyeballIndex, i)
		}
		if slices.Contains(linnaeusHostingCols, name) {
			hostingIndex = append(hostingIndex, i)
		}
	}
	if len(eyeballIndex) != len(linnaeusEyeballCols) || len(hostingIndex) != len(linnaeusHostingCols) {
		return errors.New("Linnaeus file lacks the sublevel access, mobile, satellite, content and VPN columns")
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
		if err != nil {
			return errors.New("Linnaeus row is malformed")
		}
		asn, ok := parseLabelASN(record[0])
		if !ok {
			return errors.New("Linnaeus row has an invalid ASN")
		}
		if asn == 0 {
			continue
		}
		value := labelSignal{}
		for _, i := range eyeballIndex {
			value.eyeball = value.eyeball || record[i] == "1"
		}
		for _, i := range hostingIndex {
			value.hosting = value.hosting || record[i] == "1"
		}
		visit(asn, value)
		rows++
	}
	if rows == 0 {
		return errors.New("Linnaeus file has no rows")
	}
	return nil
}

// asn,split with train, val or test; val and test are both held out.
func readLinnaeusSplits(ctx context.Context, reader io.Reader, into *asnLabels) error {
	records := csv.NewReader(reader)
	header, err := records.Read()
	if err != nil || len(header) != 2 || header[0] != "asn" || header[1] != "split" {
		return errors.New("Linnaeus splits lack their asn,split header")
	}
	into.split = map[uint32]string{}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		record, err := records.Read()
		if err == io.EOF {
			break
		}
		asn, ok := uint32(0), false
		if err == nil {
			asn, ok = parseLabelASN(record[0])
		}
		if err != nil || !ok || !slices.Contains([]string{"train", "val", "test"}, record[1]) {
			return errors.New("Linnaeus split row is malformed")
		}
		into.split[asn] = record[1]
	}
	if len(into.split) == 0 {
		return errors.New("Linnaeus splits are empty")
	}
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
	labels := newAsnLabels()
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
			return readLabelSource(ctx, source, reader, labels)
		}()
		closeErr := f.Close()
		if parseErr != nil {
			return nil, parseErr
		}
		if closeErr != nil {
			return nil, closeErr
		}
	}
	labels.deriveSignals()
	return labels, nil
}

func readLabelSource(ctx context.Context, source labelSource, reader io.Reader, labels *asnLabels) error {
	switch source.Format {
	case labelFormatBgpToolsAsns:
		return readBgpToolsAsns(ctx, reader, labels)
	case labelFormatBgpToolsTag:
		return readBgpToolsTag(ctx, reader, source.Tag, labels)
	case labelFormatApnicAspop:
		return readApnicAspop(ctx, reader, labels)
	case labelFormatAsdb:
		return readAsdb(ctx, reader, labels)
	case labelFormatIpverse:
		return readIpverse(ctx, reader, labels)
	case labelFormatLinnaeusPred:
		return readLinnaeusMatrix(ctx, reader, func(asn uint32, value labelSignal) { labels.signal("linnaeus", asn, value.eyeball, value.hosting) })
	case labelFormatLinnaeusLabels:
		if labels.truth == nil {
			labels.truth = map[uint32]labelSignal{}
		}
		return readLinnaeusMatrix(ctx, reader, func(asn uint32, value labelSignal) { labels.truth[asn] = value })
	case labelFormatLinnaeusSplits:
		return readLinnaeusSplits(ctx, reader, labels)
	default:
		return errors.New("unsupported label source format")
	}
}

// bgp.tools and APNIC signals derive from their raw classes, tags and users.
func (self *asnLabels) deriveSignals() {
	for asn, class := range self.class {
		self.signal("bgp.tools", asn, slices.Contains(labelEyeballClasses, class), slices.Contains(labelContraryClass, class))
	}
	for asn, tags := range self.tags {
		for _, tag := range tags {
			self.signal("bgp.tools", asn, slices.Contains(labelEyeballTags, tag), slices.Contains(labelContraryTags, tag))
		}
	}
	for asn, users := range self.users {
		self.signal("apnic", asn, users >= labelMinimumUsers, false)
	}
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
	Corroborated   bool              `json:"corroborated"`
	Agreeing       []string          `json:"agreeing_sources,omitempty"`
	Disagreeing    []string          `json:"disagreeing_sources,omitempty"`
	GroundTruth    string            `json:"ground_truth,omitempty"`
}

// Per source, whether any of the ASNs carries an eyeball or hosting signal.
func (self *asnLabels) sourceSignals(asns []uint32) map[string]labelSignal {
	result := map[string]labelSignal{}
	for _, source := range labelSignalSources {
		value := labelSignal{}
		for _, asn := range asns {
			s := self.signals[source][asn]
			value.eyeball, value.hosting = value.eyeball || s.eyeball, value.hosting || s.hosting
		}
		if value.eyeball || value.hosting {
			result[source] = value
		}
	}
	return result
}

// Verdicts compare independent sources with the reviewed use: agrees,
// disagrees, mixed (both), or unlabeled. For a subscriber operator, eyeball
// signals agree and hosting signals disagree. For hosting, transit and
// anonymizer operators the roles swap, except that APNIC users never disagree
// with them, because APNIC credits hosting, VPN and relay egress with users.
// Corroborated means two or more agreeing sources and none disagreeing, the
// rule measured at 0.969 eyeball precision.
func (self *asnLabels) operatorLabels(operator subscriberOperator) operatorIndependentLabels {
	result := operatorIndependentLabels{Classes: map[string]int{}, UsersByCountry: map[string]uint64{}, Tags: []string{}}
	for _, asn := range operator.ASNs {
		if class, ok := self.class[asn]; ok {
			result.Classes[class]++
		}
		for _, tag := range self.tags[asn] {
			if !slices.Contains(result.Tags, tag) {
				result.Tags = append(result.Tags, tag)
			}
		}
		result.Users += self.users[asn]
		for country, users := range self.usersByCountry[asn] {
			result.UsersByCountry[country] += users
		}
	}
	for source, value := range self.sourceSignals(operator.ASNs) {
		agree, disagree := value.eyeball, value.hosting
		if operator.Usage != "subscriber" {
			agree, disagree = value.hosting, value.eyeball && source != "apnic"
		}
		if agree {
			result.Agreeing = append(result.Agreeing, source)
		}
		if disagree {
			result.Disagreeing = append(result.Disagreeing, source)
		}
	}
	slices.Sort(result.Tags)
	slices.Sort(result.Agreeing)
	slices.Sort(result.Disagreeing)
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
	result.Corroborated = len(result.Agreeing) >= 2 && len(result.Disagreeing) == 0
	if self.truth != nil {
		truth := labelSignal{}
		labeled := false
		for _, asn := range operator.ASNs {
			if t, ok := self.truth[asn]; ok {
				labeled = true
				truth.eyeball, truth.hosting = truth.eyeball || t.eyeball, truth.hosting || t.hosting
			}
		}
		if labeled {
			result.GroundTruth = labelSignalName(truth)
		}
	}
	if len(result.Classes) == 0 {
		result.Classes = nil
	}
	if len(result.UsersByCountry) == 0 {
		result.UsersByCountry = nil
	}
	return result
}

func labelSignalName(value labelSignal) string {
	switch {
	case value.eyeball && value.hosting:
		return "mixed"
	case value.eyeball:
		return "eyeball"
	case value.hosting:
		return "hosting"
	default:
		return "other"
	}
}

// Precision and recall of one source against the hand labels' pure classes.
type labelSourceQuality struct {
	Source           string  `json:"source"`
	Scope            string  `json:"scope"`
	PureEyeball      int     `json:"ground_truth_pure_eyeball"`
	PureHosting      int     `json:"ground_truth_pure_hosting"`
	EyeballTP        int     `json:"eyeball_true_positives"`
	EyeballFP        int     `json:"eyeball_false_positives"`
	HostingTP        int     `json:"hosting_true_positives"`
	HostingFP        int     `json:"hosting_false_positives"`
	EyeballPrecision float64 `json:"eyeball_precision"`
	EyeballRecall    float64 `json:"eyeball_recall"`
	HostingPrecision float64 `json:"hosting_precision"`
	HostingRecall    float64 `json:"hosting_recall"`
}

func ratio(n, d int) float64 {
	if d == 0 {
		return 0
	}
	return math.Round(1000*float64(n)/float64(d)) / 1000
}

// Each source, and the two- and three-source consensus rules, scored against
// the hand labels. Linnaeus was trained on the hand labels, so with splits it
// is scored on its held-out validation and test splits only; without them its
// scope says so.
func (self *asnLabels) quality() []labelSourceQuality {
	if self == nil || self.truth == nil {
		return nil
	}
	score := func(name, scope string, call func(uint32) labelSignal, include func(uint32) bool) labelSourceQuality {
		q := labelSourceQuality{Source: name, Scope: scope}
		for asn, truth := range self.truth {
			if !include(asn) || truth.eyeball == truth.hosting {
				continue
			}
			if truth.eyeball {
				q.PureEyeball++
			} else {
				q.PureHosting++
			}
			c := call(asn)
			switch {
			case c.eyeball && !c.hosting && truth.eyeball:
				q.EyeballTP++
			case c.eyeball && !c.hosting:
				q.EyeballFP++
			case c.hosting && !c.eyeball && truth.hosting:
				q.HostingTP++
			case c.hosting && !c.eyeball:
				q.HostingFP++
			}
		}
		q.EyeballPrecision, q.EyeballRecall = ratio(q.EyeballTP, q.EyeballTP+q.EyeballFP), ratio(q.EyeballTP, q.PureEyeball)
		q.HostingPrecision, q.HostingRecall = ratio(q.HostingTP, q.HostingTP+q.HostingFP), ratio(q.HostingTP, q.PureHosting)
		return q
	}
	all := func(uint32) bool { return true }
	result := []labelSourceQuality{}
	for _, source := range labelSignalSources {
		if len(self.signals[source]) == 0 {
			continue
		}
		scope, include := "all", all
		if source == "linnaeus" {
			scope = "trained-on-ground-truth"
			if self.split != nil {
				scope, include = "held-out-split", func(asn uint32) bool { return self.split[asn] == "val" || self.split[asn] == "test" }
			}
		}
		signals := self.signals[source]
		result = append(result, score(source, scope, func(asn uint32) labelSignal { return signals[asn] }, include))
	}
	// Consensus excludes Linnaeus, whose hand labels are the ground truth.
	independent := []string{}
	for _, source := range labelSignalSources {
		if source != "linnaeus" && len(self.signals[source]) != 0 {
			independent = append(independent, source)
		}
	}
	for _, need := range []int{2, 3} {
		if len(independent) < need {
			continue
		}
		result = append(result, score("consensus-"+strconv.Itoa(need), "all", func(asn uint32) labelSignal {
			eyeball, hosting := 0, 0
			for _, source := range independent {
				s := self.signals[source][asn]
				if s.eyeball {
					eyeball++
				}
				if s.hosting {
					hosting++
				}
			}
			return labelSignal{eyeball >= need && hosting == 0, hosting >= need && eyeball == 0}
		}, all))
	}
	return result
}

// Independent eyeball and hosting source counts for one unreviewed ASN.
func (self *asnLabels) consensus(asn uint32) (int, int) {
	eyeball, hosting := 0, 0
	for _, source := range labelSignalSources {
		s := self.signals[source][asn]
		if s.eyeball {
			eyeball++
		}
		if s.hosting {
			hosting++
		}
	}
	return eyeball, hosting
}

// An unreviewed ASN with an independent eyeball signal, ranked by APNIC users
// in one country. Contrary signals are reported, not filtered: a reviewer
// decides.
type labelReviewCandidate struct {
	ASN            uint32   `json:"asn"`
	Name           string   `json:"name,omitempty"`
	Users          uint64   `json:"apnic_users"`
	CountryShare   float64  `json:"apnic_country_share"`
	Class          string   `json:"bgp_tools_class,omitempty"`
	Tags           []string `json:"bgp_tools_tags,omitempty"`
	EyeballSources int      `json:"eyeball_sources"`
	HostingSources int      `json:"hosting_sources"`
	Corroborated   bool     `json:"corroborated"`
	Routed         bool     `json:"routed"`
	Contrary       bool     `json:"contrary_label"`
}
