package main

import (
	"bufio"
	"bytes"
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
	mmdb "github.com/oschwald/maxminddb-golang/v2"
)

// RIR whois assignment objects are the only free evidence below the ASN: an
// incumbent registers its DSL pools and its hosting blocks as separate
// inetnum/inet6num objects with descriptive names. On 2026-10-04, across the
// RIPE, APNIC and AFRINIC dumps, objects naming DATACENTER, DEDICATED, VPS,
// HOSTING or CLOUD sat under hosting-labelled origins 86-96% of the time, and
// a reviewed sample of such objects inside eyeball origins was hosting in 42
// of 45 cases. A keyword is not proof of use, so a hosting-named most-specific
// object withholds the identified-ISP inference rather than excluding it.
type registryAssignmentSource struct {
	countryEvidenceSource `yaml:",inline" json:",inline"`
	Format                string `yaml:"format" json:"format"`
}

const (
	registryAssignmentFormatRpsl           = "rpsl"
	maxRegistryAssignmentDecompressedBytes = 16 << 30
	maxRegistryAssignmentObjects           = 16 << 20
	maxRegistryAssignmentPieces            = 4 << 20
)

// Measured hosting tokens: share of matching objects under hosting origins at
// least 0.85 with at least 1,000 objects under labelled origins.
var registryHostingTokens = []string{"DATACENTER", "DEDICATED", "DEDI", "VPS", "HOSTING", "CLOUD"}

// Access-technology tokens, each under hosting origins at most 6% of the time.
// One in the same object cancels a hosting token ("DSL and hosting"). Generic
// words such as CUSTOMERS or USERS also name hosting customers, so they do not.
var registryAccessTokens = []string{"ADSL", "VDSL", "XDSL", "DSL", "FTTH", "FTTB", "FTTX", "GPON", "PON", "DOCSIS", "CABLE",
	"PPPOE", "PPP", "DHCP", "DYNAMIC", "DYN", "POOL", "CPE", "BROADBAND", "SUBSCRIBER", "SUBSCRIBERS", "RESIDENTIAL",
	"LTE", "3G", "4G", "UMTS", "GPRS", "WIMAX", "BNG", "BRAS", "CGN", "CGNAT"}

// Alphanumeric runs, plus each run without trailing digits so that numbered
// names such as VPS2 or HOSTING01 match their token.
func registryNameTokens(text string) []string {
	tokens := strings.FieldsFunc(strings.ToUpper(text), func(r rune) bool {
		return !(r >= 'A' && r <= 'Z' || r >= '0' && r <= '9')
	})
	for _, token := range tokens {
		trimmed := strings.TrimRight(token, "0123456789")
		if trimmed != token && len(trimmed) >= 2 {
			tokens = append(tokens, trimmed)
		}
	}
	return tokens
}

// hosting, access or other, from the object's netname and descr lines.
func registryAssignmentKind(netname string, descr []string) string {
	tokens := registryNameTokens(netname + " " + strings.Join(descr, " "))
	hosting, access := false, false
	for _, token := range tokens {
		hosting = hosting || slices.Contains(registryHostingTokens, token)
		access = access || slices.Contains(registryAccessTokens, token)
	}
	switch {
	case hosting && !access:
		return "hosting"
	case access && !hosting:
		return "access"
	default:
		return "other"
	}
}

type registryAssignment struct {
	first, last netip.Addr
	kind        string
	netname     string
	source      string
}

func parseRegistryRange(key string, value string) (netip.Addr, netip.Addr, bool) {
	value = strings.TrimSpace(value)
	if key == "inet6num" {
		prefix, err := netip.ParsePrefix(value)
		if err != nil || !prefix.Addr().Is6() || prefix.Addr().Is4In6() {
			return netip.Addr{}, netip.Addr{}, false
		}
		prefix = prefix.Masked()
		return prefix.Addr(), lastAddress(prefix), true
	}
	first, last, found := strings.Cut(value, "-")
	if !found {
		return netip.Addr{}, netip.Addr{}, false
	}
	a, errA := netip.ParseAddr(strings.TrimSpace(first))
	b, errB := netip.ParseAddr(strings.TrimSpace(last))
	if errA != nil || errB != nil || !a.Is4() || !b.Is4() || b.Less(a) {
		return netip.Addr{}, netip.Addr{}, false
	}
	return a, b, true
}

func lastAddress(prefix netip.Prefix) netip.Addr {
	bytes := prefix.Masked().Addr().AsSlice()
	for bit := prefix.Bits(); bit < len(bytes)*8; bit++ {
		bytes[bit/8] |= 1 << (7 - bit%8)
	}
	last, _ := netip.AddrFromSlice(bytes)
	return last
}

