package main

import (
	"bufio"
	"compress/gzip"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// Route origin authorization is corroboration, not use evidence. A route whose
// origin is RPKI-invalid is a leak, hijack or misconfiguration until proven
// otherwise, so the identified-ISP inference is withheld there. Valid and
// not-found routes are unchanged; no risk is derived from RPKI state.
type rpkiSource struct {
	countryEvidenceSource `yaml:",inline" json:",inline"`
	Format                string `yaml:"format" json:"format"`
}

const (
	rpkiFormatClientJson     = "rpki-client-json"
	rpkiFormatRoutinatorCsv  = "routinator-csv"
	maxRpkiDecompressedBytes = 1 << 30
	maxRpkiRecords           = 4 << 20
)

type rpkiAuthorization struct {
	asn       uint32
	maxLength int
}

// Validated ROA payloads indexed by their exact prefix.
type rpkiAuthorizations struct {
	byPrefix map[netip.Prefix][]rpkiAuthorization
	count    int
}

// AS0 payloads are valid RPKI content (RFC 7607): they authorize no origin,
// so every announcement they cover is invalid.
func parseRpkiASN(text string) (uint32, bool) {
	text = strings.TrimPrefix(strings.TrimSpace(text), "AS")
	asn, err := strconv.ParseUint(text, 10, 32)
	return uint32(asn), err == nil
}

func (self *rpkiAuthorizations) add(asn uint32, prefix netip.Prefix, maxLength int) error {
	if prefix != prefix.Masked() || prefix.Addr().Is4In6() || maxLength < prefix.Bits() || maxLength > prefix.Addr().BitLen() {
		return errors.New("RPKI payload has an invalid prefix or maximum length")
	}
	if self.byPrefix == nil {
		self.byPrefix = map[netip.Prefix][]rpkiAuthorization{}
	}
	self.byPrefix[prefix] = append(self.byPrefix[prefix], rpkiAuthorization{asn: asn, maxLength: maxLength})
	self.count++
	if self.count > maxRpkiRecords {
		return errors.New("RPKI snapshot exceeds its record bound")
	}
	return nil
}

// Gzip is detected by its magic bytes; plain input also works. The
// decompressed size is bounded before any record is retained.
func openBoundedEvidenceReader(reader io.Reader, limit int64) (io.Reader, error) {
	buffered := bufio.NewReader(reader)
	magic, err := buffered.Peek(2)
	if err == nil && magic[0] == 0x1f && magic[1] == 0x8b {
		zip, err := gzip.NewReader(buffered)
		if err != nil {
			return nil, errors.New("invalid compressed evidence snapshot")
		}
		return io.LimitReader(zip, limit+1), nil
	}
	return io.LimitReader(buffered, limit+1), nil
}

// rpki-client and Cloudflare publish {"metadata":…,"roas":[{asn,prefix,maxLength,…}]}
// with integer or "AS"-prefixed ASNs.
func readRpkiClientJson(ctx context.Context, reader io.Reader, into *rpkiAuthorizations) error {
	decoder := json.NewDecoder(reader)
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return errors.New("RPKI JSON snapshot is malformed")
	}
	sawRoas := false
	for decoder.More() {
		if err := ctx.Err(); err != nil {
			return err
		}
		key, err := decoder.Token()
		if err != nil {
			return errors.New("RPKI JSON snapshot is malformed")
		}
		if key != "roas" {
			var skipped json.RawMessage
			if err := decoder.Decode(&skipped); err != nil {
				return errors.New("RPKI JSON snapshot is malformed")
			}
			continue
		}
		sawRoas = true
		if token, err := decoder.Token(); err != nil || token != json.Delim('[') {
			return errors.New("RPKI JSON snapshot has no payload array")
		}
		for decoder.More() {
			if err := ctx.Err(); err != nil {
				return err
			}
			var payload struct {
				ASN       json.RawMessage `json:"asn"`
				Prefix    string          `json:"prefix"`
				MaxLength int             `json:"maxLength"`
			}
			if err := decoder.Decode(&payload); err != nil {
				return errors.New("RPKI JSON payload is malformed")
			}
			asn, ok := parseRpkiASN(strings.Trim(string(payload.ASN), "\""))
			prefix, prefixErr := netip.ParsePrefix(payload.Prefix)
			if !ok || prefixErr != nil {
				return errors.New("RPKI JSON payload has an invalid ASN or prefix")
			}
			if err := into.add(asn, prefix, payload.MaxLength); err != nil {
				return err
			}
		}
		if token, err := decoder.Token(); err != nil || token != json.Delim(']') {
			return errors.New("RPKI JSON payload array is truncated")
		}
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim('}') || !sawRoas {
		return errors.New("RPKI JSON snapshot is truncated or lacks payloads")
	}
	return nil
}

// Routinator csv/csvext and the RIPE NCC daily archive share named columns:
// ASN, IP Prefix and Max Length; any additional columns are ignored.
func readRoutinatorCsv(ctx context.Context, reader io.Reader, into *rpkiAuthorizations) error {
	records := csv.NewReader(reader)
	records.FieldsPerRecord = -1
	records.ReuseRecord = true
	header, err := records.Read()
	if err != nil {
		return errors.New("RPKI CSV snapshot has no header")
	}
	columns := map[string]int{}
	for i, name := range header {
		columns[strings.ToLower(strings.TrimSpace(name))] = i
	}
	asnColumn, hasASN := columns["asn"]
	prefixColumn, hasPrefix := columns["ip prefix"]
	lengthColumn, hasLength := columns["max length"]
	if !hasASN || !hasPrefix || !hasLength {
		return errors.New("RPKI CSV snapshot lacks ASN, IP Prefix and Max Length columns")
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
		if err != nil || len(record) <= max(asnColumn, prefixColumn, lengthColumn) {
			return errors.New("RPKI CSV payload is malformed")
		}
		asn, ok := parseRpkiASN(record[asnColumn])
		prefix, prefixErr := netip.ParsePrefix(strings.TrimSpace(record[prefixColumn]))
		maxLength, lengthErr := strconv.Atoi(strings.TrimSpace(record[lengthColumn]))
		if !ok || prefixErr != nil || lengthErr != nil {
			return errors.New("RPKI CSV payload has an invalid ASN, prefix or length")
		}
		if err := into.add(asn, prefix, maxLength); err != nil {
			return err
		}
		rows++
	}
	if rows == 0 {
		return errors.New("RPKI CSV snapshot has no payloads")
	}
	return nil
}

func loadRpkiAuthorizations(ctx context.Context, catalogPath string, sources []rpkiSource) (*rpkiAuthorizations, error) {
	if len(sources) == 0 {
		return nil, nil
	}
	root, err := os.OpenRoot(filepath.Dir(catalogPath))
	if err != nil {
		return nil, err
	}
	defer root.Close()
	authorizations := &rpkiAuthorizations{}
	for _, source := range sources {
		f, err := root.Open(source.File)
		if err != nil {
			return nil, err
		}
		parseErr := func() error {
			reader, err := openBoundedEvidenceReader(f, maxRpkiDecompressedBytes)
			if err != nil {
				return err
			}
			switch source.Format {
			case rpkiFormatClientJson:
				return readRpkiClientJson(ctx, reader, authorizations)
			case rpkiFormatRoutinatorCsv:
				return readRoutinatorCsv(ctx, reader, authorizations)
			default:
				return errors.New("unsupported RPKI snapshot format")
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
	if authorizations.count == 0 {
		return nil, errors.New("RPKI snapshots contain no payloads")
	}
	return authorizations, nil
}

// RFC 6811 origin validation for one observed prefix/origin pair: invalid when
// some payload covers the prefix but none matches both the origin and length.
func (self *rpkiAuthorizations) validity(prefix netip.Prefix, asn uint32) string {
	if self == nil {
		return "unchecked"
	}
	covered := false
	for bits := prefix.Bits(); bits >= 0; bits-- {
		candidate := netip.PrefixFrom(prefix.Addr(), bits).Masked()
		for _, authorization := range self.byPrefix[candidate] {
			covered = true
			if authorization.asn == asn && prefix.Bits() <= authorization.maxLength {
				return "valid"
			}
		}
	}
	if covered {
		return "invalid"
	}
	return "not-found"
}

// A route is invalid if any of its origins is invalid. Equal-origin aggregates
// do not lend validity to a more-specific beyond its own authorized length.
func (self *rpkiAuthorizations) routeValidity(prefix netip.Prefix, route subscriberOriginRoute) string {
	if self == nil {
		return "unchecked"
	}
	states := []string{}
	for _, origin := range route.origins {
		states = append(states, self.validity(prefix, origin.asn))
	}
	if slices.Contains(states, "invalid") {
		return "invalid"
	}
	if slices.Contains(states, "valid") && !slices.Contains(states, "not-found") {
		return "valid"
	}
	return "not-found"
}