// The largest aligned prefixes exactly covering an inclusive range.
func rangePrefixes(first, last netip.Addr) []netip.Prefix {
	prefixes := []netip.Prefix{}
	for first.IsValid() && !last.Less(first) {
		bits := first.BitLen()
		for bits > 0 {
			candidate := netip.PrefixFrom(first, bits-1).Masked()
			if candidate.Addr() != first || lastAddress(candidate).Compare(last) > 0 {
				break
			}
			bits--
		}
		prefix := netip.PrefixFrom(first, bits)
		prefixes = append(prefixes, prefix)
		end := lastAddress(prefix)
		if end == last {
			break
		}
		first = end.Next()
	}
	return prefixes
}

// Streams RPSL objects; each inetnum or inet6num object yields one callback.
// Continuation lines and other object classes are skipped. A malformed range
// skips that object, because registries carry historic oddities, but a source
// without any parsable object fails.
func scanRpslAssignments(ctx context.Context, raw io.Reader, source string, visit func(registryAssignment) error) (int, error) {
	reader, err := openBoundedEvidenceReader(raw, maxRegistryAssignmentDecompressedBytes)
	if err != nil {
		return 0, err
	}
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 1<<20)
	var current *registryAssignment
	var descr []string
	objects := 0
	flush := func() error {
		if current == nil {
			return nil
		}
		current.kind = registryAssignmentKind(current.netname, descr)
		objects++
		if objects > maxRegistryAssignmentObjects {
			return errors.New("registry assignment source exceeds its object bound")
		}
		err := visit(*current)
		current, descr = nil, nil
		return err
	}
	lines := 0
	for scanner.Scan() {
		lines++
		if lines%65536 == 0 {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
		}
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			if err := flush(); err != nil {
				return 0, err
			}
			continue
		}
		if line[0] == ' ' || line[0] == '\t' || line[0] == '+' || line[0] == '#' || line[0] == '%' {
			continue
		}
		colon := bytes.IndexByte(line, ':')
		if colon <= 0 {
			continue
		}
		key := strings.ToLower(string(bytes.TrimSpace(line[:colon])))
		value := string(bytes.TrimSpace(line[colon+1:]))
		switch key {
		case "inetnum", "inet6num":
			if err := flush(); err != nil {
				return 0, err
			}
			first, last, ok := parseRegistryRange(key, value)
			if ok {
				current = &registryAssignment{first: first, last: last, source: source}
			}
		case "netname":
			if current != nil {
				current.netname = value
			}
		case "descr":
			if current != nil && len(descr) < 8 {
				descr = append(descr, value)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return 0, errors.New("registry assignment source is truncated or unreadable")
	}
	if err := flush(); err != nil {
		return 0, err
	}
	if objects == 0 {
		return 0, errors.New("registry assignment source has no inetnum or inet6num objects")
	}
	return objects, nil
}

// Disjoint, sorted cover of the hosting objects, for nesting checks.
type addressIntervals struct {
	first, last []netip.Addr
}

func newAddressIntervals(ranges []registryAssignment) addressIntervals {
	slices.SortFunc(ranges, func(a, b registryAssignment) int { return a.first.Compare(b.first) })
	intervals := addressIntervals{}
	for _, r := range ranges {
		n := len(intervals.first)
		if n > 0 && intervals.first[n-1].BitLen() == r.first.BitLen() && (!intervals.last[n-1].Less(r.first) || intervals.last[n-1].Next() == r.first) {
			if intervals.last[n-1].Less(r.last) {
				intervals.last[n-1] = r.last
			}
			continue
		}
		intervals.first = append(intervals.first, r.first)
		intervals.last = append(intervals.last, r.last)
	}
	return intervals
}

func (self addressIntervals) contains(first, last netip.Addr) bool {
	i, found := slices.BinarySearchFunc(self.first, first, func(a, b netip.Addr) int { return a.Compare(b) })
	if !found {
		i--
	}
	return i >= 0 && self.first[i].BitLen() == first.BitLen() && !self.last[i].Less(last)
}

type registryAssignmentIndex struct {
	reader         *mmdb.Reader
	objects        int
	hostingObjects int
	nestedObjects  int
}

// Two passes per source: collect hosting-named objects, then every object
// nested inside one of them, so that a more-specific non-hosting object (a
// DSL pool inside a block named for hosting) overrides its parent. The kept
// objects are inserted broadest first into an in-memory tree.
func loadRegistryAssignments(ctx context.Context, catalogPath string, sources []registryAssignmentSource) (*registryAssignmentIndex, error) {
	if len(sources) == 0 {
		return nil, nil
	}
	root, err := os.OpenRoot(filepath.Dir(catalogPath))
	if err != nil {
		return nil, err
	}
	defer root.Close()
	scan := func(source registryAssignmentSource, visit func(registryAssignment) error) (int, error) {
		if source.Format != registryAssignmentFormatRpsl {
			return 0, errors.New("unsupported registry assignment format")
		}
		f, err := root.Open(source.File)
		if err != nil {
			return 0, err
		}
		n, scanErr := scanRpslAssignments(ctx, f, source.Id, visit)
		closeErr := f.Close()
		if scanErr != nil {
			return 0, scanErr
		}
		return n, closeErr
	}
	index := &registryAssignmentIndex{}
	hosting := []registryAssignment{}
	for _, source := range sources {
		n, err := scan(source, func(object registryAssignment) error {
			if object.kind == "hosting" {
				hosting = append(hosting, object)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		index.objects += n
	}
	index.hostingObjects = len(hosting)
	kept := slices.Clone(hosting)
	cover := newAddressIntervals(hosting)
	for _, source := range sources {
		if _, err := scan(source, func(object registryAssignment) error {
			if object.kind != "hosting" && cover.contains(object.first, object.last) {
				kept = append(kept, object)
				index.nestedObjects++
			}
			return nil
		}); err != nil {
			return nil, err
		}
	}
	type piece struct {
		prefix netip.Prefix
		object int
	}
	pieces := []piece{}
	for i, object := range kept {
		for _, prefix := range rangePrefixes(object.first, object.last) {
			if nonGlobalPrefix(prefix) {
				continue
			}
			pieces = append(pieces, piece{prefix, i})
			if len(pieces) > maxRegistryAssignmentPieces {
				return nil, errors.New("registry assignments exceed their prefix bound")
			}
		}
	}
	// Broadest first; at equal size a non-hosting object is inserted last so
	// that duplicate registrations never manufacture a hosting finding.
	slices.SortStableFunc(pieces, func(a, b piece) int {
		if a.prefix.Bits() != b.prefix.Bits() {
			return a.prefix.Bits() - b.prefix.Bits()
		}
		ak, bk := kept[a.object].kind == "hosting", kept[b.object].kind == "hosting"
		if ak != bk {
			if ak {
				return -1
			}
			return 1
		}
		return a.prefix.Addr().Compare(b.prefix.Addr())
	})
	writer, err := mmdbwriter.New(mmdbwriter.Options{DatabaseType: "registry assignments", IncludeReservedNetworks: true, RecordSize: 32, Description: map[string]string{"en": "hosting-named registry assignments"}})
	if err != nil {
		return nil, err
	}
	for _, p := range pieces {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		object := kept[p.object]
		value := mmdbtype.Map{"kind": mmdbtype.String(object.kind)}
		if object.kind == "hosting" {
			value["netname"], value["source"] = mmdbtype.String(truncateRunes(object.netname, 64)), mmdbtype.String(object.source)
		}
		_, network, _ := net.ParseCIDR(p.prefix.String())
		if err := writer.Insert(network, value); err != nil {
			return nil, err
		}
	}
	var content bytes.Buffer
	if _, err := writer.WriteTo(&content); err != nil {
		return nil, err
	}
	index.reader, err = mmdb.OpenBytes(content.Bytes())
	if err != nil {
		return nil, err
	}
	return index, nil
}

func truncateRunes(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit])
}

// Hosting cells inside a prefix, at the registry object's own boundary.
func (self *registryAssignmentIndex) hostingCells(prefix netip.Prefix, visit func(netip.Prefix, string, string) error) error {
	if self == nil {
		return nil
	}
	for result := range self.reader.NetworksWithin(prefix) {
		if err := result.Err(); err != nil {
			return err
		}
		var value struct {
			Kind    string `maxminddb:"kind"`
			Netname string `maxminddb:"netname"`
			Source  string `maxminddb:"source"`
		}
		if err := result.Decode(&value); err != nil {
			return err
		}
		if value.Kind != "hosting" {
			continue
		}
		cell := result.Prefix()
		if cell.Bits() < prefix.Bits() {
			cell = prefix
		}
		if err := visit(cell, value.Netname, value.Source); err != nil {
			return err
		}
	}
	return nil
}

func (self *registryAssignmentIndex) close() {
	if self != nil && self.reader != nil {
		self.reader.Close()
	}
}
